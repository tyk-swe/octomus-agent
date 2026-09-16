use chrono::{Duration, Utc};
use octomus_agent::engine::App;
use octomus_agent::notifications::{self, WEBHOOK_ENV};
use octomus_agent::store::Store;
use serde_json::{Value, json};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tower::ServiceExt;

mod common;
use common::*;

const DEST: &str = "destination-a";

/// The data directory of a fresh fixture, created before the store or app opens it.
fn data_dir(tmp: &tempfile::TempDir) -> std::path::PathBuf {
    let data = tmp.path().join("data");
    std::fs::create_dir_all(&data).unwrap();
    data
}

fn store() -> (tempfile::TempDir, Store, std::path::PathBuf) {
    let tmp = tempfile::tempdir().unwrap();
    let data = data_dir(&tmp);
    let path = data.join("state.db");
    (tmp, Store::open(&path).unwrap(), path)
}

fn enabled(store: &Store) {
    store
        .configure_notifications(Some(DEST), "enabled", None)
        .unwrap();
}

fn put_task(store: &Store, id: &str, status: &str, reason: Option<&str>) {
    store
        .put(
            "task",
            id,
            &json!({
                "id": id, "cycle_id": "cycle-1", "run_id": "run-1",
                "status": status, "blocked_reason": reason,
                "config": {"github_repo": "fixture/project"},
            }),
        )
        .unwrap();
}

fn outbox(path: &std::path::Path, status: &str) -> Vec<Value> {
    let connection = rusqlite::Connection::open(path).unwrap();
    let mut rows = connection
        .prepare("SELECT data FROM (SELECT json_object('event_id',event_id,'task_id',task_id,'category',category,'action',action,'repository',repository,'cycle_id',cycle_id,'run_id',run_id,'status',status,'attempts',attempts,'last_error',last_error,'http_status',http_status,'created_at',created_at,'next_attempt_at',next_attempt_at) AS data FROM notification_outbox WHERE status=?1 ORDER BY seq)")
        .unwrap();
    rows.query_map([status], |r| r.get::<_, String>(0))
        .unwrap()
        .map(|r| serde_json::from_str(&r.unwrap()).unwrap())
        .collect()
}

fn pending(path: &std::path::Path) -> Vec<Value> {
    outbox(path, "pending")
}

#[test]
fn disabled_policy_captures_nothing() {
    let (_tmp, store, path) = store();
    store
        .configure_notifications(None, "disabled", None)
        .unwrap();
    put_task(&store, "task-1", "blocked", Some("verification_failed"));
    let health = store.notification_health().unwrap();
    assert_eq!(health["state"], "disabled");
    assert_eq!(health["configured"], false);
    assert_eq!(health["pending"], 0);
    assert!(pending(&path).is_empty());
}

#[tokio::test]
async fn webhook_url_policy_accepts_https_and_loopback_http_only() {
    let tmp = tempfile::tempdir().unwrap();
    let data = data_dir(&tmp);
    let app = App::new(Store::open(&data.join("state.db")).unwrap(), data);
    let cases = [
        (
            "https://hooks.example.com/notify?token=synthetic",
            "enabled",
        ),
        ("http://127.0.0.1:8080/hook", "enabled"),
        ("http://[::1]:8080/hook", "enabled"),
        ("http://10.0.0.1:8080/hook", "invalid"),
        ("http://localhost:8080/hook", "invalid"),
        ("http://user:pass@127.0.0.1:8080/hook", "invalid"),
        ("https://example.com/hook#fragment", "invalid"),
        ("ftp://example.com/hook", "invalid"),
        ("file:///etc/passwd", "invalid"),
        ("https://example.com:99999/hook", "invalid"),
    ];
    for (url, expected) in cases {
        let handle = notifications::start(&app, Some(url.into())).unwrap();
        let health = app.store.notification_health().unwrap();
        assert_eq!(health["state"], expected, "{url} must be {expected}");
        if expected == "invalid" {
            let error = health["last_error"].as_str().unwrap_or("");
            assert!(!error.is_empty() && !error.contains(url) && !error.contains("example.com"));
        }
        handle.abort();
    }
    let handle = notifications::start(
        &app,
        Some(format!("https://example.com/{}", "x".repeat(9000))),
    )
    .unwrap();
    assert_eq!(app.store.notification_health().unwrap()["state"], "invalid");
    handle.abort();
    let handle = notifications::start(&app, None).unwrap();
    assert_eq!(
        app.store.notification_health().unwrap()["state"],
        "disabled"
    );
    handle.abort();
}

