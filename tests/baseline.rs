use axum::{
    body::Body,
    http::{Request, StatusCode},
};
use octomus_agent::{
    api,
    config::Config,
    engine::{
        App,
        baseline::{baseline_fingerprint, bounded_output, command_output},
    },
    model::{BaselineCheck, BaselineStatus, DefaultBranchObservation, Task, now},
    process::{self, CaptureMode},
    store::{Store, redact},
};
use serde_json::{Value, json};
use std::{path::Path, time::Duration};
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

mod common;
use common::*;

fn git(cwd: &Path, args: &[&str]) -> String {
    let output = std::process::Command::new("git")
        .args(args)
        .current_dir(cwd)
        .output()
        .unwrap();
    assert!(output.status.success(), "git {args:?} failed");
    String::from_utf8_lossy(&output.stdout).trim().to_owned()
}

fn baseline_app() -> (tempfile::TempDir, App, Config) {
    let tmp = tempfile::tempdir().unwrap();
    let repo = tmp.path().join("repo");
    std::fs::create_dir_all(&repo).unwrap();
    git(&repo, &["init", "-b", "main"]);
    git(&repo, &["config", "user.name", "Fixture"]);
    git(&repo, &["config", "user.email", "fixture@example.com"]);
    std::fs::write(repo.join("README.md"), "fixture\n").unwrap();
    git(&repo, &["add", "."]);
    git(&repo, &["commit", "-m", "initial"]);
    let config = Config {
        repository: repo,
        github_repo: "fixture/project".into(),
        verification_commands: vec!["true".into()],
        ..Default::default()
    };
    let data = tmp.path().join("data");
    std::fs::create_dir_all(&data).unwrap();
    let store = Store::open(&data.join("state.db")).unwrap();
    store.put("settings", "config", &config).unwrap();
    let app = App::new(store, data);
    (tmp, app, config)
}

async fn start_check(router: &axum::Router, expected: &Config) -> (StatusCode, Value) {
    request(
        router,
        "POST",
        "/api/baseline-checks",
        Body::from(serde_json::to_vec(&json!({"expected_config": expected})).unwrap()),
    )
    .await
}

async fn wait_terminal(app: &App, id: &str) -> BaselineCheck {
    // The runtime slot frees first; the stored record turns terminal after it, so
    // only the second wait's outcome is asserted.
    let _ = common::wait_until(Duration::from_secs(4), || app.runtime().baseline.is_none()).await;
    assert!(
        common::wait_until(Duration::from_secs(4), || {
            let check: BaselineCheck = app.store.get("baseline", id).unwrap().unwrap();
            check.status != BaselineStatus::Running
        })
        .await,
        "baseline check did not finish"
    );
    let check: BaselineCheck = app.store.get("baseline", id).unwrap().unwrap();
    check
}

#[test]
fn baseline_validation_accepts_unrouted_models_but_requires_repository_and_commands() {
    let (_tmp, _app, config) = baseline_app();
    config.validate(true).unwrap_err();
    config.validate_baseline().unwrap();
    let mut no_commands = config.clone();
    no_commands.verification_commands = vec![];
    no_commands.validate_baseline().unwrap_err();
    let mut no_repo = config.clone();
    no_repo.repository = "relative/path".into();
    no_repo.validate_baseline().unwrap_err();
    let mut no_github = config.clone();
    no_github.github_repo = "not-an-owner/name/pair".into();
    no_github.validate_baseline().unwrap_err();
    config.validate_audit().unwrap_err();
}

#[test]
fn baseline_fingerprint_tracks_the_canonical_config() {
    let (_tmp, _app, config) = baseline_app();
    let fingerprint = baseline_fingerprint(&config).unwrap();
    assert_eq!(fingerprint.len(), 64);
    assert_eq!(baseline_fingerprint(&config).unwrap(), fingerprint);
    let mut changed = config.clone();
    changed.verification_commands.push("echo ok".into());
    assert_ne!(baseline_fingerprint(&changed).unwrap(), fingerprint);
}

