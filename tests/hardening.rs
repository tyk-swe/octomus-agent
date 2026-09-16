use octomus_agent::{
    config::{Config, Route},
    engine::{App, idle_delay, validate_proposals},
    git,
    model::*,
    process,
    store::{Admission, HistoryQuery, Store},
};
use serde_json::{Value, json};
use std::{os::unix::fs::PermissionsExt, time::Duration};
use tokio_util::sync::CancellationToken;
use tower::ServiceExt;

const CONTROL_TOKEN: &str = "task-control-fixture-token-at-least-32-characters";
fn control_request(path: &str) -> axum::http::Request<axum::body::Body> {
    axum::http::Request::builder()
        .uri(format!("/api/{path}"))
        .method("POST")
        .header("authorization", format!("Bearer {CONTROL_TOKEN}"))
        .header("content-type", "application/json")
        .body(axum::body::Body::empty())
        .unwrap()
}

async fn held_preflight_fixture() -> (tempfile::TempDir, App, Task, axum::Router) {
    let temp = tempfile::tempdir().unwrap();
    let store = Store::open(&temp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), temp.path().into());
    let repository = temp.path().join("repository");
    std::fs::create_dir(&repository).unwrap();
    let config = Config {
        repository: repository.clone(),
        command_timeout_seconds: 10,
        ..Default::default()
    };
    let cancel = CancellationToken::new();
    for args in [
        vec!["init", "-b", "main"],
        vec![
            "-c",
            "user.name=Fixture",
            "-c",
            "user.email=fixture@example.com",
            "commit",
            "--allow-empty",
            "-m",
            "Fixture",
        ],
        vec!["remote", "add", "origin", "."],
    ] {
        git::git(&config, &repository, &args, &cancel)
            .await
            .unwrap();
    }
    let revision = git::git(&config, &repository, &["rev-parse", "HEAD"], &cancel)
        .await
        .unwrap();
    let upload_pack = temp.path().join("held-upload-pack");
    std::fs::write(&upload_pack, "#!/bin/sh\nfixture_dir=$(dirname \"$0\")\ntouch \"$fixture_dir/entered-$$\"\nwhile [ -e \"$fixture_dir/hold\" ]; do sleep 0.02; done\nif [ -e \"$fixture_dir/fail\" ]; then exit 1; fi\nexec git-upload-pack \"$@\"\n").unwrap();
    std::fs::set_permissions(&upload_pack, std::fs::Permissions::from_mode(0o755)).unwrap();
    git::git(
        &config,
        &repository,
        &[
            "config",
            "remote.origin.uploadpack",
            upload_pack.to_str().unwrap(),
        ],
        &cancel,
    )
    .await
    .unwrap();
    std::fs::write(temp.path().join("hold"), "").unwrap();
    store.put("settings", "config", &config).unwrap();
    let mut t = task();
    t.config = config;
    t.source_revision = revision.clone();
    t.default_revision = revision;
    t.status = Status::Blocked;
    store.put("task", &t.id, &t).unwrap();
    let router = octomus_agent::api::router(app.clone(), CONTROL_TOKEN, Some(temp.path().into()));
    (temp, app, t, router)
}

