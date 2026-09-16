//! Owned OpenCode HTTP servers. Protocol baseline: OpenCode 1.18.30 (v2 SDK types).
use crate::{
    config::{Backend, Config, Route},
    process::{self, GroupChild},
    runner::{MAX_MESSAGE, Model, WORKER_INSTRUCTIONS},
    store::Store,
};
use anyhow::{Context, Result, bail, ensure};
use futures::StreamExt;
use reqwest::{Client, Method, Response, Url};
use serde_json::{Value, json};
use std::{
    path::Path,
    process::Stdio,
    sync::atomic::{AtomicU64, Ordering},
    time::Duration,
};
use tokio::task::JoinHandle;
use tokio_util::{
    codec::{FramedRead, LinesCodec},
    sync::CancellationToken,
};

pub const PROTOCOL_VERSION: &str = "1.18.30";

pub fn version_warning(version: &str) -> Option<String> {
    (version != PROTOCOL_VERSION).then(|| {
        crate::runner::version_warning(
            Backend::Opencode,
            version,
            &format!("protocol baseline {PROTOCOL_VERSION}. Pin the documented CLI"),
        )
    })
}

pub struct OpenCode {
    _child: GroupChild,
    drain: JoinHandle<()>,
    client: Client,
    url: Url,
    password: String,
    agent: String,
    version: String,
    timeout: u64,
    cancel: CancellationToken,
    store: Store,
    entity: String,
}
impl Drop for OpenCode {
    fn drop(&mut self) {
        self.drain.abort();
        // GroupChild also stops descendants on completion, failure, or cancellation.
    }
}
impl OpenCode {
    pub async fn connect(
        config: &Config,
        cwd: &Path,
        store: Store,
        entity: &str,
        cancel: CancellationToken,
    ) -> Result<Self> {
        ensure!(!cancel.is_cancelled(), "Session cancelled");
        let password = uuid::Uuid::new_v4().to_string();
        // A unique agent cannot inherit a host agent's model, variant, or tool settings.
        let agent = format!("octomus-{}", uuid::Uuid::new_v4().simple());
        let policy = json!({
            "share":"disabled", "autoshare":false, "autoupdate":false,
            "snapshot":false, "lsp":false, "formatter":false,
            "compaction":{"auto":false,"prune":false},
            "default_agent":agent,
            "agent":{
                &agent: {"mode":"primary","prompt":WORKER_INSTRUCTIONS,"permission":{"*":"allow","question":"deny","task":"deny"}},
                "title":{"disable":true},"summary":{"disable":true},"compaction":{"disable":true}
            }
        });
        let mut child = GroupChild::new(
            process::command(&config.opencode_binary, cwd)
                .args(["serve", "--hostname", "127.0.0.1", "--port", "0"])
                .env("OPENCODE_SERVER_USERNAME", "octomus")
                .env("OPENCODE_SERVER_PASSWORD", &password)
                .env("OPENCODE_CONFIG_CONTENT", policy.to_string())
                .env("OPENCODE_DISABLE_PROJECT_CONFIG", "true")
                .env("OPENCODE_DISABLE_AUTOUPDATE", "true")
                .env("OPENCODE_DISABLE_AUTOCOMPACT", "true")
                .env("OPENCODE_DISABLE_TERMINAL_TITLE", "true")
                .stderr(Stdio::null())
                .spawn()
                .context(
                    "Could not start OpenCode; install and configure OpenCode as the service user",
                )?,
        );
        let mut output = FramedRead::new(
            child.0.stdout.take().unwrap(),
            LinesCodec::new_with_max_length(16_384),
        );
        let ready = async {
            for _ in 0..1000 {
                let line = output
                    .next()
                    .await
                    .context("OpenCode exited before server readiness")??;
                if let Some(endpoint) = line.strip_prefix("opencode server listening on ") {
                    let url =
                        Url::parse(endpoint.trim()).context("Invalid OpenCode server address")?;
                    ensure!(
                        url.scheme() == "http"
                            && url.host_str() == Some("127.0.0.1")
                            && url.port().is_some_and(|p| p > 0)
                            && url.username().is_empty()
                            && url.password().is_none()
                            && url.path() == "/"
                            && url.query().is_none()
                            && url.fragment().is_none(),
                        "OpenCode did not bind to a local server address"
                    );
                    return Ok::<_, anyhow::Error>(url);
                }
            }
            bail!("OpenCode exceeded the startup output limit")
        };
        let url = process::bounded(
            config.command_timeout_seconds.min(60),
            &cancel,
            "OpenCode startup timed out",
            ready,
        )
        .await??;
        let client = Client::builder()
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(10))
            .build()?;
        let drain = tokio::spawn(async move {
            // Discard stdout without retaining raw logs. A malformed stream ends the drain.
            while let Some(Ok(_)) = output.next().await {}
        });
        let mut server = Self {
            _child: child,
            drain,
            client,
            url,
            password,
            agent,
            version: String::new(),
            timeout: config.session_timeout_seconds,
            cancel,
            store,
            entity: entity.into(),
        };
        let health = server
            .json(Method::GET, "/global/health", cwd, None, 60)
            .await?;
        ensure!(health["healthy"] == true, "OpenCode is not healthy");
        server.version = health["version"]
            .as_str()
            .filter(|v| v.len() <= 100)
            .context("Missing OpenCode version")?
            .to_owned();
        // Managed host settings can override inline config; do not run with changed policy.
        let effective = server.json(Method::GET, "/config", cwd, None, 60).await?;
        ensure!(
            effective["share"] == "disabled"
                && effective["autoupdate"] == false
                && effective["compaction"]["auto"] == false
                && effective["agent"][&server.agent]["prompt"] == WORKER_INSTRUCTIONS,
            "OpenCode did not apply Octomus unattended policy"
        );
        Ok(server)
    }
    /// Version-specific schema for contract checks, fetched from the owned server.
    pub async fn protocol_schema(&self, cwd: &Path) -> Result<Value> {
        self.json(Method::GET, "/doc", cwd, None, 60).await
    }
    pub fn version(&self) -> &str {
        &self.version
    }
    /// Server version against the documented protocol baseline.
    pub fn diagnostics(&self) -> Value {
        json!({"backend":"opencode","version":self.version(),"protocol_version":PROTOCOL_VERSION,"warning":version_warning(self.version())})
    }

    fn request(&self, method: Method, path: &str, cwd: &Path) -> reqwest::RequestBuilder {
        self.client
            .request(
                method,
                self.url
                    .join(path)
                    .expect("adapter paths are relative HTTP paths"),
            )
            .basic_auth("octomus", Some(&self.password))
            .query(&[("directory", cwd.to_string_lossy().as_ref())])
    }
    async fn json(
        &self,
        method: Method,
        path: &str,
        cwd: &Path,
        body: Option<Value>,
        seconds: u64,
    ) -> Result<Value> {
        let request = self.request(method, path, cwd);
        let request = if let Some(body) = body {
            request.json(&body)
        } else {
            request
        };
        let future = async {
            let mut response = request
                .send()
                .await
                .context("OpenCode HTTP request failed")?;
            if !response.status().is_success() {
                let mut snippet = vec![];
                while snippet.len() < 4096 {
                    match response.chunk().await {
                        Ok(Some(chunk)) => snippet.extend_from_slice(&chunk),
                        _ => break,
                    }
                }
                snippet.truncate(4096);
                bail!(
                    "OpenCode request failed with HTTP {}: {}",
                    response.status(),
                    crate::store::redact(&String::from_utf8_lossy(&snippet))
                );
            }
            read_json(response).await
        };
        process::bounded(seconds, &self.cancel, "OpenCode response timed out", future).await?
    }
    pub async fn models(&self, cwd: &Path) -> Result<Vec<Model>> {
        let value = self.json(Method::GET, "/provider", cwd, None, 60).await?;
        catalog(&value)
    }
    pub async fn start(&self, route: &Route, cwd: &Path, resume: Option<&str>) -> Result<String> {
        route.require_backend(Backend::Opencode)?;
        let permissions = json!([
            {"permission":"*","pattern":"*","action":"allow"},
            {"permission":"question","pattern":"*","action":"deny"},
            {"permission":"task","pattern":"*","action":"deny"}
        ]);
        let session = if let Some(id) = resume {
            self.json(
                Method::GET,
                &format!("/session/{}", segment(id)?),
                cwd,
                None,
                60,
            )
            .await?
        } else {
            let mut model = json!({"providerID":route.provider,"id":route.model});
            if let Some(variant) = &route.variant {
                model["variant"] = variant.clone().into();
            }
            self.json(
                Method::POST,
                "/session",
                cwd,
                Some(json!({
                    "title":format!("Octomus {}", self.entity), "agent":self.agent,
                    "model":model, "permission":permissions,
                })),
                60,
            )
            .await?
        };
        let id = session["id"]
            .as_str()
            .context("Missing OpenCode session identity")?;
        segment(id)?;
        ensure!(
            resume.is_none_or(|expected| id == expected),
            "OpenCode substituted the requested session"
        );
        ensure!(
            session["directory"]
                .as_str()
                .is_some_and(|directory| Path::new(directory) == cwd),
            "OpenCode session belongs to a different workspace"
        );
        ensure!(
            session["model"]["providerID"].as_str() == route.provider.as_deref()
                && session["model"]["id"] == route.model
                && variant_matches(session["model"]["variant"].as_str(), route),
            "OpenCode substituted the requested model or variant"
        );
        ensure!(
            session["permission"] == permissions,
            "OpenCode session has unexpected permissions"
        );
        Ok(id.to_owned())
    }

    pub async fn turn(
        &self,
        session: &str,
        route: &Route,
        cwd: &Path,
        prompt: &str,
        schema: Option<Value>,
    ) -> Result<String> {
        route.require_backend(Backend::Opencode)?;
        let path = format!("/session/{}", segment(session)?);
        let result = process::bounded(
            self.timeout,
            &self.cancel,
            "OpenCode session time limit exceeded",
            self.turn_inner(session, &path, route, cwd, prompt, schema),
        )
        .await
        .and_then(|v| v);
        if result.is_err() {
            // Independent of the cancelled token. Cleanup is bounded; dropping the owner kills the group.
            let _ = self
                .request(Method::POST, &format!("{path}/abort"), cwd)
                // OpenCode allows three seconds before force-killing detached shell groups.
                .timeout(Duration::from_secs(5))
                .send()
                .await;
        }
        result
    }
    async fn turn_inner(
        &self,
        session: &str,
        path: &str,
        route: &Route,
        cwd: &Path,
        prompt: &str,
        schema: Option<Value>,
    ) -> Result<String> {
        // Subscribe before submitting so permission requests cannot be missed.
        let response = self.request(Method::GET, "/event", cwd).send().await?;
        ensure!(
            response.status().is_success(),
            "OpenCode event subscription failed"
        );
        let mut events = Events {
            response,
            buffer: vec![],
            data: String::new(),
            frame_bytes: 0,
        };
        let message = message_id();
        let mut body = json!({"messageID":message,"model":{"providerID":route.provider,"modelID":route.model},
            "agent":self.agent,"system":WORKER_INSTRUCTIONS,"parts":[{"type":"text","text":prompt}]});
        if let Some(variant) = &route.variant {
            body["variant"] = variant.clone().into();
        }
        if let Some(schema) = &schema {
            body["format"] = json!({"type":"json_schema","schema":schema,"retryCount":0});
        }
        let message_path = format!("{path}/message");
        let request = self.json(Method::POST, &message_path, cwd, Some(body), self.timeout);
        tokio::pin!(request);
        let value = loop {
            tokio::select! {
                result = &mut request => break result?,
                event = events.next() => {
                    let event = event?;
                    let props = &event["properties"];
                    let kind = event["type"].as_str().unwrap_or("");
                    let event_session = props["sessionID"].as_str()
                        .or_else(|| props["info"]["sessionID"].as_str())
                        .or_else(|| props["part"]["sessionID"].as_str());
                    if event_session != Some(session) { continue; }
                    match kind {
                        "permission.asked" | "question.asked" | "permission.v2.asked" | "question.v2.asked" => {
                            if let Some(id) = props["id"].as_str() {
                                let id = segment(id)?;
                                let prefix = if kind.contains(".v2.") { format!("/api/session/{}", segment(session)?) } else { String::new() };
                                let (reply_path, body) = if kind.starts_with("permission.") {
                                    (format!("{prefix}/permission/{id}/reply"), json!({"reply":"reject"}))
                                } else { (format!("{prefix}/question/{id}/reject"), json!({})) };
                                let _ = self.request(Method::POST, &reply_path, cwd).json(&body).timeout(Duration::from_secs(2)).send().await;
                            }
                            bail!("OpenCode requested interactive input ({kind}); task blocked");
                        }
                        "message.updated" if props["info"]["role"] == "assistant" && props["info"]["parentID"] == message => {
                            check_model(&props["info"], route)?;
                        }
                        "session.error" => bail!("OpenCode session failed: {}", props["error"]["name"].as_str().unwrap_or("runtime error")),
                        "message.part.updated" if props["part"]["type"] == "tool" && matches!(props["part"]["state"]["status"].as_str(), Some("completed" | "error")) => {
                            self.store.event(&self.entity, "session_progress", &format!("{session} · tool · {}", props["part"]["state"]["status"]))?;
                        }
                        _ => {}
                    }
                }
            }
        };
        let info = &value["info"];
        segment(
            info["id"]
                .as_str()
                .context("Missing OpenCode response identity")?,
        )?;
        ensure!(
            info["sessionID"] == session
                && info["parentID"] == message
                && info["role"] == "assistant",
            "OpenCode returned an unrelated response"
        );
        check_model(info, route)?;
        ensure!(
            info["error"].is_null(),
            "OpenCode turn failed: {}",
            info["error"]["name"].as_str().unwrap_or("runtime error")
        );
        ensure!(
            info["time"]["completed"].is_number(),
            "OpenCode returned an incomplete result"
        );
        ensure!(
            info["finish"] == "stop" || (schema.is_some() && info["finish"] == "tool-calls"),
            "OpenCode turn did not complete successfully"
        );
        if let Some(schema) = schema {
            let output = info
                .get("structured")
                .filter(|s| !s.is_null())
                .context("OpenCode returned no structured result")?;
            crate::schemas::validate(output, &schema)
                .context("Invalid OpenCode structured result")?;
            return Ok(serde_json::to_string(output)?);
        }
        let parts = value["parts"]
            .as_array()
            .context("OpenCode returned no response parts")?;
        let mut answer = String::new();
        for part in parts.iter().filter(|part| {
            part["type"] == "text" && part["ignored"] != true && part["synthetic"] != true
        }) {
            ensure!(
                part["sessionID"] == session && part["messageID"] == info["id"],
                "OpenCode returned an unrelated response part"
            );
            if !answer.is_empty() {
                answer.push('\n');
            }
            answer.push_str(
                part["text"]
                    .as_str()
                    .context("Invalid OpenCode text result")?,
            );
        }
        ensure!(
            !answer.trim().is_empty(),
            "OpenCode returned no final result"
        );
        Ok(answer)
    }
}

