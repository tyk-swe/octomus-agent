use axum::{
    body::{Body, to_bytes},
    http::{Request, StatusCode},
};
use octomus_agent::{
    api,
    config::{Config, Route},
    engine::{App, validate_proposals},
    model::{Grounding, Proposal, PullRequest},
    process,
    store::Store,
};
use serde_json::{Value, json};
use std::{collections::BTreeMap, sync::Arc, time::Duration};
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

const TOKEN: &str = "operator-fixture-token-with-at-least-32-characters";
fn proposal(id: &str) -> Proposal {
    Proposal {
        id: id.into(),
        title: format!("Improve {id}"),
        problem: "Missing documented capability".into(),
        evidence: vec!["src/main.rs:1".into()],
        benefit: "Completes documented behavior".into(),
        category: "features".into(),
        target: "main".into(),
        tier: "M".into(),
        scope: "One cohesive feature".into(),
        dependencies: vec![],
        prompt: "Implement and verify the documented capability".into(),
        decision: "accepted".into(),
        reason: "Both adversaries accepted; no duplicate work".into(),
    }
}
fn grounding() -> Grounding {
    Grounding {
        revision: "a".repeat(40),
        prs: vec![PullRequest {
            number: 1,
            title: "Existing".into(),
            branch: "octomus/existing".into(),
            head: "b".repeat(40),
            base: "main".into(),
            url: "https://github.com/fixture/project/pull/1".into(),
            body: "<!-- octomus:task:existing -->".into(),
            state: "open".into(),
            changed_lines: 2000,
            created_at: "2026-01-01T00:00:00Z".into(),
            owned: true,
        }],
        history: json!([]),
        maintenance_due: true,
        maintenance_targets: vec!["octomus/existing".into()],
    }
}

#[test]
fn proposal_dependencies_require_delivered_code_and_no_cycles() {
    let c = Config::default();
    let g = grounding();
    let mut a = proposal("a");
    let mut b = proposal("b");
    b.dependencies.push(a.id.clone());
    assert!(
        validate_proposals(&c, &[a.clone(), b.clone()], &g, &[]).is_err(),
        "Separate default-branch PRs cannot pretend dependencies are already merged"
    );
    a.target = "octomus/existing".into();
    b.target = a.target.clone();
    validate_proposals(&c, &[a.clone(), b.clone()], &g, &[]).unwrap();
    a.dependencies.push(b.id.clone());
    assert!(validate_proposals(&c, &[a, b], &g, &[]).is_err());
}
#[test]
fn rejected_and_unowned_work_is_never_executable() {
    let c = Config::default();
    let mut p = proposal("a");
    p.target = "someone-elses/work".into();
    assert!(validate_proposals(&c, &[p.clone()], &grounding(), &[]).is_err());
    p.decision = "rejected".into();
    validate_proposals(&c, &[p], &grounding(), &[]).unwrap();
    let p = proposal("same");
    assert!(validate_proposals(&c, &[p.clone(), p], &grounding(), &[]).is_err());
}
#[test]
fn unsupported_effort_never_falls_back() {
    let c = Config {
        roles: BTreeMap::from_iter(
            [
                "orchestrator",
                "discovery",
                "proposal_reviewer",
                "code_reviewer",
            ]
            .map(|role| (role.into(), Route::new("gpt-6-astra", "medium"))),
        ),
        ..Config::default()
    };
    let models = vec![
        json!({"model":"gpt-6-astra","supportedReasoningEfforts":[{"reasoningEffort":"medium"},{"reasoningEffort":"low"},{"reasoningEffort":"high"}]}),
        json!({"model":"gpt-5.6-luna","supportedReasoningEfforts":[{"reasoningEffort":"xhigh"}]}),
    ];
    let error = octomus_agent::codex::validate_routes(&c, &models).unwrap_err();
    assert!(error.to_string().contains("max"));
    assert_eq!(c.tiers["S"].effort, "max");
}
#[test]
fn daily_admission_budget_is_atomic_under_concurrency() {
    let temp = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open(&temp.path().join("state.db")).unwrap());
    let handles: Vec<_> = (0..20)
        .map(|_| {
            let store = store.clone();
            std::thread::spawn(move || store.reserve_session(5).is_ok())
        })
        .collect();
    assert_eq!(
        handles
            .into_iter()
            .map(|h| h.join().unwrap())
            .filter(|success| *success)
            .count(),
        5
    );
    assert_eq!(store.sessions_today().unwrap(), 5);
}
#[tokio::test]
async fn private_api_enforces_auth_content_type_and_configuration_rules() {
    let temp = tempfile::tempdir().unwrap();
    let store = Store::open(&temp.path().join("state.db")).unwrap();
    let app = App::new(store, temp.path().into());
    let router = api::router(app.clone(), TOKEN, temp.path().into());
    let response = router
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/state")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    let response = router
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/control/resume")
                .method("POST")
                .header("authorization", format!("Bearer {TOKEN}"))
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::UNSUPPORTED_MEDIA_TYPE);
    let response = router
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/control/resume")
                .method("POST")
                .header("authorization", format!("Bearer {TOKEN}"))
                .header("content-type", "application/json")
                .body(Body::from("{}"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert!(app.control().unwrap().paused);
    let response = router
        .oneshot(
            Request::builder()
                .uri("/api/state")
                .header("authorization", format!("Bearer {TOKEN}"))
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let value: Value =
        serde_json::from_slice(&to_bytes(response.into_body(), 100000).await.unwrap()).unwrap();
    assert_eq!(value["status"], "paused");
    assert_eq!(value["configured"], false);
}
#[tokio::test]
async fn cancellation_kills_the_command_process_group() {
    let temp = tempfile::tempdir().unwrap();
    let cancel = CancellationToken::new();
    let token = cancel.clone();
    let cwd = temp.path().to_owned();
    let work = tokio::spawn(async move {
        process::run(
            "bash",
            &["-c", "sleep 30 & echo $! > child.pid; wait"],
            &cwd,
            10,
            &token,
        )
        .await
    });
    for _ in 0..100 {
        if temp.path().join("child.pid").exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    let pid = std::fs::read_to_string(temp.path().join("child.pid"))
        .unwrap()
        .trim()
        .to_owned();
    cancel.cancel();
    assert!(work.await.unwrap().is_err());
    for _ in 0..100 {
        let stat = std::fs::read_to_string(format!("/proc/{pid}/stat"));
        if stat
            .as_ref()
            .map_or(true, |s| s.split_whitespace().nth(2) == Some("Z"))
        {
            return;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("Child process survived cancellation");
}
