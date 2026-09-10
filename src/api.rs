use crate::{
    config::{Backend, Config, validate_binary},
    engine::App,
    model::{Cycle, CycleMode, Status, Task},
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
    let tasks = s.app.store.list::<Task>("task")?;
    let cycles = s.app.store.list::<Cycle>("cycle")?;
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
    // Configuration and complete task details are fetched separately to keep polling inexpensive.
    Ok(Json(
        json!({"status":status,"control":c,"repository":config.github_repo,"configured":config.validate(true).is_ok(),"audit_configured":config.validate_audit().is_ok(),"active_cycle_mode":rt.cycle_mode,"active_tasks":rt.tasks.len(),"cycle_active":rt.cycle.is_some(),"sessions_today":s.app.store.sessions_today()?,"session_limit":config.max_sessions_per_day,"tasks":tasks.iter().take(300).map(|t|json!({"id":t.id,"cycle_id":t.cycle_id,"title":t.proposal.title,"category":t.proposal.category,"tier":t.proposal.tier,"target":t.proposal.target,"branch":t.branch,"status":t.status,"pr_url":t.pr_url,"pr_number":t.pr_number,"error":t.error,"created_at":t.created_at,"updated_at":t.updated_at})).collect::<Vec<_>>(),"cycles":cycles.into_iter().take(20).collect::<Vec<_>>(),"prs":s.app.store.get::<Value>("settings","prs")?.unwrap_or(json!([])),"events":s.app.store.events(None)?}),
    ))
}
async fn task(State(s): State<Api>, Path(id): Path<String>) -> Result<Json<Task>> {
    Ok(Json(s.app.store.get("task", &id)?.ok_or(ApiError(
        StatusCode::NOT_FOUND,
        "Task not found".into(),
    ))?))
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
        || old.github_repo != c.github_repo
        || old.branch_prefix != c.branch_prefix
        || old.default_branch != c.default_branch)
        && s.app
            .store
            .list::<Task>("task")?
            .iter()
            .any(|t| !matches!(t.status, Status::Published | Status::Cancelled))
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
    if (action == "audit" && (!c.paused || !rt.tasks.is_empty() || rt.cycle.is_some()))
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
        "pause" => c.paused = true,
        "resume" => {
            s.app.config()?.validate(true)?;
            c.paused = false;
            c.error = None;
        }
        "cycle" => {
            s.app.config()?.validate(true)?;
            c.paused = false;
            c.next_cycle_at = 0;
            c.error = None;
        }
        _ => return Err(ApiError(StatusCode::NOT_FOUND, "Unknown control".into())),
    }
    s.app.store.put("settings", "control", &c)?;
    s.app.store.event("system", "operator", &action)?;
    Ok(Json(json!(c)))
}
async fn task_action(
    State(s): State<Api>,
    Path((id, action)): Path<(String, String)>,
) -> Result<Json<Value>> {
    let _gate = s.app.gate.lock().await;
    let mut t: Task = s
        .app
        .store
        .get("task", &id)?
        .ok_or(ApiError(StatusCode::NOT_FOUND, "Task not found".into()))?;
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
            t.attempts += 1;
            t.error = None;
            t.status = Status::Queued;
            // Keep original target, prompt, routes and verification contract; allow operators to raise limits.
            t.config.max_repair_rounds = config.max_repair_rounds;
            t.config.max_no_progress_rounds = config.max_no_progress_rounds;
            t.config.task_timeout_seconds = config.task_timeout_seconds;
            t.config.session_timeout_seconds = config.session_timeout_seconds;
            t.config.max_sessions_per_day = config.max_sessions_per_day;
            t.config.max_workspace_bytes = config.max_workspace_bytes;
            s.app.save_task(&mut t)?;
        }
        _ => {
            return Err(ApiError(
                StatusCode::NOT_FOUND,
                "Unknown task action".into(),
            ));
        }
    }
    s.app.store.event(&id, "operator", &action)?;
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