fn message_id() -> String {
    // OpenCode's schema/identifier format: low 48 bits of (milliseconds * 4096 +
    // counter), followed by 14 base62 characters. Random UUID IDs are not ordered
    // like native messages and can reorder a session's history on timestamp ties.
    static CLOCK: AtomicU64 = AtomicU64::new(0);
    let now = chrono::Utc::now().timestamp_millis().max(0) as u64 * 4096;
    let previous = CLOCK
        .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |previous| {
            Some((previous + 1).max(now + 1))
        })
        .expect("clock update always succeeds");
    let clock = (previous + 1).max(now + 1) & 0xffff_ffff_ffff;
    let random = uuid::Uuid::new_v4();
    let alphabet = b"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";
    let suffix: String = random.as_bytes()[..14]
        .iter()
        .map(|byte| alphabet[usize::from(*byte) % alphabet.len()] as char)
        .collect();
    format!("msg_{clock:012x}{suffix}")
}

fn variant_matches(reported: Option<&str>, route: &Route) -> bool {
    reported == route.variant.as_deref() || (route.variant.is_none() && reported == Some("default"))
}

fn check_model(info: &Value, route: &Route) -> Result<()> {
    ensure!(
        info["modelID"] == route.model && info["providerID"].as_str() == route.provider.as_deref(),
        "OpenCode substituted the requested model"
    );
    ensure!(
        variant_matches(info["variant"].as_str(), route),
        "OpenCode substituted the requested variant"
    );
    Ok(())
}
fn segment(id: &str) -> Result<String> {
    ensure!(
        !id.is_empty()
            && id.len() <= 256
            && id
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b)),
        "Invalid OpenCode identity"
    );
    Ok(percent_encoding::utf8_percent_encode(id, percent_encoding::NON_ALPHANUMERIC).to_string())
}
async fn read_json(mut response: Response) -> Result<Value> {
    let mut bytes = vec![];
    while let Some(chunk) = response.chunk().await? {
        ensure!(
            bytes.len() + chunk.len() <= MAX_MESSAGE,
            "OpenCode message exceeds 16 MB protocol limit"
        );
        bytes.extend_from_slice(&chunk);
    }
    serde_json::from_slice(&bytes).context("Invalid OpenCode JSON response")
}