#[test]
fn baseline_output_bounds_are_utf8_safe() {
    let marker = "\n[output truncated]";
    let (output, truncated) = bounded_output("short", 16 * 1024, false);
    assert!(!truncated && output == "short");
    let (output, truncated) = bounded_output("anything", 0, false);
    assert!(truncated && output.is_empty());
    let (output, truncated) = bounded_output("", 0, false);
    assert!(!truncated && output.is_empty());
    let (output, truncated) = bounded_output("anything", 0, true);
    assert!(truncated && output.is_empty());
    let (output, truncated) = bounded_output(&"x".repeat(100), 1, false);
    assert!(truncated && output.len() == 1);
    for limit in [marker.len() - 1, marker.len()] {
        let (output, truncated) = bounded_output(&"x".repeat(100 * 1024), limit, false);
        assert!(truncated && output.len() <= limit, "limit {limit}");
    }
    let long = "x".repeat(20 * 1024);
    let (output, truncated) = bounded_output(&long, 16 * 1024, false);
    assert!(truncated && output.len() <= 16 * 1024 && output.ends_with("[output truncated]"));
    let wide = "𐐀".repeat(16 * 1024);
    let (output, truncated) = bounded_output(&wide, 16 * 1024, false);
    assert!(truncated && output.len() <= 16 * 1024);
    let (output, truncated) = bounded_output("small", 16 * 1024, true);
    assert!(truncated && output.ends_with("[output truncated]"));
    let mut remaining = 1024 * 1024;
    let mut total = 0;
    for _ in 0..100 {
        let (output, truncated) = bounded_output(&long, remaining.min(16 * 1024), false);
        assert!(truncated || output.is_empty());
        assert!(output.len() <= remaining.min(16 * 1024));
        remaining = remaining.saturating_sub(output.len());
        total += output.len();
    }
    assert!(total <= 1024 * 1024 && remaining == 0);
}

#[tokio::test]
async fn baseline_command_output_preserves_real_capture_truncation() {
    let tmp = tempfile::tempdir().unwrap();
    let cancel = CancellationToken::new();
    let captured = process::capture(
        "bash",
        &["-c", "yes '𐐀' | head -c 20000"],
        tmp.path(),
        10,
        &cancel,
        CaptureMode::Diagnostic,
    )
    .await;
    let (text, diagnostic_truncated, success) = command_output(&captured);
    assert!(success && !diagnostic_truncated);
    let (output, output_truncated) =
        bounded_output(&redact(&text), 16 * 1024, diagnostic_truncated);
    assert!(output_truncated && output.len() <= 16 * 1024);
    let captured = process::capture(
        "bash",
        &["-c", "yes 'x' | head -c 300000; exit 3"],
        tmp.path(),
        10,
        &cancel,
        CaptureMode::Diagnostic,
    )
    .await;
    let (text, diagnostic_truncated, success) = command_output(&captured);
    assert!(!success && diagnostic_truncated && text.contains("exit status: 3"));
    let (output, output_truncated) =
        bounded_output(&redact(&text), 16 * 1024, diagnostic_truncated);
    assert!(output_truncated && output.len() <= 16 * 1024);
    let captured = process::capture(
        "bash",
        &["-c", "echo out; echo err >&2; exit 1"],
        tmp.path(),
        10,
        &cancel,
        CaptureMode::Diagnostic,
    )
    .await;
    let (text, diagnostic_truncated, success) = command_output(&captured);
    assert!(!success && !diagnostic_truncated);
    assert!(text.contains("out") && text.contains("[stderr]") && text.contains("err"));
}

#[tokio::test]
async fn baseline_observation_never_regresses_to_an_older_revision() {
    let (_tmp, app, config) = baseline_app();
    let older = (chrono::Utc::now() - chrono::Duration::seconds(30)).to_rfc3339();
    let newer = chrono::Utc::now().to_rfc3339();
    app.observe_default_branch(&config, "a".repeat(40).as_str(), &newer)
        .await
        .unwrap();
    app.observe_default_branch(&config, "b".repeat(40).as_str(), &older)
        .await
        .unwrap();
    let observation = app.runtime().default_observation.clone().unwrap();
    assert_eq!(observation.revision, "a".repeat(40));
    let mut other = config.clone();
    other.default_branch = "other".into();
    assert!(
        app.observe_default_branch(&other, "c".repeat(40).as_str(), &newer)
            .await
            .is_err()
    );
    let observation = app.runtime().default_observation.clone().unwrap();
    assert_eq!(observation.revision, "a".repeat(40));
}