async fn wait_for_preflights(path: &std::path::Path, count: usize) {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            let entered = std::fs::read_dir(path)
                .unwrap()
                .filter(|entry| {
                    entry
                        .as_ref()
                        .unwrap()
                        .file_name()
                        .to_string_lossy()
                        .starts_with("entered-")
                })
                .count();
            if entered >= count {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("Git preflight did not reach the controlled remote");
}

#[tokio::test]
async fn remote_preflights_release_controls_and_preserve_concurrent_task_actions() {
    use axum::http::StatusCode;
    for action in ["retry", "reconcile"] {
        for (mutation, remote_fails) in [("cancel", false), ("archive", true)] {
            let (temp, app, mut t, router) = held_preflight_fixture().await;
            if action == "reconcile" {
                t.blocked_reason = Some(BlockedReason::RemoteConflict);
                app.save_task(&mut t).unwrap();
            }
            let request = tokio::spawn(
                router
                    .clone()
                    .oneshot(control_request(&format!("tasks/{}/{action}", t.id))),
            );
            wait_for_preflights(temp.path(), 1).await;
            let mut unrelated = task();
            unrelated.status = Status::Executing;
            app.store.put("task", &unrelated.id, &unrelated).unwrap();
            let token = app.shutdown.child_token();
            app.runtime
                .lock()
                .unwrap()
                .tasks
                .insert(unrelated.id.clone(), token.clone());
            for path in [
                "control/pause".into(),
                format!("tasks/{}/cancel", unrelated.id),
                format!("tasks/{}/{mutation}", t.id),
            ] {
                let response = tokio::time::timeout(
                    Duration::from_secs(1),
                    router.clone().oneshot(control_request(&path)),
                )
                .await
                .expect("Operator control waited for Git")
                .unwrap();
                assert_eq!(response.status(), StatusCode::OK, "{path}");
            }
            assert!(token.is_cancelled());
            assert!(app.control().unwrap().paused);
            let changed: Task = app.store.get("task", &t.id).unwrap().unwrap();
            if remote_fails {
                std::fs::write(temp.path().join("fail"), "").unwrap();
            }
            std::fs::remove_file(temp.path().join("hold")).unwrap();
            assert_eq!(
                request.await.unwrap().unwrap().status(),
                StatusCode::CONFLICT
            );
            let saved: Task = app.store.get("task", &t.id).unwrap().unwrap();
            assert_eq!(
                json!(saved),
                json!(changed),
                "Stale {action} overwrote {mutation}"
            );
        }
    }
}

#[tokio::test]
async fn concurrent_retries_queue_only_one_attempt() {
    use axum::http::StatusCode;
    let (temp, app, t, router) = held_preflight_fixture().await;
    let path = format!("tasks/{}/retry", t.id);
    let first = tokio::spawn(router.clone().oneshot(control_request(&path)));
    let second = tokio::spawn(router.oneshot(control_request(&path)));
    wait_for_preflights(temp.path(), 2).await;
    std::fs::remove_file(temp.path().join("hold")).unwrap();
    let mut statuses = [
        first.await.unwrap().unwrap().status(),
        second.await.unwrap().unwrap().status(),
    ];
    statuses.sort();
    assert_eq!(statuses, [StatusCode::OK, StatusCode::CONFLICT]);
    let saved: Task = app.store.get("task", &t.id).unwrap().unwrap();
    assert_eq!(saved.status, Status::Queued);
    assert_eq!(saved.attempts, 1);
}

#[tokio::test]
async fn retry_starts_a_fresh_repair_round_budget() {
    let (temp, app, mut t, router) = held_preflight_fixture().await;
    let round = ReviewRound {
        session_id: "reviewer".into(),
        revision: "r".into(),
        comparison_base: "c".into(),
        result: Review {
            completed: true,
            summary: "findings".into(),
            findings: vec![],
        },
        created_at: now(),
    };
    t.reviews = vec![round.clone(), round];
    t.blocked_reason = Some(BlockedReason::VerificationFailed);
    app.store.put("task", &t.id, &t).unwrap();
    let path = format!("tasks/{}/retry", t.id);
    let request = tokio::spawn(router.oneshot(control_request(&path)));
    wait_for_preflights(temp.path(), 1).await;
    std::fs::remove_file(temp.path().join("hold")).unwrap();
    assert_eq!(
        request.await.unwrap().unwrap().status(),
        axum::http::StatusCode::OK
    );
    let saved: Task = app.store.get("task", &t.id).unwrap().unwrap();
    assert_eq!((saved.attempts, saved.review_baseline), (1, 2));
    assert_eq!(saved.reviews.len(), 2, "earlier evidence is retained");
    assert_eq!(saved.attempt_reviews(), 0);
}

#[tokio::test]
async fn retry_rechecks_policy_after_remote_checks() {
    let (temp, app, t, router) = held_preflight_fixture().await;
    let request = tokio::spawn(router.oneshot(control_request(&format!("tasks/{}/retry", t.id))));
    wait_for_preflights(temp.path(), 1).await;
    {
        let _gate = tokio::time::timeout(Duration::from_secs(1), app.gate.lock())
            .await
            .unwrap();
        let mut config = app.config().unwrap();
        config.max_retries = 0;
        app.store.put("settings", "config", &config).unwrap();
    }
    std::fs::remove_file(temp.path().join("hold")).unwrap();
    assert_eq!(
        request.await.unwrap().unwrap().status(),
        axum::http::StatusCode::CONFLICT
    );
    let saved: Task = app.store.get("task", &t.id).unwrap().unwrap();
    assert_eq!(json!(saved), json!(t));
}

fn task() -> Task {
    serde_json::from_value(json!({
        "id":id(),"cycle_id":"cycle","proposal":{"id":"a","title":"Concrete improvement","problem":"Missing behavior","benefit":"Useful behavior","scope":"one file","evidence":["README.md"],"category":"features","target":"main","tier":"M","dependencies":[],"prompt":"Implement the documented behavior","decision":"accepted","reason":"Grounded"},
        "status":"queued","route":Route::new("fixture","low"),"config":Config {github_repo:"fixture/project".into(),..Config::default()},
        "source_revision":"source","comparison_base":"source","default_revision":"source","branch":"octomus/work","workspace":"","execution_session":null,"repair_session":null,"sessions":[],"reviews":[],"verification":[],"output_commit":null,"pr_number":null,"pr_url":null,"attempts":0,"error":null,"created_at":now(),"updated_at":now()
    })).unwrap()
}
#[test]
fn live_policy_survives_restart_and_never_uses_task_snapshot() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    let queued = task();
    store.put("task", &queued.id, &queued).unwrap();
    let mut live = queued.config.clone();
    live.max_sessions_per_day = 2;
    store.put("settings", "config", &live).unwrap();
    let reserve = |s: &Store| {
        s.reserve_session(
            0,
            &Admission::new("cycle", Some(&queued.id), "executor", &queued.route),
        )
    };
    reserve(&store).unwrap();
    reserve(&store).unwrap();
    assert!(
        reserve(&store).unwrap_err().downcast_ref::<BlockedReason>()
            == Some(&BlockedReason::BudgetExhausted)
    );
    live.max_sessions_per_day = 1;
    store.put("settings", "config", &live).unwrap();
    drop(store);
    let store = Store::open(&path).unwrap();
    assert!(reserve(&store).is_err());
    assert_eq!(store.sessions_today().unwrap(), 2);
    live.max_sessions_per_day = 3;
    store.put("settings", "config", &live).unwrap();
    reserve(&store).unwrap();
    live.max_workspace_bytes = 42;
    store.put("settings", "config", &live).unwrap();
    assert_eq!(
        store
            .reserve_session(
                42,
                &Admission::new("cycle", None, "reviewer", &queued.route)
            )
            .unwrap_err()
            .downcast_ref::<BlockedReason>(),
        Some(&BlockedReason::StorageLimit)
    );
    assert_eq!(store.sessions_today().unwrap(), 3);
    assert_eq!(
        store
            .get::<Task>("task", &queued.id)
            .unwrap()
            .unwrap()
            .config
            .max_sessions_per_day,
        150
    );
}
#[tokio::test]
async fn retry_preflight_adopts_the_current_command_timeout() {
    use axum::{
        body::Body,
        http::{Request, StatusCode},
    };
    use std::os::unix::fs::PermissionsExt;
    use tower::ServiceExt;

    let temp = tempfile::tempdir().unwrap();
    let store = Store::open(&temp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), temp.path().into());
    let repository = temp.path().join("repository");
    std::fs::create_dir(&repository).unwrap();
    let mut config = Config {
        repository: repository.clone(),
        ..Default::default()
    };
    let cancel = CancellationToken::new();
    for args in [
        vec!["init", "-b", "main"],
        vec![
            "-c",
            "user.name=Fixture",
            "-c",
            "user.email=fixture@example.com",
            "commit",
            "--allow-empty",
            "-m",
            "Initial fixture",
        ],
        vec!["remote", "add", "origin", "."],
    ] {
        git::git(&config, &repository, &args, &cancel)
            .await
            .unwrap();
    }
    let revision = git::git(&config, &repository, &["rev-parse", "HEAD"], &cancel)
        .await
        .unwrap();
    let upload_pack = temp.path().join("slow-upload-pack");
    std::fs::write(
        &upload_pack,
        "#!/bin/sh\nsleep 2\nexec git-upload-pack \"$@\"\n",
    )
    .unwrap();
    std::fs::set_permissions(&upload_pack, std::fs::Permissions::from_mode(0o755)).unwrap();
    git::git(
        &config,
        &repository,
        &[
            "config",
            "remote.origin.uploadpack",
            upload_pack.to_str().unwrap(),
        ],
        &cancel,
    )
    .await
    .unwrap();

    config.command_timeout_seconds = 1;
    store.put("settings", "config", &config).unwrap();
    let mut task = task();
    task.config = config.clone();
    task.attempt_policy = Some(AttemptPolicy::from_config(&config));
    task.source_revision = revision.clone();
    task.default_revision = revision;
    task.status = Status::Blocked;
    store.put("task", &task.id, &task).unwrap();
    let token = "retry-test-operator-token-at-least-32-characters";
    let router = octomus_agent::api::router(app, token, Some(temp.path().into()));
    let request = || {
        Request::builder()
            .uri(format!("/api/tasks/{}/retry", task.id))
            .method("POST")
            .header("authorization", format!("Bearer {token}"))
            .header("content-type", "application/json")
            .body(Body::from("{}"))
            .unwrap()
    };
    assert_eq!(
        router.clone().oneshot(request()).await.unwrap().status(),
        StatusCode::BAD_REQUEST
    );
    assert_eq!(
        store
            .get::<Task>("task", &task.id)
            .unwrap()
            .unwrap()
            .attempts,
        0
    );
    config.command_timeout_seconds = 5;
    store.put("settings", "config", &config).unwrap();
    assert_eq!(
        router.oneshot(request()).await.unwrap().status(),
        StatusCode::OK
    );
    let retried: Task = store.get("task", &task.id).unwrap().unwrap();
    assert_eq!(retried.status, Status::Queued);
    assert_eq!(retried.attempts, 1);
    assert_eq!(retried.execution_config().command_timeout_seconds, 5);
    assert_eq!(retried.config.command_timeout_seconds, 1);
}
#[test]
fn publication_checks_every_identity_field_and_closed_reconciliation() {
    let mut t = task();
    t.output_commit = Some("reviewed".into());
    let p:PullRequest=serde_json::from_value(json!({"number":1,"title":"x","branch":t.branch,"head":"reviewed","base":"main","url":"https://github.com/fixture/project/pull/1","body":format!("<!-- octomus:task:{} -->",t.id),"state":"open","changed_lines":1,"created_at":now(),"owned":true,"head_repository":"fixture/project","base_repository":"fixture/project"})).unwrap();
    git::validate_publication(&t, &p, false).unwrap();
    for field in [
        "head",
        "branch",
        "base",
        "body",
        "head_repository",
        "base_repository",
        "owned",
    ] {
        let mut value = serde_json::to_value(&p).unwrap();
        value[field] = if field == "owned" {
            json!(false)
        } else {
            json!("mismatch")
        };
        assert!(
            git::validate_publication(&t, &serde_json::from_value(value).unwrap(), false).is_err(),
            "{field}"
        );
    }
    for state in ["closed", "merged"] {
        let mut p = p.clone();
        p.state = state.into();
        assert!(git::validate_publication(&t, &p, false).is_err());
        git::validate_publication(&t, &p, true).unwrap();
    }
}
#[test]
fn shared_branch_requires_a_total_dependency_order() {
    let c = Config::default();
    let t = task();
    let mut a = t.proposal;
    a.target = "octomus/existing".into();
    let mut b = a.clone();
    b.id = "b".into();
    b.title = "Second".into();
    let mut d = a.clone();
    d.id = "c".into();
    d.title = "Third".into();
    let pr:PullRequest=serde_json::from_value(json!({"number":1,"title":"Existing","branch":a.target,"head":"source","base":"main","url":"https://github.com/fixture/project/pull/1","body":"","state":"open","changed_lines":1,"created_at":now(),"owned":true})).unwrap();
    let g = Grounding {
        revision: "source".into(),
        prs: vec![pr],
        external_prs: vec![],
        pr_coverage: PrCoverage::default(),
        history: json!([]),
        maintenance_due: false,
        maintenance_targets: vec![],
    };
    assert!(validate_proposals(&c, &[a.clone(), b.clone()], &g, &[]).is_err());
    b.dependencies = vec![a.id.clone()];
    d.dependencies = vec![a.id.clone()];
    assert!(validate_proposals(&c, &[a.clone(), b.clone(), d.clone()], &g, &[]).is_err());
    d.dependencies.push(b.id.clone());
    validate_proposals(&c, &[d, b, a], &g, &[]).unwrap();
}
#[tokio::test]
async fn machine_capture_never_corrupts_successful_json() {
    let tmp = tempfile::tempdir().unwrap();
    let token = CancellationToken::new();
    let text = process::run_machine(
        "python3",
        &[
            "-c",
            "import json; print(json.dumps({'body': 'x' * 402790}))",
        ],
        tmp.path(),
        10,
        &token,
    )
    .await
    .unwrap();
    assert!(serde_json::from_str::<Value>(&text).is_ok());
    let error = process::run_machine(
        "python3",
        &[
            "-c",
            "import sys; sys.stdout.write('x' * (16 * 1024 * 1024 + 1))",
        ],
        tmp.path(),
        10,
        &token,
    )
    .await
    .unwrap_err();
    assert!(error.downcast_ref::<process::OutputTooLarge>().is_some());
    assert!(
        process::run_machine(
            "python3",
            &["-c", "import sys; sys.stdout.buffer.write(bytes([255]))"],
            tmp.path(),
            10,
            &token
        )
        .await
        .unwrap_err()
        .to_string()
        .contains("UTF-8")
    );
    let out = process::capture(
        "python3",
        &["-c", "print('x' * 300000)"],
        tmp.path(),
        10,
        &token,
        process::CaptureMode::Diagnostic,
    )
    .await
    .unwrap();
    assert!(out.status.success() && out.stdout.truncated);
    assert_eq!(out.stdout.bytes.len(), process::DIAGNOSTIC_LIMIT);
}
#[test]
fn old_attention_survives_bounded_dashboard_and_pages() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let mut old = task();
    old.status = Status::Blocked;
    store.put("task", &old.id, &old).unwrap();
    let mut published = task();
    published.status = Status::Published;
    published.proposal.prompt = "x".repeat(64000);
    for i in 0..1000 {
        published.id = format!("task-{i}");
        store.put("task", &published.id, &published).unwrap();
    }
    let snapshot = store.dashboard().unwrap();
    assert_eq!(snapshot["tasks"].as_array().unwrap().len(), 300);
    assert!(serde_json::to_vec(&snapshot).unwrap().len() < 1024 * 1024);
    assert_eq!(snapshot["counts"]["blocked"], 1);
    assert_eq!(snapshot["attention_tasks"][0]["id"], old.id);
    let page = store
        .history_page(
            "task",
            &HistoryQuery {
                status: Some("attention".into()),
                ..Default::default()
            },
        )
        .unwrap();
    assert_eq!(page.items.len(), 1);
    assert_eq!(page.items[0]["id"], old.id);
    old.status = Status::Executing;
    store.put("task", &old.id, &old).unwrap();
    let active = store.dashboard().unwrap();
    assert_eq!(active["tasks"][0]["id"], old.id);
    assert_eq!(active["tasks"].as_array().unwrap().len(), 300);
    let first = store
        .history_page("task", &HistoryQuery::default())
        .unwrap();
    let second = store
        .history_page(
            "task",
            &HistoryQuery {
                before: first.next_cursor,
                ..Default::default()
            },
        )
        .unwrap();
    assert_eq!(first.items.len(), 50);
    assert!(
        second
            .items
            .iter()
            .all(|r| !first.items.iter().any(|a| a["id"] == r["id"]))
    );
}
#[test]
fn legacy_modes_and_attempts_have_explicit_defaults() {
    for (paused, mode) in [
        (true, OperatingMode::Paused),
        (false, OperatingMode::Continuous),
    ] {
        let control: Control = serde_json::from_value(
            json!({"paused":paused,"cycle_number":4,"next_cycle_at":5,"error":null}),
        )
        .unwrap();
        assert_eq!(control.mode, mode);
    }
    let mut t = task();
    let original = serde_json::to_value(&t.config).unwrap();
    let mut config = t.config.clone();
    config.task_timeout_seconds = 123;
    t.attempt_policy = Some(AttemptPolicy::from_config(&config));
    assert_eq!(t.execution_config().task_timeout_seconds, 123);
    assert_eq!(serde_json::to_value(&t.config).unwrap(), original);
    t.status = Status::Blocked;
    t.blocked_reason = Some(BlockedReason::StaleBase);
    assert!(t.allowed_actions().contains(&"supersede"));
    assert!(!t.allowed_actions().contains(&"retry"));
    assert_eq!(idle_delay(1800, 1), 1800);
    assert_eq!(idle_delay(1800, 2), 3600);
    assert_eq!(idle_delay(1800, 20), 86400);
    assert_eq!(idle_delay(100000, 20), 100000);
}
#[test]
fn one_shot_membership_and_committed_phase_survive_restart() {
    let tmp = tempfile::tempdir().unwrap();
    let path = tmp.path().join("state.db");
    let store = Store::open(&path).unwrap();
    let t = task();
    store.put("task", &t.id, &t).unwrap();
    let mut control = Control::default();
    store.start_batch(&mut control).unwrap();
    let run = control.batch.as_ref().unwrap().id.clone();
    assert_eq!(
        store
            .get::<Task>("task", &t.id)
            .unwrap()
            .unwrap()
            .run_id
            .as_deref(),
        Some(run.as_str())
    );
    let mut later = task();
    later.id = id();
    store.put("task", &later.id, &later).unwrap();
    assert_eq!(store.batch_counts(&run).unwrap(), (1, 0));
    let cycle:Cycle=serde_json::from_value(json!({"id":id(),"mode":"execution","number":1,"status":"completed","started_at":now(),"completed_at":now(),"grounding":null,"proposals":[],"assessments":[],"sessions":[],"error":null,"run_id":run})).unwrap();
    control.batch.as_mut().unwrap().phase = BatchPhase::Planning;
    control.batch.as_mut().unwrap().cycle_id = Some(cycle.id.clone());
    store.put("settings", "control", &control).unwrap();
    store.commit_plan(&cycle, &[]).unwrap();
    drop(store);
    let app = App::new(Store::open(&path).unwrap(), tmp.path().into());
    app.recover().unwrap();
    assert_eq!(
        app.control().unwrap().batch.unwrap().phase,
        BatchPhase::Executing
    );
}