/// Only allowlisted metadata leaves the adapter: /provider can contain credentials and options.
pub fn catalog(value: &Value) -> Result<Vec<Model>> {
    let providers = value["all"]
        .as_array()
        .context("Invalid OpenCode provider catalog")?;
    let connected = value["connected"]
        .as_array()
        .context("Missing OpenCode provider availability")?;
    let mut out = vec![];
    for provider in providers {
        let id = provider["id"]
            .as_str()
            .context("Missing OpenCode provider identity")?;
        let available = connected.iter().any(|p| p == id);
        for (model_id, model) in provider["models"]
            .as_object()
            .context("Invalid OpenCode models")?
        {
            ensure!(out.len() < 10000, "OpenCode catalog exceeds 10000 models");
            let toolcall = model["capabilities"]["toolcall"] == true;
            let text = model["capabilities"]["input"]["text"] == true
                && model["capabilities"]["output"]["text"] == true;
            out.push(Model {
                backend: Backend::Opencode,
                provider: Some(id.into()),
                provider_name: provider["name"].as_str().map(str::to_owned),
                model: model_id.clone(),
                display_name: model["name"].as_str().unwrap_or(model_id).into(),
                efforts: vec![],
                variants: model["variants"]
                    .as_object()
                    .map(|variants| variants.keys().cloned().collect())
                    .unwrap_or_default(),
                available: available && toolcall && text,
                unavailable_reason: if !available {
                    Some(
                        "Provider is not configured; configure OpenCode as the service user".into(),
                    )
                } else if !toolcall || !text {
                    Some("Model must support text and tool calling".into())
                } else {
                    None
                },
            });
        }
    }
    Ok(out)
}