#[tokio::test]
async fn baseline_view_reports_config_match_and_revision_staleness_separately() {
    let (_tmp, app, config) = baseline_app();
    let check = BaselineCheck {
        id: octomus_agent::model::id(),
        status: BaselineStatus::Passed,
        config: config.clone(),
        config_fingerprint: baseline_fingerprint(&config).unwrap(),
        revision: Some("a".repeat(40)),
        started_at: now(),
        completed_at: Some(now()),
        commands: vec![],
        error: None,
        workspace_removed: true,
        cleanup_error: None,
    };
    app.store.put("baseline", &check.id, &check).unwrap();
    app.store
        .put("settings", "baseline_latest", &check.id)
        .unwrap();
    let view = app.baseline_view(None).unwrap();
    assert_eq!(view["check"]["id"], check.id);
    assert_eq!(view["config_matches"], true);
    assert_eq!(view["revision_status"], "unknown");
    assert!(view["default_observation"].is_null());
    app.runtime().default_observation = Some(DefaultBranchObservation {
        repository: config.github_repo.clone(),
        default_branch: config.default_branch.clone(),
        revision: "a".repeat(40),
        observed_at: now(),
    });
    assert_eq!(
        app.baseline_view(None).unwrap()["revision_status"],
        "matches_last_observation"
    );
    app.runtime().default_observation.as_mut().unwrap().revision = "b".repeat(40);
    assert_eq!(app.baseline_view(None).unwrap()["revision_status"], "stale");
    app.runtime()
        .default_observation
        .as_mut()
        .unwrap()
        .observed_at = (chrono::Utc::now() + chrono::Duration::hours(1)).to_rfc3339();
    assert_eq!(
        app.baseline_view(None).unwrap()["revision_status"],
        "unknown"
    );
    app.runtime()
        .default_observation
        .as_mut()
        .unwrap()
        .observed_at = (chrono::Utc::now() - chrono::Duration::hours(1)).to_rfc3339();
    assert_eq!(
        app.baseline_view(None).unwrap()["revision_status"],
        "unknown"
    );
    app.runtime()
        .default_observation
        .as_mut()
        .unwrap()
        .observed_at = now();
    app.runtime()
        .default_observation
        .as_mut()
        .unwrap()
        .default_branch = "other".into();
    assert_eq!(
        app.baseline_view(None).unwrap()["revision_status"],
        "unknown"
    );
    app.runtime()
        .default_observation
        .as_mut()
        .unwrap()
        .default_branch = config.default_branch.clone();
    assert_eq!(app.baseline_view(None).unwrap()["revision_status"], "stale");
    let mut changed = config.clone();
    changed.default_branch = "moved".into();
    app.store.put("settings", "config", &changed).unwrap();
    let view = app.baseline_view(None).unwrap();
    assert_eq!(view["config_matches"], false);
    assert_eq!(view["revision_status"], "unknown");
    let mut no_commands = changed.clone();
    no_commands.verification_commands = vec![];
    app.store.put("settings", "config", &no_commands).unwrap();
    let view = app.baseline_view(None).unwrap();
    assert_eq!(view["eligible"], false);
    assert!(view["reason"].as_str().unwrap().contains("verification"));
}