#[tokio::test]
async fn paused_housekeeping_preserves_unresolved_evidence_and_rejects_symlinks() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let mut refused = task();
    refused.status = Status::Published;
    refused.updated_at = "2020-01-01T00:00:00Z".into();
    let protected = tmp.path().join("refused-target");
    std::fs::create_dir_all(protected.join("workspace")).unwrap();
    std::fs::write(protected.join("workspace/evidence"), "preserve").unwrap();
    let tasks = tmp.path().join("tasks");
    std::fs::create_dir(&tasks).unwrap();
    std::os::unix::fs::symlink(&protected, tasks.join(&refused.id)).unwrap();
    refused.workspace = tasks
        .join(&refused.id)
        .join("workspace")
        .to_string_lossy()
        .into_owned();
    store.put("task", &refused.id, &refused).unwrap();
    let mut completed = task();
    completed.status = Status::Published;
    completed.updated_at = "2020-01-01T00:00:00Z".into();
    completed.workspace = tmp
        .path()
        .join("tasks")
        .join(&completed.id)
        .join("workspace")
        .to_string_lossy()
        .into_owned();
    std::fs::create_dir_all(&completed.workspace).unwrap();
    std::fs::write(
        std::path::Path::new(&completed.workspace).join("evidence"),
        "completed",
    )
    .unwrap();
    store.put("task", &completed.id, &completed).unwrap();
    let mut unresolved = task();
    unresolved.status = Status::Blocked;
    unresolved.updated_at = completed.updated_at.clone();
    unresolved.workspace = tmp
        .path()
        .join("tasks")
        .join(&unresolved.id)
        .join("workspace")
        .to_string_lossy()
        .into_owned();
    std::fs::create_dir_all(&unresolved.workspace).unwrap();
    std::fs::write(
        std::path::Path::new(&unresolved.workspace).join("evidence"),
        "unresolved",
    )
    .unwrap();
    store.put("task", &unresolved.id, &unresolved).unwrap();
    let transcripts = tmp.path().join("runner-storage");
    std::fs::create_dir(&transcripts).unwrap();
    std::fs::write(transcripts.join("synthetic-session"), b"fixture").unwrap();
    let mut policy = Config::default();
    policy
        .runner_storage_paths
        .insert("codex".into(), transcripts.clone());
    store.put("settings", "config", &policy).unwrap();
    let service = tokio::spawn(app.clone().run());
    for _ in 0..100 {
        if store.get::<Value>("settings", "storage").unwrap().is_some() {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(50)).await;
    }
    assert!(app.control().unwrap().paused);
    assert!(protected.join("workspace/evidence").exists());
    assert!(
        std::fs::symlink_metadata(tasks.join(&refused.id))
            .unwrap()
            .is_symlink()
    );
    assert!(
        store
            .get::<Task>("task", &refused.id)
            .unwrap()
            .unwrap()
            .lifecycle
            .discarded_at
            .is_none()
    );
    assert!(
        store
            .events(Some(&refused.id))
            .unwrap()
            .iter()
            .any(|event| { event.kind == "cleanup_error" && event.message.contains("symlink") })
    );
    assert!(!std::path::Path::new(&completed.workspace).exists());
    assert!(
        std::path::Path::new(&unresolved.workspace)
            .join("evidence")
            .exists()
    );
    assert!(
        store
            .get::<Task>("task", &completed.id)
            .unwrap()
            .unwrap()
            .lifecycle
            .discarded_at
            .is_some()
    );
    assert!(store.get::<Value>("settings", "storage").unwrap().is_some());
    let discarded: Task = store.get("task", &completed.id).unwrap().unwrap();
    assert!(discarded.lifecycle.archived_at.is_none());
    assert_eq!(discarded.allowed_actions(), ["archive"]);
    let duplicates = || {
        store
            .duplicate_tasks(
                &completed.config.github_repo,
                std::slice::from_ref(&completed.proposal),
            )
            .unwrap()
    };
    assert!(duplicates().iter().any(|t| t.id == completed.id));
    let router = octomus_agent::api::router(app.clone(), CONTROL_TOKEN, Some(tmp.path().into()));
    assert_eq!(
        router
            .oneshot(control_request(&format!("tasks/{}/archive", completed.id)))
            .await
            .unwrap()
            .status(),
        axum::http::StatusCode::OK
    );
    let archived: Task = store.get("task", &completed.id).unwrap().unwrap();
    assert_eq!(archived.status, Status::Published);
    assert!(archived.lifecycle.archived_at.is_some());
    assert_eq!(
        archived.lifecycle.discarded_at,
        discarded.lifecycle.discarded_at
    );
    assert!(archived.allowed_actions().is_empty());
    assert!(!duplicates().iter().any(|t| t.id == completed.id));
    let storage: Value = store.get("settings", "storage").unwrap().unwrap();
    assert_eq!(
        storage["runner_transcripts"]["runners"]["codex"]["bytes"],
        7
    );
    assert!(storage["runner_transcripts"]["runners"]["opencode"]["bytes"].is_null());
    assert!(transcripts.join("synthetic-session").is_file());
    app.shutdown.cancel();
    service.await.unwrap();
    let outside = tmp.path().join("outside");
    std::fs::create_dir(&outside).unwrap();
    std::fs::write(outside.join("evidence"), "preserve").unwrap();
    let link = tmp.path().join("tasks").join(&completed.id);
    std::os::unix::fs::symlink(&outside, &link).unwrap();
    completed.workspace = link.join("workspace").to_string_lossy().into_owned();
    assert!(app.discard_task(&mut completed).await.is_err());
    assert!(outside.join("evidence").exists());
}

