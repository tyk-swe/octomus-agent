use crate::{
    config::{Backend, Config, validate_binary},
    engine::App,
    model::{AttemptPolicy, BlockedReason, Cycle, CycleMode, OperatingMode, Status, Task},
    store::redact,
};
use axum::{
    Json, Router,
    extract::{Path, Query, Request, State},
    http::{HeaderValue, StatusCode},
    middleware::{self, Next},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::{
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use subtle::ConstantTimeEq;
use tower_http::services::{ServeDir, ServeFile};

#[derive(Clone)]
pub struct Api {
    pub app: App,
    pub token_hash: Arc<[u8; 32]>,
    failures: Arc<Mutex<AuthFailures>>,
}
#[derive(Default)]
struct AuthFailures {
    count: u32,
    last: Option<Instant>,
}
impl AuthFailures {
    fn delay(&mut self, now: Instant) -> Duration {
        if self
            .last
            .is_none_or(|last| now.duration_since(last) >= Duration::from_secs(60))
        {
            self.count = 0;
        }
        let delay = Duration::from_millis((100u64 << self.count.min(4)).min(1000));
        self.count = self.count.saturating_add(1);
        self.last = Some(now);
        delay
    }
}
pub struct ApiError(pub StatusCode, pub String);
impl From<anyhow::Error> for ApiError {
    fn from(e: anyhow::Error) -> Self {
        Self(StatusCode::BAD_REQUEST, redact(&format!("{e:#}")))
    }
}
impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        (self.0, Json(json!({"error":self.1}))).into_response()
    }
}
type Result<T> = std::result::Result<T, ApiError>;
pub fn router(app: App, token: &str, assets: Option<PathBuf>) -> Router {
    let state = Api {
        app,
        failures: Arc::default(),
        token_hash: Arc::new(Sha256::digest(token.as_bytes()).into()),
    };
    let api = Router::new()
        .route("/state", get(state_view))
        .route("/tasks", get(task_history))
        .route("/cycles", get(cycle_history))
        .route("/cycles/{id}", get(cycle_detail))
        .route("/cycles/{id}/{action}", post(cycle_action))
        .route("/proposals", get(proposal_history))
        .route("/proposals/{cycle}/{id}", get(proposal_detail))
        .route("/prs", get(pr_history))
        .route("/tasks/{id}", get(task))
        .route("/tasks/{id}/{action}", post(task_action))
        .route("/config", get(config).put(save_config))
        .route("/control/{action}", post(control))
        .route("/doctor", post(doctor))
        .route("/models", get(models))
        .route("/model-catalog", post(model_catalog))
        .route("/events", get(events))
        .fallback(|| async {
            (
                StatusCode::NOT_FOUND,
                Json(json!({"error":"Unknown API route"})),
            )
        })
        .route_layer(middleware::from_fn_with_state(state.clone(), authenticate))
        .with_state(state.clone());
    let router = Router::new().nest("/api", api).route(
        "/healthz",
        get(|| async { Json(json!({"ok":true,"version":env!("CARGO_PKG_VERSION")})) }),
    );
    let router = if let Some(assets) = assets {
        router.fallback_service(
            ServeDir::new(&assets).not_found_service(ServeFile::new(assets.join("200.html"))),
        )
    } else {
        router.fallback(crate::assets::serve)
    };
    router
        .layer(axum::extract::DefaultBodyLimit::max(256 * 1024))
        .layer(middleware::from_fn(headers))
}
async fn headers(req: Request, next: Next) -> Response {
    let mut response = next.run(req).await;
    let h = response.headers_mut();
    h.insert(
        "x-content-type-options",
        HeaderValue::from_static("nosniff"),
    );
    h.insert("x-frame-options", HeaderValue::from_static("DENY"));
    h.insert("referrer-policy", HeaderValue::from_static("no-referrer"));
    h.insert("cache-control", HeaderValue::from_static("no-store"));
    h.insert("content-security-policy",HeaderValue::from_static("default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"));
    response
}
async fn authenticate(State(s): State<Api>, req: Request, next: Next) -> Response {
    let token = req
        .headers()
        .get("authorization")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or("");
    let digest: [u8; 32] = Sha256::digest(token.as_bytes()).into();
    if !bool::from(digest.ct_eq(s.token_hash.as_ref())) {
        let delay = s.failures.lock().unwrap().delay(Instant::now());
        tokio::time::sleep(delay).await;
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({"error":"Enter the operator access token to connect."})),
        )
            .into_response();
    }
    // Browser mutations require a non-simple content type. No CORS policy is enabled.
    if req.method() != axum::http::Method::GET
        && !req
            .headers()
            .get("content-type")
            .is_some_and(|v| v.as_bytes().starts_with(b"application/json"))
    {
        return (
            StatusCode::UNSUPPORTED_MEDIA_TYPE,
            Json(json!({"error":"Use application/json"})),
        )
            .into_response();
    }
    let response = next.run(req).await;
    if response
        .headers()
        .get("content-type")
        .is_some_and(|v| v.as_bytes().starts_with(b"application/json"))
    {
        let (mut parts, body) = response.into_parts();
        let sanitized = async {
            let bytes = axum::body::to_bytes(body, 32 * 1024 * 1024).await?;
            let mut value: Value = serde_json::from_slice(&bytes)?;
            crate::store::redact_json(&mut value);
            Ok::<_, anyhow::Error>(serde_json::to_vec(&value)?)
        }
        .await;
        return match sanitized {
            Ok(body) => { parts.headers.remove("content-length"); Response::from_parts(parts, axum::body::Body::from(body)) },
            Err(_) => (StatusCode::INTERNAL_SERVER_ERROR, Json(json!({"error":"Response exceeded the dashboard size limit or could not be encoded"}))).into_response()
        };
    }
    response
}
async fn state_view(State(s): State<Api>) -> Result<Json<Value>> {
    let c = s.app.control()?;
    let mut snapshot = s.app.store.dashboard()?;
    let config = s.app.config()?;
    let rt = s.app.runtime.lock().unwrap();
    let status = if rt.cycle_mode == Some(CycleMode::Audit) {
        "auditing"
    } else if c.paused {
        "paused"
    } else if c.error.is_some() {
        "unhealthy"
    } else if rt.cycle.is_some() || !rt.tasks.is_empty() {
        "running"
    } else {
        "idle"
    };
    let fields = json!({"status":status,"control":c,"repository":config.github_repo,"configured":config.validate(true).is_ok(),"audit_configured":config.validate_audit().is_ok(),"active_cycle_mode":rt.cycle_mode,"active_tasks":rt.tasks.len(),"cycle_active":rt.cycle.is_some(),"session_limit":config.max_sessions_per_day,"storage_limit":config.max_workspace_bytes,"storage":s.app.store.get::<Value>("settings","storage")?});
    snapshot
        .as_object_mut()
        .unwrap()
        .extend(fields.as_object().unwrap().clone());
    Ok(Json(snapshot))
}
async fn task(State(s): State<Api>, Path(id): Path<String>) -> Result<Json<Value>> {
    let task: Task = s
        .app
        .store
        .get("task", &id)?
        .ok_or(ApiError(StatusCode::NOT_FOUND, "Task not found".into()))?;
    let mut value = serde_json::to_value(&task).map_err(anyhow::Error::from)?;
    value["allowed_actions"] = json!(task.allowed_actions());
    value["effective_attempt_policy"] = json!(AttemptPolicy::from_config(&task.execution_config()));
    let live = s.app.config()?;
    value["operating_policy"] = json!({"max_sessions_per_day":live.max_sessions_per_day,"max_workspace_bytes":live.max_workspace_bytes});
    Ok(Json(value))
}
async fn task_history(
    State(s): State<Api>,
    Query(q): Query<crate::store::HistoryQuery>,
) -> Result<Json<crate::store::Page>> {
    Ok(Json(s.app.store.history_page("task", &q)?))
}
async fn cycle_history(
    State(s): State<Api>,
    Query(q): Query<crate::store::HistoryQuery>,
) -> Result<Json<crate::store::Page>> {
    Ok(Json(s.app.store.history_page("cycle", &q)?))
}
async fn proposal_history(
    State(s): State<Api>,
    Query(q): Query<crate::store::HistoryQuery>,
) -> Result<Json<crate::store::Page>> {
    Ok(Json(s.app.store.proposal_page(&q)?))
}
async fn proposal_detail(
    State(s): State<Api>,
    Path((cycle, id)): Path<(String, String)>,
) -> Result<Json<Value>> {
    Ok(Json(s.app.store.proposal_detail(&cycle, &id)?.ok_or(
        ApiError(StatusCode::NOT_FOUND, "Proposal not found".into()),
    )?))
}
async fn pr_history(
    State(s): State<Api>,
    Query(q): Query<crate::store::HistoryQuery>,
) -> Result<Json<crate::store::Page>> {
    Ok(Json(s.app.store.history_page("pr", &q)?))
}
async fn cycle_detail(State(s): State<Api>, Path(id): Path<String>) -> Result<Json<Cycle>> {
    Ok(Json(s.app.store.get("cycle", &id)?.ok_or(ApiError(
        StatusCode::NOT_FOUND,
        "Cycle not found".into(),
    ))?))
}
async fn cycle_action(
    State(s): State<Api>,
    Path((id, action)): Path<(String, String)>,
) -> Result<Json<Value>> {
    let _gate = s.app.gate.lock().await;
    let mut c: Cycle = s
        .app
        .store
        .get("cycle", &id)?
        .ok_or(ApiError(StatusCode::NOT_FOUND, "Cycle not found".into()))?;
    if c.status == "running" {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "Wait for planning to finish".into(),
        ));
    }
    match action.as_str() {
        "archive" => {
            c.lifecycle.archived_at = Some(crate::model::now());
            s.app.store.put("cycle", &id, &c)?;
        }
        "discard" if c.lifecycle.archived_at.is_some() => s.app.discard_cycle(&mut c).await?,
        _ => {
            return Err(ApiError(
                StatusCode::CONFLICT,
                "Archive the cycle before discarding its workspace".into(),
            ));
        }
    }
    s.app.store.event(&id, "operator", &action)?;
    Ok(Json(json!({"ok":true})))
}
async fn config(State(s): State<Api>) -> Result<Json<Config>> {
    Ok(Json(s.app.config()?))
}
async fn save_config(State(s): State<Api>, Json(c): Json<Config>) -> Result<Json<Value>> {
    let _gate = s.app.gate.lock().await;
    let rt = s.app.runtime.lock().unwrap();
    if !s.app.control()?.paused || !rt.tasks.is_empty() || rt.cycle.is_some() {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "Pause and wait for active work to finish before changing configuration.".into(),
        ));
    }
    c.validate(false)?;
    let old = s.app.config()?;
    if (old.repository != c.repository
        || !old.github_repo.eq_ignore_ascii_case(&c.github_repo)
        || old.branch_prefix != c.branch_prefix
        || old.default_branch != c.default_branch)
        && s.app.store.has_unresolved_tasks()?
    {
        return Err(ApiError(StatusCode::CONFLICT,"Resolve or cancel existing tasks before changing repository identity or branch policy.".into()));
    }
    s.app.store.put("settings", "config", &c)?;
    s.app
        .store
        .event("system", "configuration", "Operator saved configuration")?;
    Ok(Json(json!({"ok":true})))
}
async fn control(State(s): State<Api>, Path(action): Path<String>) -> Result<Json<Value>> {
    let _gate = s.app.gate.lock().await;
    let mut c = s.app.control()?;
    let rt = s.app.runtime.lock().unwrap();
    if (matches!(action.as_str(), "audit" | "cycle")
        && (!c.paused || !rt.tasks.is_empty() || rt.cycle.is_some()))
        || (matches!(action.as_str(), "resume" | "cycle")
            && rt.cycle_mode == Some(CycleMode::Audit))
    {
        return Err(ApiError(StatusCode::CONFLICT, "Audits require paused operation with no active work; wait for the audit to finish before resuming.".into()));
    }
    drop(rt);
    match action.as_str() {
        "audit" => {
            s.app.start_audit()?;
            s.app.store.event("system", "operator", "audit")?;
            return Ok(Json(json!(s.app.control()?)));
        }
        "pause" => c.set_mode(OperatingMode::Paused),
        "resume" => {
            s.app.config()?.validate(true)?;
            c.set_mode(OperatingMode::Continuous);
            c.error = None;
        }
        "cycle" => {
            s.app.config()?.validate(true)?;
            s.app.store.start_batch(&mut c)?;
        }
        _ => return Err(ApiError(StatusCode::NOT_FOUND, "Unknown control".into())),
    }
    s.app.store.put("settings", "control", &c)?;
    s.app.store.event("system", "operator", &action)?;
    Ok(Json(json!(c)))
}
// Caller holds the scheduler gate, including when rechecking after remote work.
fn eligible_task(app: &App, id: &str, action: &str) -> Result<Task> {
    let t: Task = app
        .store
        .get("task", id)?
        .ok_or(ApiError(StatusCode::NOT_FOUND, "Task not found".into()))?;
    if !t.allowed_actions().contains(&action) {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "This action is not eligible for the task's recorded failure and workspace state"
                .into(),
        ));
    }
    if action != "cancel" && app.runtime.lock().unwrap().tasks.contains_key(id) {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "Wait for active task work to finish".into(),
        ));
    }
    Ok(t)
}
fn revalidate_task_action(app: &App, task: &Task, action: &str) -> Result<()> {
    let current = eligible_task(app, &task.id, action)?;
    if json!(current) != json!(task) {
        return Err(ApiError(
            StatusCode::CONFLICT,
            "Task changed during remote checks; inspect its current state before trying again"
                .into(),
        ));
    }
    Ok(())
}
async fn task_action(
    State(s): State<Api>,
    Path((id, action)): Path<(String, String)>,
) -> Result<Json<Value>> {
    let mut gate = s.app.gate.lock().await;
    let mut t = eligible_task(&s.app, &id, &action)?;
    match action.as_str() {
        "cancel" => {
            let rt = s.app.runtime.lock().unwrap();
            if let Some(cancel) = rt.tasks.get(&id) {
                if t.status == Status::Publishing {
                    return Err(ApiError(
                        StatusCode::CONFLICT,
                        "Publication is in progress; wait for reconciliation before cancelling."
                            .into(),
                    ));
                }
                cancel.cancel();
            } else if t.status != Status::Published {
                t.status = Status::Cancelled;
                s.app.save_task(&mut t)?;
            } else {
                return Err(ApiError(
                    StatusCode::CONFLICT,
                    "Published tasks cannot be cancelled.".into(),
                ));
            }
        }
        "retry" => {
            if !t.status.retryable() {
                return Err(ApiError(
                    StatusCode::CONFLICT,
                    "Only failed or blocked tasks can be retried.".into(),
                ));
            }
            let config = s.app.config()?;
            if t.attempts >= config.max_retries {
                return Err(ApiError(
                    StatusCode::CONFLICT,
                    "Retry limit reached. Adjust the operating limit after inspecting the failure."
                        .into(),
                ));
            }
            let original = t.clone();
            t.attempt_policy = Some(AttemptPolicy::from_config(&config));
            if t.output_commit.is_none() {
                drop(gate);
                let preflight = s
                    .app
                    .retry_preflight(&t, &s.app.shutdown.child_token())
                    .await;
                gate = s.app.gate.lock().await;
                revalidate_task_action(&s.app, &original, &action)?;
                if t.attempt_policy.as_ref() != Some(&AttemptPolicy::from_config(&s.app.config()?))
                {
                    return Err(ApiError(
                        StatusCode::CONFLICT,
                        "Retry policy changed during remote checks; try again with the current limits"
                            .into(),
                    ));
                }
                if let Err(error) = preflight {
                    t.blocked_reason = Some(BlockedReason::from_error(&error));
                    t.error = Some(redact(&format!("{error:#}")));
                    s.app.save_task(&mut t)?;
                    return Err(error.into());
                }
            }
            t.attempts += 1;
            // A new attempt starts its repair-round budget from the reviews recorded so far.
            t.review_baseline = t.reviews.len();
            t.error = None;
            t.blocked_reason = None;
            t.status = Status::Queued;
            t.run_id = None;
            s.app.store.event(
                &id,
                "attempt_policy",
                &serde_json::to_string(&t.attempt_policy).map_err(anyhow::Error::from)?,
            )?;
            s.app.save_task(&mut t)?;
        }
        "supersede" => {
            t.status = Status::Cancelled;
            t.rediscovery_requested = true;
            t.rediscovery_result = None;
            s.app.save_task(&mut t)?;
        }
        "archive" => {
            if t.status.retryable() {
                t.status = Status::Cancelled;
            }
            t.lifecycle.archived_at = Some(crate::model::now());
            s.app.save_task(&mut t)?;
        }
        "discard" => s.app.discard_task(&mut t).await?,
        "reconcile" => {
            if !s.app.runtime.lock().unwrap().tasks.is_empty() {
                return Err(ApiError(
                    StatusCode::CONFLICT,
                    "Wait for active tasks before publication reconciliation".into(),
                ));
            }
            if t.output_commit.is_some() {
                let previous_status = t.status.clone();
                s.app.transition(&mut t, Status::Publishing)?;
                let cancel = s.app.shutdown.child_token();
                {
                    let mut rt = s.app.runtime.lock().unwrap();
                    rt.tasks.insert(id.clone(), cancel.clone());
                    rt.reconciling_publication = true;
                }
                let app = s.app.clone();
                // Own completion independently of the HTTP request so a disconnected
                // operator cannot release the reservation while publication is running.
                let work = tokio::spawn(async move {
                    let published = {
                        let limit = Duration::from_secs(t.execution_config().task_timeout_seconds);
                        let publish = crate::git::publish(&t, &cancel);
                        tokio::pin!(publish);
                        match tokio::time::timeout(limit, &mut publish).await {
                            Ok(result) => result,
                            Err(_) => {
                                // Use the normal task deadline cleanup: prevent further
                                // commands and let cancellation stop owned process groups.
                                cancel.cancel();
                                let _ = tokio::time::timeout(Duration::from_secs(8), &mut publish)
                                    .await;
                                Err(anyhow::Error::new(BlockedReason::Timeout))
                            }
                        }
                    };
                    let _gate = app.gate.lock().await;
                    let result = (|| -> Result<()> {
                        match published {
                            Ok(pr) => {
                                t.pr_number = Some(pr.number);
                                t.pr_url = Some(pr.url.clone());
                                t.error = None;
                                t.blocked_reason = None;
                                app.transition(&mut t, Status::Published)?;
                                app.observe_delivery(&t.config, pr)?;
                            }
                            Err(error) => {
                                t.blocked_reason = Some(BlockedReason::from_error(&error));
                                t.error = Some(redact(&format!("{error:#}")));
                                app.transition(&mut t, previous_status)?;
                            }
                        }
                        app.store.event(&id, "operator", &action)?;
                        Ok(())
                    })();
                    let mut rt = app.runtime.lock().unwrap();
                    rt.tasks.remove(&id);
                    rt.reconciling_publication = false;
                    result
                });
                drop(gate);
                work.await.map_err(anyhow::Error::from)??;
                return Ok(Json(json!({"ok":true})));
            } else {
                drop(gate);
                let preflight = s
                    .app
                    .retry_preflight(&t, &s.app.shutdown.child_token())
                    .await;
                gate = s.app.gate.lock().await;
                revalidate_task_action(&s.app, &t, &action)?;
                match preflight {
                    Ok(()) => {
                        t.blocked_reason = Some(BlockedReason::Unknown);
                        t.error =
                            Some("Remote prerequisites are restored; task can be retried".into());
                    }
                    Err(error) => {
                        t.blocked_reason = Some(BlockedReason::from_error(&error));
                        t.error = Some(redact(&format!("{error:#}")));
                    }
                }
                s.app.save_task(&mut t)?;
            }
        }
        _ => {
            return Err(ApiError(
                StatusCode::NOT_FOUND,
                "Unknown task action".into(),
            ));
        }
    }
    s.app.store.event(&id, "operator", &action)?;
    drop(gate);
    Ok(Json(json!({"ok":true})))
}
#[derive(Default, Deserialize)]
struct DoctorQuery {
    #[serde(default)]
    mode: CycleMode,
}
async fn doctor(State(s): State<Api>, Query(q): Query<DoctorQuery>) -> Result<Json<Value>> {
    Ok(Json(s.app.doctor_for(&s.app.config()?, q.mode).await?))
}
async fn models(State(s): State<Api>) -> Result<Json<Value>> {
    let mut cx = crate::codex::Codex::connect(
        &s.app.config()?,
        &s.app.data_dir,
        s.app.store.clone(),
        "system",
        s.app.shutdown.child_token(),
    )
    .await?;
    Ok(Json(json!(cx.models().await?)))
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CatalogRequest {
    backend: Backend,
    binary: String,
}
async fn model_catalog(
    State(s): State<Api>,
    Json(request): Json<CatalogRequest>,
) -> Result<Json<Vec<crate::runner::Model>>> {
    validate_binary(&request.binary)?;
    let mut config = s.app.config()?;
    match request.backend {
        Backend::Codex => config.codex_binary = request.binary,
        Backend::Opencode => config.opencode_binary = request.binary,
    }
    let mut client = crate::runner::Runner::connect(
        request.backend,
        &config,
        &s.app.data_dir,
        s.app.store.clone(),
        "system",
        s.app.shutdown.child_token(),
    )
    .await?;
    Ok(Json(client.models(&s.app.data_dir).await?))
}
#[derive(Deserialize)]
struct EventQuery {
    entity: Option<String>,
}
async fn events(State(s): State<Api>, Query(q): Query<EventQuery>) -> Result<Json<Value>> {
    Ok(Json(json!(s.app.store.events(q.entity.as_deref())?)))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn authentication_backoff_is_bounded_and_expires() {
        let mut failures = AuthFailures::default();
        let now = Instant::now();
        for expected in [100, 200, 400, 800, 1000, 1000] {
            assert_eq!(failures.delay(now), Duration::from_millis(expected));
        }
        assert_eq!(
            failures.delay(now + Duration::from_secs(60)),
            Duration::from_millis(100)
        );
    }
}