#[test]
fn attention_triggers_enqueue_one_row_per_episode() {
    let (_tmp, store, path) = store();
    enabled(&store);
    put_task(&store, "task-1", "queued", None);
    assert!(pending(&path).is_empty());
    put_task(&store, "task-1", "blocked", Some("verification_failed"));
    let rows = pending(&path);
    assert_eq!(rows.len(), 1);
    assert_eq!(rows[0]["task_id"], "task-1");
    assert_eq!(rows[0]["category"], "verification_failed");
    assert_eq!(rows[0]["action"], "inspect_task");
    assert_eq!(rows[0]["repository"], "fixture/project");
    assert_eq!(rows[0]["cycle_id"], "cycle-1");
    assert_eq!(rows[0]["run_id"], "run-1");
    assert_eq!(rows[0]["event_id"].as_str().unwrap().len(), 32);
    put_task(&store, "task-1", "blocked", Some("storage_limit"));
    assert_eq!(pending(&path).len(), 1, "same-state writes never duplicate");
    put_task(&store, "task-1", "executing", None);
    put_task(&store, "task-1", "blocked", Some("verification_failed"));
    let rows = pending(&path);
    assert_eq!(rows.len(), 2);
    assert_ne!(
        rows[0]["event_id"], rows[1]["event_id"],
        "a new attempt is a new episode"
    );
    put_task(&store, "task-2", "failed", None);
    let rows = pending(&path);
    assert_eq!(rows.len(), 3);
    assert_eq!(rows[2]["category"], "unknown");
}

#[test]
fn control_error_pause_enqueues_once_per_pause_episode() {
    let (_tmp, store, path) = store();
    store
        .put(
            "settings",
            "config",
            &json!({"github_repo": "fixture/project", "retain_events": 100}),
        )
        .unwrap();
    enabled(&store);
    let control = |paused: bool, error: Option<&str>| json!({"paused": paused, "error": error, "batch": {"id": "run-9", "phase": "queued", "cycle_id": "cycle-9"}});
    store
        .put("settings", "control", &control(true, Some("boom")))
        .unwrap();
    let rows = pending(&path);
    assert_eq!(rows.len(), 1);
    assert_eq!(rows[0]["category"], "service_error_paused");
    assert_eq!(rows[0]["action"], "inspect_service");
    assert_eq!(rows[0]["repository"], "fixture/project");
    assert_eq!(rows[0]["run_id"], "run-9");
    assert_eq!(rows[0]["cycle_id"], "cycle-9");
    assert!(rows[0]["task_id"].is_null());
    store
        .put("settings", "control", &control(true, Some("still broken")))
        .unwrap();
    assert_eq!(
        pending(&path).len(),
        1,
        "error changes while paused never duplicate"
    );
    store
        .put("settings", "control", &control(false, None))
        .unwrap();
    store
        .put("settings", "control", &control(true, Some("again")))
        .unwrap();
    assert_eq!(pending(&path).len(), 2);
}