#[test]
fn late_retry_cannot_erase_a_one_shot_failure() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let mut task = task();
    store.put("task", &task.id, &task).unwrap();
    let mut control = Control::default();
    store.start_batch(&mut control).unwrap();
    let run = control.batch.unwrap().id;
    task = store.get("task", &task.id).unwrap().unwrap();
    task.status = Status::Blocked;
    store.put("task", &task.id, &task).unwrap();
    task.status = Status::Queued;
    task.run_id = None;
    store.put("task", &task.id, &task).unwrap();
    assert_eq!(store.batch_counts(&run).unwrap(), (0, 1));
}

#[tokio::test]
async fn queued_history_never_hides_active_branch_writers() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let mut config = Config {
        repository: tmp.path().join("checkout"),
        github_repo: "fixture/project".into(),
        execution_concurrency: 2,
        verification_commands: vec!["true".into()],
        ..Default::default()
    };
    for route in config.roles.values_mut() {
        *route = Route::new("fixture", "low");
    }
    std::fs::create_dir_all(config.repository.join(".git")).unwrap();
    config.validate(true).unwrap();
    store.put("settings", "config", &config).unwrap();
    // This first queued task proves a scheduler tick ran without launching a worker.
    let mut sentinel = task();
    sentinel.config = config.clone();
    sentinel.proposal.dependencies = vec!["missing-dependency".into()];
    store.put("task", &sentinel.id, &sentinel).unwrap();
    let mut queued_ids = vec![];
    for _ in 0..501 {
        let mut queued = task();
        queued.config = config.clone();
        queued.cycle_id = id();
        queued.proposal.target = "octomus/existing".into();
        queued.run_id = Some("other-batch".into());
        store.put("task", &queued.id, &queued).unwrap();
        queued_ids.push(queued.id);
    }
    let mut active_ids = vec![];
    for status in [
        Status::Executing,
        Status::Reviewing,
        Status::Repairing,
        Status::Verifying,
        Status::Publishing,
    ] {
        let mut active = task();
        active.config = config.clone();
        active.proposal.target = "octomus/existing".into();
        active.status = status;
        store.put("task", &active.id, &active).unwrap();
        active_ids.push(active.id);
    }
    for run in [None, Some("other-batch"), Some("empty-batch")] {
        let tasks = store.scheduling_tasks(run).unwrap();
        assert_eq!(
            tasks.iter().filter(|t| t.status == Status::Queued).count(),
            if run == Some("empty-batch") {
                0
            } else if run.is_none() {
                501
            } else {
                500
            }
        );
        assert_eq!(
            tasks.iter().filter(|t| t.status.active()).count(),
            active_ids.len()
        );
        assert!(
            active_ids
                .iter()
                .all(|id| tasks.iter().any(|t| &t.id == id))
        );
    }
    {
        let mut rt = app.runtime.lock().unwrap();
        rt.tasks
            .insert(active_ids[0].clone(), app.shutdown.child_token());
        rt.checked_cycles.insert(sentinel.cycle_id.clone());
        rt.last_retention_at = chrono::Utc::now().timestamp();
        rt.last_observation_at = rt.last_retention_at;
    }
    let mut control = Control::default();
    control.set_mode(OperatingMode::Continuous);
    store.put("settings", "control", &control).unwrap();
    let service = tokio::spawn(app.clone().run());
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            if store
                .get::<Task>("task", &sentinel.id)
                .unwrap()
                .unwrap()
                .status
                == Status::Blocked
            {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("Scheduler did not inspect the queue");
    app.shutdown.cancel();
    service.await.unwrap();
    for id in queued_ids {
        assert_eq!(
            store.get::<Task>("task", &id).unwrap().unwrap().status,
            Status::Queued
        );
    }
    assert_eq!(store.sessions_today().unwrap(), 0);
}