#[tokio::test]
async fn baseline_cleanup_removes_the_owned_clone_and_refuses_symlinks() {
    let (tmp, app, config) = baseline_app();
    let mut check = BaselineCheck {
        id: octomus_agent::model::id(),
        status: BaselineStatus::Failed,
        config,
        config_fingerprint: String::new(),
        revision: None,
        started_at: now(),
        completed_at: Some(now()),
        commands: vec![],
        error: None,
        workspace_removed: false,
        cleanup_error: None,
    };
    let workspace = tmp
        .path()
        .join("data/baselines")
        .join(&check.id)
        .join("workspace");
    std::fs::create_dir_all(&workspace).unwrap();
    std::fs::write(workspace.join("artifact"), "data").unwrap();
    app.cleanup_baseline(&mut check).await.unwrap();
    assert!(check.workspace_removed && check.cleanup_error.is_none());
    assert!(!tmp.path().join("data/baselines").join(&check.id).exists());
    let saved: BaselineCheck = app.store.get("baseline", &check.id).unwrap().unwrap();
    assert!(saved.workspace_removed);
    let outside = tmp.path().join("outside");
    std::fs::create_dir_all(&outside).unwrap();
    std::fs::write(outside.join("keep"), "data").unwrap();
    let mut bad = BaselineCheck {
        id: octomus_agent::model::id(),
        ..check.clone()
    };
    bad.workspace_removed = false;
    std::os::unix::fs::symlink(&outside, tmp.path().join("data/baselines").join(&bad.id)).unwrap();
    app.cleanup_baseline(&mut bad).await.unwrap();
    assert!(!bad.workspace_removed && bad.cleanup_error.is_some());
    assert!(outside.join("keep").exists());
    let mut invalid = BaselineCheck {
        id: "../etc".into(),
        ..check.clone()
    };
    invalid.workspace_removed = false;
    assert!(app.cleanup_baseline(&mut invalid).await.is_err());
}

#[test]
fn recover_baselines_finalizes_running_records_and_preserves_cancel_intent() {
    let (_tmp, app, config) = baseline_app();
    let make = |status| BaselineCheck {
        id: octomus_agent::model::id(),
        status,
        config: config.clone(),
        config_fingerprint: String::new(),
        revision: None,
        started_at: now(),
        completed_at: None,
        commands: vec![],
        error: None,
        workspace_removed: false,
        cleanup_error: None,
    };
    let interrupted = make(BaselineStatus::Running);
    let cancelled = make(BaselineStatus::Running);
    let finished = make(BaselineStatus::Passed);
    for check in [&interrupted, &cancelled, &finished] {
        app.store.put("baseline", &check.id, check).unwrap();
    }
    app.store
        .put("baseline_cancel", &cancelled.id, &json!(now()))
        .unwrap();
    app.recover_baselines().unwrap();
    let interrupted: BaselineCheck = app.store.get("baseline", &interrupted.id).unwrap().unwrap();
    assert_eq!(interrupted.status, BaselineStatus::Interrupted);
    assert!(interrupted.completed_at.is_some() && interrupted.error.is_some());
    let cancelled: BaselineCheck = app.store.get("baseline", &cancelled.id).unwrap().unwrap();
    assert_eq!(cancelled.status, BaselineStatus::Cancelled);
    let finished: BaselineCheck = app.store.get("baseline", &finished.id).unwrap().unwrap();
    assert_eq!(finished.status, BaselineStatus::Passed);
    let candidates = app.store.baseline_cleanup_candidates().unwrap();
    assert_eq!(candidates.len(), 3);
    assert!(app.store.running_baselines().unwrap().is_empty());
}

