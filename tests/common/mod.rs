//! Support shared by the integration tests. Every test crate compiles this module
//! separately, so helpers a given crate does not use are expected.
#![allow(dead_code)]
use octomus_agent::{
    config::{Config, Route},
    model::{Task, id, now},
};
use serde_json::{Value, json};
use std::time::Duration;
use tower::ServiceExt;

pub const TOKEN: &str = "operator-fixture-token-with-at-least-32-characters";

/// A queued default-branch task with a grounded accepted proposal. Callers layer
/// their own scenario on top with `task_with`.
pub fn task() -> Task {
    serde_json::from_value(json!({
        "id":id(),"cycle_id":"cycle","proposal":{"id":"a","title":"Concrete improvement","problem":"Missing behavior","benefit":"Useful behavior","scope":"one file","evidence":["README.md"],"category":"features","target":"main","tier":"M","dependencies":[],"prompt":"Implement the documented behavior","decision":"accepted","reason":"Grounded","problem_key":"","relevant_paths":[],"reconsiders":[]},
        "status":"queued","route":Route::new("fixture","low"),"config":Config {github_repo:"fixture/project".into(),..Config::default()},
        "source_revision":"source","comparison_base":"source","default_revision":"source","branch":"octomus/work","workspace":"","execution_session":null,"repair_session":null,"sessions":[],"reviews":[],"verification":[],"output_commit":null,"pr_number":null,"pr_url":null,"attempts":0,"error":null,"created_at":now(),"updated_at":now()
    })).unwrap()
}

pub fn task_with(edit: impl FnOnce(&mut Task)) -> Task {
    let mut task = task();
    edit(&mut task);
    task
}

/// The fixture repository every test's stored configuration names.
pub fn config() -> Config {
    Config {
        github_repo: "fixture/project".into(),
        ..Default::default()
    }
}

/// A POST under `/api` carrying the token the caller's router was built with.
pub fn control_request(path: &str, token: &str) -> axum::http::Request<axum::body::Body> {
    api_request("POST", &format!("/api/{path}"), Some(token))
}

/// A JSON request. The auth header is optional because tests that exercise
/// authentication deliberately omit it.
pub fn api_request(
    method: &str,
    path: &str,
    token: Option<&str>,
) -> axum::http::Request<axum::body::Body> {
    let mut builder = axum::http::Request::builder()
        .uri(path)
        .method(method)
        .header("content-type", "application/json");
    if let Some(token) = token {
        builder = builder.header("authorization", format!("Bearer {token}"));
    }
    builder.body(axum::body::Body::empty()).unwrap()
}

/// Sends one request through the router with the module's token and decodes the
/// JSON body, if any.
pub async fn request(
    router: &axum::Router,
    method: &str,
    path: &str,
    body: axum::body::Body,
) -> (axum::http::StatusCode, Value) {
    let response = router
        .clone()
        .oneshot(
            axum::http::Request::builder()
                .uri(path)
                .method(method)
                .header("authorization", format!("Bearer {TOKEN}"))
                .header("content-type", "application/json")
                .body(body)
                .unwrap(),
        )
        .await
        .unwrap();
    let status = response.status();
    let bytes = axum::body::to_bytes(response.into_body(), 4 * 1024 * 1024)
        .await
        .unwrap();
    (
        status,
        serde_json::from_slice(&bytes).unwrap_or(Value::Null),
    )
}

/// Polls `ready` every 10 ms until it holds or `timeout` elapses, and reports
/// which happened so the caller can assert with its own label.
pub async fn wait_until(timeout: Duration, mut ready: impl FnMut() -> bool) -> bool {
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if ready() {
            return true;
        }
        if tokio::time::Instant::now() >= deadline {
            return false;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

/// `wait_until` for probes that have to await.
pub async fn wait_until_async<F, Fut>(timeout: Duration, mut ready: F) -> bool
where
    F: FnMut() -> Fut,
    Fut: Future<Output = bool>,
{
    let deadline = tokio::time::Instant::now() + timeout;
    loop {
        if ready().await {
            return true;
        }
        if tokio::time::Instant::now() >= deadline {
            return false;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}