#[tokio::test]
async fn one_shot_blocks_dependents_of_retries_excluded_from_the_batch() {
    for phase in [BatchPhase::Draining, BatchPhase::Executing] {
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("state.db");
        let store = Store::open(&path).unwrap();
        let mut config = Config {
            repository: tmp.path().join("checkout"),
            github_repo: "fixture/project".into(),
            codex_binary: "/nonexistent-octomus-test-runner".into(),
            verification_commands: vec!["true".into()],
            ..Config::default()
        };
        for route in config.roles.values_mut() {
            *route = Route::new("fixture", "low");
        }
        std::fs::create_dir_all(config.repository.join(".git")).unwrap();
        config.validate(true).unwrap();
        store.put("settings", "config", &config).unwrap();
        let mut prerequisite = task();
        prerequisite.config = config;
        prerequisite.proposal.target = "octomus/existing".into();
        let mut dependent = prerequisite.clone();
        dependent.id = id();
        dependent.proposal.dependencies = vec![prerequisite.id.clone()];
        store.put("task", &prerequisite.id, &prerequisite).unwrap();
        store.put("task", &dependent.id, &dependent).unwrap();
        let mut control = Control::default();
        store.start_batch(&mut control).unwrap();
        control.batch.as_mut().unwrap().phase = phase;
        store.put("settings", "control", &control).unwrap();
        let run = control.batch.unwrap().id;
        prerequisite = store.get("task", &prerequisite.id).unwrap().unwrap();
        prerequisite.status = Status::Blocked;
        store.put("task", &prerequisite.id, &prerequisite).unwrap();
        // A retry belongs to later work, while its failure remains in this run.
        prerequisite.status = Status::Queued;
        prerequisite.run_id = None;
        store.put("task", &prerequisite.id, &prerequisite).unwrap();
        drop(store);

        let app = App::new(Store::open(&path).unwrap(), tmp.path().into());
        app.recover().unwrap();
        {
            let mut runtime = app.runtime.lock().unwrap();
            runtime.last_retention_at = chrono::Utc::now().timestamp();
            runtime.last_observation_at = runtime.last_retention_at;
        }
        let service = tokio::spawn(app.clone().run());
        for _ in 0..100 {
            if app.control().unwrap().paused {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        }
        app.shutdown.cancel();
        service.await.unwrap();
        let control = app.control().unwrap();
        assert!(control.paused && control.error.is_none(), "{control:?}");
        let saved: Task = app.store.get("task", &dependent.id).unwrap().unwrap();
        assert_eq!(saved.status, Status::Blocked);
        assert_eq!(saved.blocked_reason, Some(BlockedReason::DependencyBlocked));
        let saved: Task = app.store.get("task", &prerequisite.id).unwrap().unwrap();
        assert_eq!(saved.status, Status::Queued);
        assert!(saved.run_id.is_none());
        assert_eq!(app.store.batch_counts(&run).unwrap(), (0, 2));
        assert!(app.store.list::<Cycle>("cycle").unwrap().is_empty());
        assert_eq!(app.store.sessions_today().unwrap(), 0);
    }
}

#[tokio::test]
async fn unaffordable_planning_refuses_audit_and_run_once_without_side_effects() {
    use axum::http::StatusCode;
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let mut config = Config {
        repository: tmp.path().join("checkout"),
        github_repo: "fixture/project".into(),
        max_sessions_per_day: 12,
        verification_commands: vec!["true".into()],
        ..Default::default()
    };
    for route in config.roles.values_mut() {
        *route = Route::new("fixture", "low");
    }
    std::fs::create_dir_all(config.repository.join(".git")).unwrap();
    config.validate(true).unwrap();
    config.validate_audit().unwrap();
    store.put("settings", "config", &config).unwrap();
    let queued = task();
    store.put("task", &queued.id, &queued).unwrap();
    let before = serde_json::to_value(app.control().unwrap()).unwrap();
    let router = octomus_agent::api::router(app.clone(), CONTROL_TOKEN, Some(tmp.path().into()));
    for action in ["audit", "cycle"] {
        let response = router
            .clone()
            .oneshot(control_request(&format!("control/{action}")))
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::CONFLICT, "{action}");
        let body: Value = serde_json::from_slice(
            &axum::body::to_bytes(response.into_body(), 4096)
                .await
                .unwrap(),
        )
        .unwrap();
        let message = body["error"].as_str().unwrap();
        assert!(
            message.contains("requires 13") && message.contains("cannot fund"),
            "{body}"
        );
    }
    assert_eq!(
        serde_json::to_value(app.control().unwrap()).unwrap(),
        before
    );
    assert!(
        store
            .get::<Task>("task", &queued.id)
            .unwrap()
            .unwrap()
            .run_id
            .is_none()
    );
    assert!(store.list::<Cycle>("cycle").unwrap().is_empty());
    assert_eq!(store.sessions_today().unwrap(), 0);
}