struct Events {
    response: Response,
    buffer: Vec<u8>,
    data: String,
    frame_bytes: usize,
}
impl Events {
    async fn next(&mut self) -> Result<Value> {
        loop {
            if let Some(end) = self.buffer.iter().position(|b| *b == b'\n') {
                let line = self.buffer.drain(..=end).collect::<Vec<_>>();
                self.frame_bytes += line.len();
                ensure!(
                    self.frame_bytes <= MAX_MESSAGE,
                    "OpenCode event exceeds 16 MB protocol limit"
                );
                let line = std::str::from_utf8(&line)
                    .context("Invalid OpenCode event encoding")?
                    .trim_end_matches(['\r', '\n']);
                if line.is_empty() {
                    self.frame_bytes = 0;
                    if !self.data.is_empty() {
                        let data = std::mem::take(&mut self.data);
                        return serde_json::from_str(&data).context("Invalid OpenCode event JSON");
                    }
                } else if let Some(data) = line.strip_prefix("data:") {
                    if !self.data.is_empty() {
                        self.data.push('\n');
                    }
                    self.data.push_str(data.strip_prefix(' ').unwrap_or(data));
                }
            } else {
                let chunk = self
                    .response
                    .chunk()
                    .await?
                    .context("OpenCode event stream disconnected")?;
                ensure!(
                    self.buffer.len() + chunk.len() <= MAX_MESSAGE,
                    "OpenCode event backlog exceeds 16 MB"
                );
                self.buffer.extend_from_slice(&chunk);
            }
        }
    }
}
