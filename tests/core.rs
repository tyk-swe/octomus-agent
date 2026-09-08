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
fn repair_routes_are_backward_compatible_and_validated() {
    let mut old = serde_json::to_value(Config::default()).unwrap();
    old.as_object_mut().unwrap().remove("repair_route");
    let mut config: Config = serde_json::from_value(old).unwrap();
    assert_eq!(config.repair_route, Route::new("gpt-6-astra", "medium"));
    for route in config.roles.values_mut().chain(config.tiers.values_mut()) {
        *route = Route::new("available", "low");
    }
    let catalog =
        vec![json!({"model":"available","supportedReasoningEfforts":[{"reasoningEffort":"low"}]})];
    let error = octomus_agent::codex::validate_routes(&config, &catalog).unwrap_err();
    assert!(error.to_string().contains("gpt-6-astra / medium"));
    config.repair_route = Route::new("available", "high");
    assert!(
        octomus_agent::codex::validate_routes(&config, &catalog)
            .unwrap_err()
            .to_string()
            .contains("available / high")
    );
    config.repair_route.effort = "low".into();
    octomus_agent::codex::validate_routes(&config, &catalog).unwrap();
    let saved: Config = serde_json::from_str(&serde_json::to_string(&config).unwrap()).unwrap();
    assert_eq!(saved.repair_route, config.repair_route);
    config.repair_route.model = "x".repeat(101);
    assert!(config.validate(false).is_err());
}