#[tokio::test]
async fn run_once_pauses_when_the_drain_consumed_planning_allowance() {
    use axum::http::StatusCode;
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let mut config = Config {
        repository: tmp.path().join("checkout"),
        github_repo: "fixture/project".into(),
        max_sessions_per_day: 14,
        verification_commands: vec!["true".into()],
        ..Default::default()
    };
    for route in config.roles.values_mut() {
        *route = Route::new("fixture", "low");
    }
    std::fs::create_dir_all(config.repository.join(".git")).unwrap();
    config.validate(true).unwrap();
    store.put("settings", "config", &config).unwrap();
    let router = octomus_agent::api::router(app.clone(), CONTROL_TOKEN, Some(tmp.path().into()));
    let response = router
        .oneshot(control_request("control/cycle"))
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::OK);
    let run = app.control().unwrap().batch.unwrap().id;
    for role in ["executor", "reviewer"] {
        store
            .reserve_session(
                0,
                &Admission::new(
                    "other-cycle",
                    Some("task"),
                    role,
                    &Route::new("fixture", "low"),
                ),
            )
            .unwrap();
    }
    {
        let mut rt = app.runtime.lock().unwrap();
        rt.last_retention_at = chrono::Utc::now().timestamp();
        rt.last_observation_at = rt.last_retention_at;
    }
    let service = tokio::spawn(app.clone().run());
    tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            if app.control().unwrap().paused {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .expect("Run once did not pause on unaffordable planning");
    let control = app.control().unwrap();
    assert_eq!(control.mode, OperatingMode::Paused);
    assert!(control.batch.is_none());
    let error = control.error.as_deref().unwrap_or_default();
    assert!(
        error.contains("requires 13") && error.contains("12 remain"),
        "{error}"
    );
    assert!(store.list::<Cycle>("cycle").unwrap().is_empty());
    assert_eq!(store.sessions_today().unwrap(), 2);
    assert_eq!(
        store
            .events(Some("system"))
            .unwrap()
            .iter()
            .filter(|e| e.kind == "planning_capacity")
            .count(),
        1
    );
    assert_eq!(store.batch_counts(&run).unwrap(), (0, 0));
    config.max_sessions_per_day = 150;
    store.put("settings", "config", &config).unwrap();
    tokio::time::sleep(Duration::from_millis(1300)).await;
    assert!(app.control().unwrap().paused);
    assert!(store.list::<Cycle>("cycle").unwrap().is_empty());
    app.shutdown.cancel();
    service.await.unwrap();
}