#[tokio::test]
async fn baseline_api_authentication_routes_and_missing_records() {
    let (_tmp, app, _config) = baseline_app();
    let router = api::router(app.clone(), TOKEN, None);
    let unauthenticated = router
        .clone()
        .oneshot(
            Request::builder()
                .uri("/api/baseline-checks/latest")
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(unauthenticated.status(), StatusCode::UNAUTHORIZED);
    let (status, view) =
        request(&router, "GET", "/api/baseline-checks/latest", Body::empty()).await;
    assert_eq!(status, StatusCode::OK);
    assert!(view["check"].is_null() && view["eligible"] == true);
    let (status, _) = request(
        &router,
        "GET",
        "/api/baseline-checks/no-such-check",
        Body::empty(),
    )
    .await;
    assert_eq!(status, StatusCode::NOT_FOUND);
    let (status, _) = request(
        &router,
        "POST",
        "/api/baseline-checks/no-such-check/cancel",
        Body::from("{}"),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
    let (status, _) = request(
        &router,
        "POST",
        "/api/baseline-checks/latest",
        Body::from("{}"),
    )
    .await;
    assert_eq!(status, StatusCode::METHOD_NOT_ALLOWED);
}

#[tokio::test]
async fn baseline_start_conflicts_and_gate_blocks_cover_the_live_slot() {
    let (_tmp, app, config) = baseline_app();
    let router = api::router(app.clone(), TOKEN, None);
    let mut invalid = config.clone();
    invalid.repository = "relative".into();
    app.store.put("settings", "config", &invalid).unwrap();
    let (status, _) = start_check(&router, &invalid).await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    let (_, view) = request(&router, "GET", "/api/baseline-checks/latest", Body::empty()).await;
    assert_eq!(view["eligible"], false);
    assert!(!view["reason"].is_null());
    app.store.put("settings", "config", &config).unwrap();
    let mut stale = config.clone();
    stale.verification_commands = vec!["false".into()];
    let (status, _) = start_check(&router, &stale).await;
    assert_eq!(status, StatusCode::CONFLICT);
    assert!(app.store.latest_baseline().unwrap().is_none());
    let (status, check) = start_check(&router, &config).await;
    assert_eq!(status, StatusCode::ACCEPTED);
    assert_eq!(check["status"], "running");
    let id = check["id"].as_str().unwrap().to_owned();
    let (status, _) = start_check(&router, &config).await;
    assert_eq!(status, StatusCode::CONFLICT);
    for path in [
        "/api/control/cycle",
        "/api/control/resume",
        "/api/control/audit",
    ] {
        let (status, _) = request(&router, "POST", path, Body::from("{}")).await;
        assert_eq!(
            status,
            StatusCode::CONFLICT,
            "{path} must conflict during a baseline"
        );
    }
    let (status, _) = request(
        &router,
        "PUT",
        "/api/config",
        Body::from(serde_json::to_vec(&config).unwrap()),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
    let task: Task = serde_json::from_value(json!({
        "id":octomus_agent::model::id(),"cycle_id":"cycle","proposal":{"id":"a","title":"T","problem":"P","benefit":"B","scope":"S","evidence":[],"category":"features","target":"main","tier":"M","dependencies":[],"prompt":"Do it","decision":"accepted","reason":"R","problem_key":"","relevant_paths":[],"reconsiders":[]},
        "status":"blocked","blocked_reason":"publication_uncertain","route":{"backend":"codex","model":"m","effort":"low"},"config":config,
        "source_revision":"s","comparison_base":"s","default_revision":"s","branch":"octomus/work","workspace":"","sessions":[],"reviews":[],"verification":[],"output_commit":"o","attempts":0,"created_at":now(),"updated_at":now()
    }))
    .unwrap();
    app.store.put("task", &task.id, &task).unwrap();
    let (status, _) = request(
        &router,
        "POST",
        &format!("/api/tasks/{}/reconcile", task.id),
        Body::from("{}"),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
    let (status, _) = request(&router, "POST", "/api/control/pause", Body::from("{}")).await;
    assert_eq!(status, StatusCode::OK);
    let (status, state) = request(&router, "GET", "/api/state", Body::empty()).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(state["baseline_active"], true);
    assert_eq!(state["baseline"]["id"], id);
    assert_eq!(state["baseline"]["config_matches"], true);
    assert!(state["baseline"].get("commands").is_none());
    let check = wait_terminal(&app, &id).await;
    assert_eq!(check.status, BaselineStatus::Failed);
    assert!(check.completed_at.is_some() && check.error.is_some());
    assert!(check.workspace_removed);
    let (_, view) = request(&router, "GET", "/api/baseline-checks/latest", Body::empty()).await;
    assert_eq!(view["check"]["id"], id);
    assert_eq!(view["eligible"], true);
    assert_eq!(app.store.sessions_today().unwrap(), 0);
    assert!(app.store.running_cycles().unwrap().is_empty());
    let tasks = app
        .store
        .history_page("task", &octomus_agent::store::HistoryQuery::default())
        .unwrap();
    assert_eq!(tasks.items.len(), 1, "only the seeded task exists");
    let (status, _) = request(
        &router,
        "POST",
        &format!("/api/baseline-checks/{id}/cancel"),
        Body::from("{}"),
    )
    .await;
    assert_eq!(status, StatusCode::CONFLICT);
}
