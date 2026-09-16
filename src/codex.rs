use crate::{
    config::{Backend, Config, Route},
    process::{self, GroupChild},
    runner::{MAX_MESSAGE, Model},
    store::Store,
};
use anyhow::{Context, Result, bail, ensure};
use futures::StreamExt;
use serde_json::{Value, json};
use std::{collections::VecDeque, path::Path, process::Stdio, time::Duration};
use tokio::io::AsyncWriteExt;
use tokio::process::{ChildStdin, ChildStdout};
use tokio_util::codec::{FramedRead, LinesCodec};
use tokio_util::sync::CancellationToken;

pub const TESTED_VERSION: &str = "0.153.4";

pub fn version_warning(installed: &str) -> Option<String> {
    let expected = format!("codex-cli {TESTED_VERSION}");
    (installed.trim() != expected).then(|| {
        crate::runner::version_warning(
            Backend::Codex,
            installed,
            &format!("tested {expected}. Pin the tested CLI before live commissioning"),
        )
    })
}

pub struct Codex {
    _child: GroupChild,
    input: ChildStdin,
    output: FramedRead<ChildStdout, LinesCodec>,
    serial: u64,
    pending: VecDeque<(Value, usize)>,
    pending_bytes: usize,
    timeout: u64,
    cancel: CancellationToken,
    store: Store,
    entity: String,
}
impl Codex {
    pub async fn connect(
        config: &Config,
        cwd: &Path,
        store: Store,
        entity: &str,
        cancel: CancellationToken,
    ) -> Result<Self> {
        ensure!(!cancel.is_cancelled(), "Session cancelled");
        let mut child = GroupChild::new(
            process::command(&config.codex_binary, cwd)
                .args(["app-server", "--listen", "stdio://"])
                .stdin(Stdio::piped())
                .stderr(Stdio::null())
                .spawn()
                .context(
                    "Could not start Codex app-server; install and authenticate Codex on this host",
                )?,
        );
        let input = child.0.stdin.take().unwrap();
        let output = FramedRead::new(
            child.0.stdout.take().unwrap(),
            LinesCodec::new_with_max_length(MAX_MESSAGE),
        );
        let mut s = Self {
            _child: child,
            input,
            output,
            serial: 0,
            pending: VecDeque::new(),
            pending_bytes: 0,
            timeout: config.session_timeout_seconds,
            cancel,
            store,
            entity: entity.into(),
        };
        s.rpc("initialize",json!({"clientInfo":{"name":"octomus_agent","title":"Octomus Agent","version":env!("CARGO_PKG_VERSION")},"capabilities":{"experimentalApi":false}})).await?;
        s.send(json!({"method":"initialized","params":{}})).await?;
        Ok(s)
    }
    /// Authentication state plus the installed CLI's version against the tested
    /// baseline.
    pub async fn diagnostics(
        &mut self,
        config: &Config,
        cwd: &Path,
        cancel: &CancellationToken,
    ) -> Result<Value> {
        let account = self
            .rpc("account/read", json!({"refreshToken":false}))
            .await?;
        ensure!(
            account["requiresOpenaiAuth"] == false || !account["account"].is_null(),
            "Codex authentication is missing; run codex login as the service user"
        );
        let version = crate::process::run_machine(
            &config.codex_binary,
            &["--version"],
            cwd,
            config.command_timeout_seconds.min(60),
            cancel,
        )
        .await?
        .trim()
        .to_owned();
        Ok(crate::runner::diagnostics_value(
            Backend::Codex,
            &version,
            TESTED_VERSION,
            version_warning(&version),
        ))
    }
    async fn send(&mut self, value: Value) -> Result<()> {
        let payload = format!("{value}\n");
        let write = async {
            self.input.write_all(payload.as_bytes()).await?;
            self.input.flush().await
        };
        process::bounded(
            self.timeout.min(60),
            &self.cancel,
            "Codex write timed out",
            write,
        )
        .await??;
        Ok(())
    }
    /// Writes without checking `self.cancel`: cleanup requests must still reach the
    /// server after operator cancellation. Callers bound the wait themselves.
    async fn send_best_effort(&mut self, value: Value) -> Result<()> {
        let payload = format!("{value}\n");
        self.input.write_all(payload.as_bytes()).await?;
        self.input.flush().await?;
        Ok(())
    }
    async fn receive(&mut self) -> Result<Value> {
        let line = process::bounded(
            self.timeout,
            &self.cancel,
            "Codex response timed out",
            self.output.next(),
        )
        .await?
        .context("Codex app-server disconnected")??;
        let v: Value = serde_json::from_str(&line).context("Invalid app-server JSON")?;
        if v.get("method").is_some() && v.get("id").is_some() {
            self.send(json!({"id":v["id"],"error":{"code":-32000,"message":"Octomus unattended mode cannot answer interactive requests"}})).await?;
            bail!(
                "Codex requested interactive input ({}); task blocked",
                v["method"]
            );
        }
        Ok(v)
    }
    pub async fn rpc(&mut self, method: &str, params: Value) -> Result<Value> {
        self.serial += 1;
        let id = self.serial;
        self.send(json!({"id":id,"method":method,"params":params}))
            .await?;
        // A turn can emit notifications before the request response; preserve their order.
        let deadline = tokio::time::Instant::now() + Duration::from_secs(60);
        let cancel = self.cancel.clone();
        loop {
            let v = process::bounded_at(deadline, &cancel, "Codex RPC timed out", self.receive())
                .await??;
            if v["id"] == id {
                if let Some(e) = v.get("error") {
                    bail!("Codex {method}: {}", crate::store::redact(&e.to_string()));
                }
                return v.get("result").cloned().context("Missing RPC result");
            }
            ensure!(
                self.pending.len() < 10000,
                "Codex notification backlog exceeded"
            );
            let size = v.to_string().len();
            ensure!(
                self.pending_bytes + size <= 8 * 1024 * 1024,
                "Codex notification backlog exceeds 8 MB"
            );
            self.pending_bytes += size;
            self.pending.push_back((v, size));
        }
    }
    pub async fn models(&mut self) -> Result<Vec<Model>> {
        let mut out = vec![];
        let mut cursor = Value::Null;
        loop {
            let v = self
                .rpc(
                    "model/list",
                    json!({"limit":100,"cursor":cursor,"includeHidden":true}),
                )
                .await?;
            for m in v["data"].as_array().context("Invalid model catalog")? {
                out.push(Model {
                    backend: Backend::Codex,
                    provider: None,
                    provider_name: None,
                    model: m["model"]
                        .as_str()
                        .context("Invalid Codex model identity")?
                        .into(),
                    display_name: m["displayName"].as_str().unwrap_or("").into(),
                    efforts: m["supportedReasoningEfforts"]
                        .as_array()
                        .context("Invalid Codex reasoning catalog")?
                        .iter()
                        .map(|e| {
                            e["reasoningEffort"]
                                .as_str()
                                .map(str::to_owned)
                                .context("Invalid reasoning effort")
                        })
                        .collect::<Result<_>>()?,
                    variants: vec![],
                    available: true,
                    unavailable_reason: None,
                });
            }
            cursor = v["nextCursor"].clone();
            if cursor.is_null() {
                break;
            }
            ensure!(out.len() < 10000, "Invalid model pagination");
        }
        Ok(out)
    }
    pub async fn start(
        &mut self,
        route: &Route,
        cwd: &Path,
        resume: Option<&str>,
    ) -> Result<String> {
        route.require_backend(Backend::Codex)?;
        let mut params = json!({"model":route.model,"cwd":cwd,"approvalPolicy":"never","sandbox":"danger-full-access","config":{"model_reasoning_effort":route.effort},"developerInstructions":crate::runner::WORKER_INSTRUCTIONS});
        let method = if let Some(id) = resume {
            params["threadId"] = id.into();
            "thread/resume"
        } else {
            "thread/start"
        };
        let v = self.rpc(method, params).await?;
        ensure!(
            v["model"].as_str() == Some(&route.model),
            "Runtime substituted the requested model"
        );
        ensure!(
            v["reasoningEffort"].as_str() == Some(&route.effort),
            "Runtime substituted the requested reasoning effort"
        );
        ensure!(
            v["approvalPolicy"] == "never" && v["sandbox"]["type"] == "dangerFullAccess",
            "Runtime did not grant the configured no-sandbox/never-approve policy"
        );
        let identity = v["thread"]["id"]
            .as_str()
            .context("Missing Codex thread identity")?;
        uuid::Uuid::parse_str(identity).context("Invalid Codex thread identity")?;
        ensure!(
            resume.is_none_or(|id| identity == id),
            "Codex substituted the requested thread"
        );
        Ok(identity.to_owned())
    }
    pub async fn turn(
        &mut self,
        thread: &str,
        route: &Route,
        cwd: &Path,
        prompt: &str,
        schema: Option<Value>,
    ) -> Result<String> {
        route.require_backend(Backend::Codex)?;
        let mut params = json!({"threadId":thread,"cwd":cwd,"model":route.model,"effort":route.effort,"approvalPolicy":"never","sandboxPolicy":{"type":"dangerFullAccess"},"input":[{"type":"text","text":prompt,"text_elements":[]}]});
        if let Some(schema) = schema {
            params["outputSchema"] = schema;
        }
        let v = self.rpc("turn/start", params).await?;
        let turn = v["turn"]["id"]
            .as_str()
            .context("Missing turn identity")?
            .to_owned();
        let deadline = tokio::time::Instant::now() + Duration::from_secs(self.timeout);
        let cancel = self.cancel.clone();
        let result: Result<String> = async {
            let mut answer = String::new();
            loop {
                ensure!(!self.cancel.is_cancelled(), "Session cancelled");
                ensure!(
                    tokio::time::Instant::now() < deadline,
                    "Codex session time limit exceeded"
                );
                let event = if let Some((v, size)) = self.pending.pop_front() {
                    self.pending_bytes -= size;
                    v
                } else {
                    process::bounded_at(
                        deadline,
                        &cancel,
                        "Codex session time limit exceeded",
                        self.receive(),
                    )
                    .await??
                };
                if event["params"]["threadId"].as_str() != Some(thread) {
                    continue;
                }
                if event["params"]["turnId"]
                    .as_str()
                    .is_some_and(|id| id != turn)
                {
                    continue;
                }
                match event["method"].as_str().unwrap_or("") {
                    "item/completed" => {
                        let item = &event["params"]["item"];
                        if item["type"] == "agentMessage" && item["phase"] != "commentary" {
                            answer = item["text"].as_str().unwrap_or("").to_owned();
                        }
                        // Only record metadata, never raw tool arguments or command output from session notifications.
                        self.store.event(
                            &self.entity,
                            "session_progress",
                            &format!(
                                "{} · {} · {}",
                                thread,
                                item["type"].as_str().unwrap_or("item"),
                                item["status"].as_str().unwrap_or("completed")
                            ),
                        )?;
                    }
                    "turn/completed" if event["params"]["turn"]["id"] == turn => {
                        ensure!(
                            event["params"]["turn"]["status"] == "completed",
                            "Codex turn did not complete successfully: {}",
                            crate::store::redact(&event["params"]["turn"]["error"].to_string())
                        );
                        ensure!(!answer.trim().is_empty(), "Codex returned no final result");
                        break Ok(answer);
                    }
                    _ => {}
                }
            }
        }
        .await;
        if result.is_err() {
            // Best-effort interrupt bypasses the cancel select so it still reaches
            // the server after operator cancellation; the response is not awaited.
            self.serial += 1;
            let _ = tokio::time::timeout(
                Duration::from_secs(5),
                self.send_best_effort(
                    json!({"id":self.serial,"method":"turn/interrupt","params":{"threadId":thread,"turnId":turn}}),
                ),
            )
            .await;
        }
        result
    }
}