#[tokio::test]
async fn continuous_waits_for_planning_allowance_without_failed_cycles() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let app = App::new(store.clone(), tmp.path().into());
    let mut config = Config {
        repository: tmp.path().join("checkout"),
        github_repo: "fixture/project".into(),
        codex_binary: "/nonexistent-octomus-test-runner".into(),
        max_sessions_per_day: 13,
        verification_commands: vec!["true".into()],
        ..Default::default()
    };
    for route in config.roles.values_mut() {
        *route = Route::new("fixture", "low");
    }
    std::fs::create_dir_all(config.repository.join(".git")).unwrap();
    config.validate(true).unwrap();
    store.put("settings", "config", &config).unwrap();
    store
        .reserve_session(
            0,
            &Admission::new("cycle", None, "executor", &Route::new("fixture", "low")),
        )
        .unwrap();
    let mut control = Control::default();
    control.set_mode(OperatingMode::Continuous);
    store.put("settings", "control", &control).unwrap();
    {
        let mut rt = app.runtime.lock().unwrap();
        rt.last_retention_at = chrono::Utc::now().timestamp();
        rt.last_observation_at = rt.last_retention_at;
    }
    let service = tokio::spawn(app.clone().run());
    tokio::time::sleep(Duration::from_millis(1300)).await;
    let control = app.control().unwrap();
    assert_eq!(control.mode, OperatingMode::Continuous);
    assert!(!control.paused && control.error.is_none() && control.cycle_number == 0);
    assert!(store.list::<Cycle>("cycle").unwrap().is_empty());
    assert!(
        !store
            .events(None)
            .unwrap()
            .iter()
            .any(|e| e.kind == "planning_capacity")
    );
    config.max_sessions_per_day = 150;
    store.put("settings", "config", &config).unwrap();
    tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            if !store.list::<Cycle>("cycle").unwrap().is_empty() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .expect("Continuous operation did not reconsider after the allowance returned");
    app.shutdown.cancel();
    service.await.unwrap();
}