#[test]
fn codex_version_diagnostics_do_not_accept_prefix_matches() {
    use octomus_agent::codex::version_warning;
    assert!(version_warning("codex-cli 0.153.4\n").is_none());
    for version in ["codex-cli 0.153.40", "codex-cli 0.153.4-dev", "unknown"] {
        assert!(version_warning(version).unwrap().contains("mismatch"));
    }
}
#[test]
fn daily_admission_budget_is_atomic_under_concurrency() {
    let temp = tempfile::tempdir().unwrap();
    let store = Arc::new(Store::open(&temp.path().join("state.db")).unwrap());
    let handles: Vec<_> = (0..20)
        .map(|_| {
            let store = store.clone();
            std::thread::spawn(move || {
                store
                    .reserve_session(
                        5,
                        &octomus_agent::store::Admission::new(
                            "cycle",
                            None,
                            "grounding",
                            &Route::new("fixture", "low"),
                        ),
                    )
                    .is_ok()
            })
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
    let report = octomus_agent::report::usage_report(&temp.path().join("state.db")).unwrap();
    assert_eq!(report["admissions"].as_array().unwrap().len(), 5);
    assert_eq!(report["daily"][0]["unattributed_admissions"], 0);
}
#[tokio::test]
async fn private_api_enforces_auth_content_type_and_configuration_rules() {
    let temp = tempfile::tempdir().unwrap();
    let store = Store::open(&temp.path().join("state.db")).unwrap();
    let app = App::new(store, temp.path().into());
    let router = api::router(app.clone(), TOKEN, Some(temp.path().into()));
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

#[test]
fn audit_readiness_requires_only_planning_routes_and_no_verification() {
    let temp = tempfile::tempdir().unwrap();
    std::fs::create_dir(temp.path().join(".git")).unwrap();
    let mut c = Config {
        repository: temp.path().into(),
        github_repo: "fixture/project".into(),
        ..Config::default()
    };
    for role in ["orchestrator", "discovery", "proposal_reviewer"] {
        c.roles.insert(role.into(), Route::new("available", "low"));
    }
    let catalog =
        vec![json!({"model":"available","supportedReasoningEfforts":[{"reasoningEffort":"low"}]})];
    c.validate_audit().unwrap();
    assert!(c.validate(true).is_err());
    octomus_agent::codex::validate_routes_for(&c, &catalog, true).unwrap();
    assert!(octomus_agent::codex::validate_routes(&c, &catalog).is_err());
    c.roles.get_mut("discovery").unwrap().effort = "max".into();
    assert!(octomus_agent::codex::validate_routes_for(&c, &catalog, true).is_err());
    c.roles.get_mut("discovery").unwrap().model.clear();
    assert!(c.validate_audit().is_err());
}

#[tokio::test]
async fn embedded_dashboard_and_overrides_preserve_http_boundaries() {
    let temp = tempfile::tempdir().unwrap();
    let app = App::new(
        Store::open(&temp.path().join("state.db")).unwrap(),
        temp.path().into(),
    );
    let router = api::router(app.clone(), TOKEN, None);
    for (uri, status, mime) in [
        ("/", StatusCode::OK, "text/html"),
        ("/proposals", StatusCode::OK, "text/html"),
        ("/favicon.svg", StatusCode::OK, "image/svg+xml"),
        ("/_app/missing.js", StatusCode::NOT_FOUND, ""),
        ("/%2e%2e/Cargo.toml", StatusCode::BAD_REQUEST, ""),
        ("/api/missing", StatusCode::NOT_FOUND, "application/json"),
    ] {
        let response = router
            .clone()
            .oneshot(Request::builder().uri(uri).body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(response.status(), status, "{uri}");
        assert_eq!(response.headers()["x-content-type-options"], "nosniff");
        if !mime.is_empty() {
            assert!(
                response.headers()["content-type"]
                    .to_str()
                    .unwrap()
                    .starts_with(mime)
            );
        }
    }
    let head = router
        .clone()
        .oneshot(
            Request::builder()
                .method("HEAD")
                .uri("/")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(head.status(), StatusCode::OK);
    assert!(to_bytes(head.into_body(), 100000).await.unwrap().is_empty());
    std::fs::write(temp.path().join("200.html"), "override dashboard").unwrap();
    let response = api::router(app, TOKEN, Some(temp.path().into()))
        .oneshot(Request::builder().uri("/").body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(
        to_bytes(response.into_body(), 100000).await.unwrap(),
        "override dashboard"
    );
}

#[tokio::test]
async fn valid_authentication_bypasses_pending_failure_delay_and_audit_controls_conflict() {
    let temp = tempfile::tempdir().unwrap();
    let app = App::new(
        Store::open(&temp.path().join("state.db")).unwrap(),
        temp.path().into(),
    );
    let router = api::router(app.clone(), TOKEN, None);
    let bad = tokio::spawn(
        router.clone().oneshot(
            Request::builder()
                .uri("/api/state")
                .body(Body::empty())
                .unwrap(),
        ),
    );
    tokio::task::yield_now().await;
    let response = router
        .clone()
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
    assert!(
        !bad.is_finished(),
        "Valid tokens must not wait on an unauthenticated request"
    );
    assert_eq!(
        bad.await.unwrap().unwrap().status(),
        StatusCode::UNAUTHORIZED
    );
    app.runtime.lock().unwrap().cycle = Some(CancellationToken::new());
    app.runtime.lock().unwrap().cycle_mode = Some(octomus_agent::model::CycleMode::Audit);
    for action in ["audit", "cycle", "resume"] {
        let response = router
            .clone()
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri(format!("/api/control/{action}"))
                    .header("authorization", format!("Bearer {TOKEN}"))
                    .header("content-type", "application/json")
                    .body(Body::from("{}"))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::CONFLICT);
    }
}

#[test]
fn legacy_cycles_default_to_execution_without_rewriting_evidence() {
    use octomus_agent::model::{Cycle, CycleMode};
    let old = json!({"id":"legacy","number":7,"status":"completed","started_at":"2026-09-08T00:00:00Z","completed_at":"2026-09-08T00:05:00Z","grounding":null,"proposals":[],"assessments":[],"sessions":[],"error":null});
    let cycle: Cycle = serde_json::from_value(old.clone()).unwrap();
    assert_eq!(cycle.mode, CycleMode::Execution);
    let mut upgraded = serde_json::to_value(cycle).unwrap();
    assert_eq!(
        upgraded.as_object_mut().unwrap().remove("mode"),
        Some(json!("execution"))
    );
    assert_eq!(upgraded, old);
}