#[test]
fn enqueue_rolls_back_with_the_failed_record_write() {
    let (_tmp, store, path) = store();
    enabled(&store);
    let mut connection = rusqlite::Connection::open(&path).unwrap();
    let transaction = connection.transaction().unwrap();
    transaction
        .execute(
            "INSERT INTO records VALUES ('task','task-rollback',?1)",
            [r#"{"id":"task-rollback","cycle_id":"c","status":"blocked","blocked_reason":"timeout","config":{"github_repo":"fixture/project"}}"#],
        )
        .unwrap();
    transaction.rollback().unwrap();
    assert!(pending(&path).is_empty());
}

#[test]
fn configure_preserves_same_destination_and_cancels_on_change() {
    let (_tmp, store, path) = store();
    put_task(&store, "early", "blocked", Some("timeout"));
    enabled(&store);
    assert!(
        pending(&path).is_empty(),
        "enabling never backfills history"
    );
    put_task(&store, "task-1", "blocked", Some("timeout"));
    assert_eq!(pending(&path).len(), 1);
    store
        .configure_notifications(Some(DEST), "enabled", None)
        .unwrap();
    assert_eq!(
        pending(&path).len(),
        1,
        "same destination restart keeps pending"
    );
    store
        .configure_notifications(Some("destination-b"), "enabled", None)
        .unwrap();
    assert!(pending(&path).is_empty());
    let cancelled = outbox(&path, "cancelled");
    assert_eq!(cancelled.len(), 1);
    assert_eq!(cancelled[0]["last_error"], "destination_changed");
    put_task(&store, "task-2", "blocked", Some("timeout"));
    store
        .configure_notifications(None, "disabled", None)
        .unwrap();
    assert!(pending(&path).is_empty(), "disabling cancels pending");
}

#[test]
fn claim_preschedules_five_attempts_and_keeps_payload_frozen() {
    let (_tmp, store, _path) = store();
    enabled(&store);
    put_task(&store, "task-1", "blocked", Some("publication_uncertain"));
    let mut now = Utc::now();
    let delivery = store.claim_notification(DEST, now).unwrap().unwrap();
    assert_eq!(delivery.attempts, 1);
    assert_eq!(delivery.event_id.len(), 32);
    assert!(
        store.claim_notification(DEST, now).unwrap().is_none(),
        "claimed rows reschedule before delivery"
    );
    put_task(&store, "task-1", "blocked", Some("storage_limit"));
    let frozen = notifications::payload(&delivery).unwrap();
    let mut expected = 30;
    for attempt in 2..=5 {
        now += Duration::seconds(expected);
        let next = store.claim_notification(DEST, now).unwrap().unwrap();
        assert_eq!(next.attempts, attempt);
        assert_eq!(next.event_id, delivery.event_id);
        assert_eq!(notifications::payload(&next).unwrap(), frozen);
        expected = [120, 600, 1800, 1800][(attempt - 2) as usize];
    }
    put_task(&store, "task-1", "published", None);
    let later = store
        .claim_notification(DEST, now + Duration::seconds(1800))
        .unwrap();
    assert!(
        later.is_none(),
        "attempt five leaves the row pending until the next claim expires it"
    );
    let health = store.notification_health().unwrap();
    assert_eq!(
        health["failed"], 1,
        "a fifth attempt surfaces as delivery_uncertain"
    );
}

#[test]
fn destination_rotation_never_routes_new_events_to_an_old_worker() {
    let (_tmp, store, _path) = store();
    enabled(&store);
    put_task(&store, "old", "blocked", Some("timeout"));
    store
        .configure_notifications(Some("destination-b"), "enabled", None)
        .unwrap();
    put_task(&store, "new", "blocked", Some("timeout"));
    assert!(
        store
            .claim_notification(DEST, Utc::now())
            .unwrap()
            .is_none()
    );
    let delivery = store
        .claim_notification("destination-b", Utc::now())
        .unwrap()
        .unwrap();
    assert_eq!(delivery.task_id.as_deref(), Some("new"));
    store
        .configure_notifications(None, "invalid", Some("invalid destination"))
        .unwrap();
    let health = store.notification_health().unwrap();
    assert_eq!(health["last_error"], "invalid destination");
    assert_eq!(health["configured"], true);
}

#[tokio::test]
async fn slow_delivery_does_not_cause_a_catch_up_burst() {
    let (_tmp, store, path) = store();
    let app = App::new(store, path.parent().unwrap().into());
    let mut server = receiver(200, std::time::Duration::from_millis(2200)).await;
    let worker = notifications::start(&app, Some(server.url.clone())).unwrap();
    for i in 0..4 {
        put_task(&app.store, &format!("task-{i}"), "blocked", Some("timeout"));
    }
    server.next().await;
    server.next().await;
    let mut previous = std::time::Instant::now();
    for _ in 0..2 {
        server.next().await;
        assert!(previous.elapsed() >= std::time::Duration::from_millis(850));
        previous = std::time::Instant::now();
    }
    app.shutdown.cancel();
    worker.await.unwrap();
}

#[tokio::test]
async fn delivery_timeout_is_bounded_and_visible() {
    let (_tmp, store, path) = store();
    let app = App::new(store, path.parent().unwrap().into());
    let mut server = receiver(200, std::time::Duration::from_secs(60)).await;
    let worker = notifications::start(&app, Some(server.url.clone())).unwrap();
    put_task(&app.store, "task", "blocked", Some("timeout"));
    server.next().await;
    assert!(
        wait_until(std::time::Duration::from_secs(15), || {
            app.store.notification_health().unwrap()["last_error"] == "timeout"
        })
        .await,
        "the bounded delivery timeout was never reported on the destination"
    );
    app.shutdown.cancel();
    worker.await.unwrap();
}

#[test]
fn recovery_and_guard_failures_generate_attention() {
    use octomus_agent::{
        config::Config,
        model::{Task, id, now},
    };
    let (_tmp, store, path) = store();
    let app = App::new(store.clone(), path.parent().unwrap().into());
    enabled(&store);
    let task: Task = serde_json::from_value(json!({
        "id":id(),"cycle_id":"cycle","proposal":{"id":"p","title":"T","problem":"P","evidence":[],"benefit":"B","category":"tests","target":"main","tier":"M","scope":"S","dependencies":[],"prompt":"P","decision":"accepted","reason":"R"},
        "status":"executing","config":Config {github_repo:"fixture/project".into(),..Config::default()},"route":{"model":"fixture","effort":"low"},
        "source_revision":"s","comparison_base":"s","default_revision":"s","branch":"tyk/task","workspace":"","sessions":[],"reviews":[],"verification":[],"attempts":0,"created_at":now(),"updated_at":now()
    })).unwrap();
    store.put("task", &task.id, &task).unwrap();
    app.recover().unwrap();
    assert_eq!(pending(&path).len(), 1);
    let mut guarded = task.clone();
    guarded.id = id();
    store.put("task", &guarded.id, &guarded).unwrap();
    drop(app.task_guard(&guarded.id));
    assert_eq!(pending(&path).len(), 2);
    app.recover().unwrap();
    assert_eq!(pending(&path).len(), 2);
}

#[test]
fn oversized_identities_fail_as_invalid_payload_instead_of_truncating() {
    let (_tmp, store, _path) = store();
    enabled(&store);
    let long = "x".repeat(300);
    store
        .put(
            "task",
            &long,
            &json!({"id": long, "cycle_id": "c", "status": "blocked", "blocked_reason": "timeout", "config": {"github_repo": "fixture/project"}}),
        )
        .unwrap();
    let delivery = store.claim_notification(DEST, Utc::now()).unwrap().unwrap();
    let event = notifications::payload(&delivery);
    assert!(event.is_err());
    store
        .finish_notification_failure(delivery.seq, "invalid_payload", None, false)
        .unwrap();
    assert_eq!(store.notification_health().unwrap()["failed"], 1);
}

#[test]
fn overflow_caps_pending_at_one_thousand() {
    let (_tmp, store, path) = store();
    enabled(&store);
    let mut connection = rusqlite::Connection::open(&path).unwrap();
    let transaction = connection.transaction().unwrap();
    for index in 0..1005 {
        transaction
            .execute(
                "INSERT INTO records VALUES ('task',?1,?2)",
                rusqlite::params![
                    format!("task-{index}"),
                    format!(r#"{{"id":"task-{index}","cycle_id":"c","status":"blocked","blocked_reason":"timeout","config":{{"github_repo":"fixture/project"}}}}"#)
                ],
            )
            .unwrap();
    }
    transaction.commit().unwrap();
    let pending = pending(&path);
    let failed = outbox(&path, "failed");
    assert_eq!(pending.len(), 1000);
    assert_eq!(failed.len(), 5);
    assert!(
        failed
            .iter()
            .all(|row| row["last_error"] == "queue_overflow")
    );
    let ids: Vec<&str> = failed
        .iter()
        .filter_map(|r| r["task_id"].as_str())
        .collect();
    assert!(ids.contains(&"task-0") && ids.contains(&"task-4"));
}

#[test]
fn claim_expires_day_old_rows_and_prunes_terminal_history() {
    let (_tmp, store, path) = store();
    store
        .put(
            "settings",
            "config",
            &json!({"github_repo": "fixture/project", "retain_events": 3}),
        )
        .unwrap();
    enabled(&store);
    put_task(&store, "old", "blocked", Some("timeout"));
    let connection = rusqlite::Connection::open(&path).unwrap();
    connection
        .execute(
            "UPDATE notification_outbox SET created_at=strftime('%Y-%m-%dT%H:%M:%fZ','now','-2 days') WHERE task_id='old'",
            [],
        )
        .unwrap();
    assert!(
        store
            .claim_notification(DEST, Utc::now())
            .unwrap()
            .is_none()
    );
    assert_eq!(outbox(&path, "failed")[0]["last_error"], "expired");
    for index in 0..5 {
        put_task(&store, &format!("t{index}"), "blocked", Some("timeout"));
        let delivery = store.claim_notification(DEST, Utc::now()).unwrap().unwrap();
        store
            .finish_notification_failure(delivery.seq, "transport_error", None, false)
            .unwrap();
    }
    assert!(
        store
            .claim_notification(DEST, Utc::now())
            .unwrap()
            .is_none()
    );
    let connection = rusqlite::Connection::open(&path).unwrap();
    let terminal: i64 = connection
        .query_row(
            "SELECT COUNT(*) FROM notification_outbox WHERE status!='pending'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(terminal, 3, "terminal history prunes to retain_events");
}

struct Receiver {
    url: String,
    received: tokio::sync::mpsc::UnboundedReceiver<String>,
    handle: tokio::task::JoinHandle<()>,
}
impl Drop for Receiver {
    fn drop(&mut self) {
        self.handle.abort();
    }
}
impl Receiver {
    async fn next(&mut self) -> String {
        tokio::time::timeout(std::time::Duration::from_secs(15), self.received.recv())
            .await
            .unwrap()
            .unwrap()
    }
}
async fn receiver(status: u16, first_delay: std::time::Duration) -> Receiver {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = listener.local_addr().unwrap().port();
    let (sender, received) = tokio::sync::mpsc::unbounded_channel();
    let handle = tokio::spawn(async move {
        let mut delay = first_delay;
        loop {
            let (mut stream, _) = listener.accept().await.unwrap();
            let request = async {
                let mut buffer = Vec::new();
                let mut chunk = [0; 8192];
                loop {
                    if let Some(end) = buffer.windows(4).position(|w| w == b"\r\n\r\n") {
                        let header_end = end + 4;
                        let headers = String::from_utf8_lossy(&buffer[..header_end]);
                        let length = headers
                            .lines()
                            .find_map(|line| {
                                line.to_ascii_lowercase()
                                    .strip_prefix("content-length:")
                                    .and_then(|v| v.trim().parse::<usize>().ok())
                            })
                            .unwrap_or(0);
                        if buffer.len() >= header_end + length {
                            return String::from_utf8_lossy(&buffer[..header_end + length])
                                .into_owned();
                        }
                    }
                    assert!(buffer.len() <= 16384);
                    let n = stream.read(&mut chunk).await.unwrap();
                    assert!(n > 0);
                    buffer.extend_from_slice(&chunk[..n]);
                }
            };
            let text = tokio::time::timeout(std::time::Duration::from_secs(15), request)
                .await
                .unwrap();
            if sender.send(text).is_err() {
                return;
            }
            tokio::time::sleep(delay).await;
            delay = std::time::Duration::ZERO;
            let response =
                format!("HTTP/1.1 {status} X\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
            let _ = stream.write_all(response.as_bytes()).await;
        }
    });
    Receiver {
        url: format!("http://127.0.0.1:{port}/hook"),
        received,
        handle,
    }
}

#[tokio::test]
async fn local_receiver_verifies_minimal_payload_and_delivery() {
    let tmp = tempfile::tempdir().unwrap();
    let data = data_dir(&tmp);
    let app = App::new(Store::open(&data.join("state.db")).unwrap(), data);
    let mut server = receiver(200, std::time::Duration::ZERO).await;
    let worker = notifications::start(&app, Some(server.url.clone())).unwrap();
    put_task(&app.store, "task-1", "blocked", Some("stale_base"));
    let request = server.next().await;
    assert!(
        request.contains("content-type: application/json")
            || request.contains("Content-Type: application/json")
    );
    let body: Value = serde_json::from_str(request.rsplit('\n').next().unwrap()).unwrap();
    assert_eq!(body["schema_version"], 1);
    assert_eq!(body["category"], "stale_base");
    assert_eq!(body["action"], "inspect_task");
    assert_eq!(body["task_id"], "task-1");
    assert_eq!(body["repository"], "fixture/project");
    let keys: std::collections::BTreeSet<&str> = body
        .as_object()
        .unwrap()
        .keys()
        .map(String::as_str)
        .collect();
    assert_eq!(
        keys,
        [
            "schema_version",
            "event_id",
            "occurred_at",
            "repository",
            "cycle_id",
            "run_id",
            "task_id",
            "category",
            "action"
        ]
        .into_iter()
        .collect()
    );
    assert!(
        wait_until(std::time::Duration::from_secs(5), || {
            app.store.notification_health().unwrap()["last_delivered_at"].is_string()
        })
        .await,
        "the delivered notification was never recorded"
    );
    let health = app.store.notification_health().unwrap();
    assert_eq!(health["pending"], 0);
    assert!(health["last_delivered_at"].is_string());
    worker.abort();
}

#[tokio::test]
async fn retryable_and_terminal_statuses_are_classified() {
    for (status, retryable) in [
        (408u16, true),
        (429, true),
        (503, true),
        (404, false),
        (302, false),
    ] {
        let tmp = tempfile::tempdir().unwrap();
        let data = data_dir(&tmp);
        let db = data.join("state.db");
        let app = App::new(Store::open(&db).unwrap(), data);
        let mut server = receiver(status, std::time::Duration::ZERO).await;
        let worker = notifications::start(&app, Some(server.url.clone())).unwrap();
        put_task(&app.store, "task-1", "blocked", Some("timeout"));
        server.next().await;
        let recorded = || {
            pending(&db)
                .into_iter()
                .chain(outbox(&db, "failed"))
                .any(|row| row["http_status"] == status)
        };
        assert!(
            wait_until(std::time::Duration::from_secs(5), recorded).await,
            "HTTP {status} was never recorded in the outbox"
        );
        let rows = pending(&db);
        let failed = outbox(&db, "failed");
        if retryable {
            assert_eq!(rows.len(), 1, "HTTP {status} must retry");
            assert_eq!(rows[0]["http_status"], status);
        } else {
            assert!(rows.is_empty(), "HTTP {status} must terminate");
            assert_eq!(failed[0]["http_status"], status);
        }
        worker.abort();
    }
}

#[tokio::test]
async fn held_http_does_not_block_scheduling_and_shutdown_recovers_the_row() {
    let mut server = receiver(200, std::time::Duration::from_secs(60)).await;
    let tmp = tempfile::tempdir().unwrap();
    let data = data_dir(&tmp);
    let db = data.join("state.db");
    let app = App::new(Store::open(&db).unwrap(), data);
    let worker = notifications::start(&app, Some(server.url.clone())).unwrap();
    put_task(&app.store, "task-1", "blocked", Some("timeout"));
    server.next().await;
    let router = octomus_agent::api::router(app.clone(), "fixture-token", None);
    for (method, path) in [("GET", "/api/state"), ("POST", "/api/control/pause")] {
        let request = api_request(method, path, Some("fixture-token"));
        let response = tokio::time::timeout(
            std::time::Duration::from_secs(2),
            router.clone().oneshot(request),
        )
        .await
        .unwrap()
        .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::OK);
    }
    assert_eq!(app.store.notification_health().unwrap()["pending"], 1);
    app.shutdown.cancel();
    worker.await.unwrap();
    let destination: String = rusqlite::Connection::open(&db)
        .unwrap()
        .query_row(
            "SELECT destination_id FROM notification_policy WHERE id=1",
            [],
            |r| r.get(0),
        )
        .unwrap();
    let before = pending(&db);
    let delivery = app
        .store
        .claim_notification(&destination, Utc::now() + Duration::seconds(31))
        .unwrap()
        .expect("an abandoned in-flight row retries");
    assert_eq!(
        delivery.event_id,
        before[0]["event_id"].as_str().unwrap(),
        "recovery after an ambiguous send keeps the same event id"
    );
}

#[test]
fn webhook_env_is_removed_from_children_and_redacted() {
    let command = octomus_agent::process::command("true", std::path::Path::new("/"));
    let removed = command
        .as_std()
        .get_envs()
        .any(|(key, value)| key == std::ffi::OsStr::new(WEBHOOK_ENV) && value.is_none());
    assert!(removed, "child processes must not inherit the webhook URL");
}