#[test]
fn unresolved_problem_identity_survives_rewording() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let mut old = task();
    old.status = Status::Blocked;
    old.proposal.problem_key = "stable-problem".into();
    store.put("task", &old.id, &old).unwrap();
    let mut proposed = old.proposal.clone();
    proposed.id = "fresh".into();
    proposed.title = "Different wording for the same work".into();
    let duplicates = store
        .duplicate_tasks(&old.config.github_repo, std::slice::from_ref(&proposed))
        .unwrap();
    assert_eq!(duplicates.len(), 1);
    let g = Grounding {
        revision: "new-context".into(),
        prs: vec![],
        external_prs: vec![],
        pr_coverage: PrCoverage::default(),
        history: json!([]),
        maintenance_due: false,
        maintenance_targets: vec![],
    };
    assert!(validate_proposals(&old.config, &[proposed], &g, &duplicates).is_err());
}

#[test]
fn published_work_remains_in_duplicate_lookups() {
    let tmp = tempfile::tempdir().unwrap();
    let store = Store::open(&tmp.path().join("state.db")).unwrap();
    let mut delivered = task();
    delivered.status = Status::Published;
    delivered.pr_number = Some(42);
    delivered.proposal.problem_key = "stable-problem".into();
    store.put("task", &delivered.id, &delivered).unwrap();
    for match_title in [true, false] {
        let mut proposed = delivered.proposal.clone();
        if match_title {
            proposed.problem_key = "different-key".into();
        } else {
            proposed.title = "Different wording for delivered work".into();
        }
        let duplicates = store
            .duplicate_tasks(
                &delivered.config.github_repo,
                std::slice::from_ref(&proposed),
            )
            .unwrap();
        assert_eq!(duplicates.len(), 1, "title lookup: {match_title}");
        assert_eq!(duplicates[0].id, delivered.id);
        assert!(
            store
                .duplicate_tasks("another/project", std::slice::from_ref(&proposed))
                .unwrap()
                .is_empty()
        );
        proposed.target = "another-branch".into();
        assert!(
            store
                .duplicate_tasks(&delivered.config.github_repo, &[proposed])
                .unwrap()
                .is_empty()
        );
    }
    delivered.status = Status::Cancelled;
    store.put("task", &delivered.id, &delivered).unwrap();
    assert!(
        store
            .duplicate_tasks(
                &delivered.config.github_repo,
                std::slice::from_ref(&delivered.proposal)
            )
            .unwrap()
            .is_empty()
    );
    delivered.status = Status::Published;
    delivered.lifecycle.archived_at = Some(now());
    store.put("task", &delivered.id, &delivered).unwrap();
    assert!(
        store
            .duplicate_tasks(
                &delivered.config.github_repo,
                std::slice::from_ref(&delivered.proposal)
            )
            .unwrap()
            .is_empty()
    );
}
