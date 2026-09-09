use crate::{
    codex::{self, Codex},
    config::{Config, Route},
    git,
    model::*,
    store::{Store, redact},
};
use anyhow::{Result, ensure};
use serde_json::{Value, json};
use std::{
    collections::{HashMap, HashSet},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio_util::sync::CancellationToken;

mod execution;
mod planning;

pub use planning::{feedback_targets, validate_proposals};

#[derive(Default)]
pub struct Runtime {
    pub tasks: HashMap<String, CancellationToken>,
    pub cycle: Option<CancellationToken>,
    pub cycle_mode: Option<CycleMode>,
    pub last_retention_at: i64,
}
#[derive(Clone)]
pub struct App {
    pub store: Store,
    pub data_dir: PathBuf,
    pub runtime: Arc<Mutex<Runtime>>,
    pub gate: Arc<tokio::sync::Mutex<()>>,
    pub shutdown: CancellationToken,
}
impl App {
    pub fn new(store: Store, data_dir: PathBuf) -> Self {
        Self {
            store,
            data_dir,
            runtime: Arc::new(Mutex::new(Runtime::default())),
            gate: Arc::new(tokio::sync::Mutex::new(())),
            shutdown: CancellationToken::new(),
        }
    }
    pub fn config(&self) -> Result<Config> {
        Ok(self.store.get("settings", "config")?.unwrap_or_default())
    }
    pub fn control(&self) -> Result<Control> {
        Ok(self.store.get("settings", "control")?.unwrap_or_default())
    }
    pub fn save_task(&self, t: &mut Task) -> Result<()> {
        t.updated_at = now();
        self.store.put("task", &t.id, t)
    }
    pub fn transition(&self, t: &mut Task, status: Status) -> Result<()> {
        t.status = status;
        self.save_task(t)?;
        self.store
            .event(&t.id, "status", &format!("{:?}", t.status))
    }
    pub fn recover(&self) -> Result<()> {
        for mut task in self.store.list::<Task>("task")? {
            if !task.status.active() {
                continue;
            }
            let initialized = task.execution_session.is_some()
                && Path::new(&task.workspace).join(".git").exists()
                && !task.comparison_base.is_empty();
            for session in &mut task.sessions {
                if session.status == "running" {
                    session.status = "interrupted".into();
                }
            }
            if initialized && task.attempts < task.config.max_retries {
                task.attempts += 1;
                task.status = Status::Queued;
                task.error = Some("Recovering an interrupted task: inspecting the recorded workspace and reconciling remote state before continuing.".into());
            } else {
                task.status = Status::Blocked;
                task.error = Some("Service interrupted before workspace initialization completed, or retry budget exhausted. Inspect the preserved task before retrying.".into());
            }
            self.save_task(&mut task)?;
            self.store.event(
                &task.id,
                "recovery",
                task.error.as_deref().unwrap_or("Recovering"),
            )?;
        }
        for mut cycle in self.store.list::<Cycle>("cycle")? {
            if cycle.status == "running" {
                cycle.status = "interrupted".into();
                cycle.error =
                    Some("Discovery interrupted; incomplete proposals were not dispatched".into());
                cycle.completed_at = Some(now());
                self.store.put("cycle", &cycle.id, &cycle)?;
            }
        }
        Ok(())
    }
    pub async fn doctor(&self, c: &Config) -> Result<Value> {
        self.doctor_for(c, CycleMode::Execution).await
    }
    pub async fn doctor_for(&self, c: &Config, mode: CycleMode) -> Result<Value> {
        if mode == CycleMode::Audit {
            c.validate_audit()?;
        } else {
            c.validate(true)?;
        }
        let cancel = self.shutdown.child_token();
        let installed = crate::process::run(
            &c.codex_binary,
            &["--version"],
            &self.data_dir,
            c.command_timeout_seconds.min(60),
            &cancel,
        )
        .await?;
        let warning = codex::version_warning(&installed);
        if let Some(warning) = &warning {
            tracing::warn!("{warning}");
        }
        git::validate_remote(c, &cancel).await?;
        let mut cx =
            Codex::connect(c, &self.data_dir, self.store.clone(), "system", cancel).await?;
        let account = cx
            .rpc("account/read", json!({"refreshToken":false}))
            .await?;
        ensure!(
            account["requiresOpenaiAuth"] == false || !account["account"].is_null(),
            "Codex authentication is missing; run codex login as the service user"
        );
        let models = cx.models().await?;
        codex::validate_routes_for(c, &models, mode == CycleMode::Audit)?;
        Ok(
            json!({"ok":true,"mode":mode,"models":models,"codex_version":installed,"tested_codex_version":codex::TESTED_VERSION,"warnings":warning.iter().collect::<Vec<_>>(),"message":format!("Repository, GitHub authentication, and {} model/effort routes are available.{}", if mode == CycleMode::Audit { "planning" } else { "all" }, warning.map(|w| format!(" Warning: {w}")).unwrap_or_default())}),
        )
    }
    pub async fn run(self) {
        let mut interval = tokio::time::interval(Duration::from_secs(1));
        loop {
            tokio::select! {
                _ = self.shutdown.cancelled() => break,
                _ = interval.tick() => {
                    if let Err(error) = self.tick().await {
                        let message = format!("{error:#}");
                        tracing::error!("Scheduler: {}", redact(&message));
                        let _ = self.store.event("system", "error", &message);
                        if let Ok(mut control) = self.control() {
                            control.error = Some(redact(&message));
                            control.paused = true;
                            let _ = self.store.put("settings", "control", &control);
                        }
                        self.notify("service_paused", json!({"error": redact(&message)}));
                    }
                }
            }
        }
        let rt = self.runtime.lock().unwrap();
        for token in rt.tasks.values() {
            token.cancel();
        }
        if let Some(token) = &rt.cycle {
            token.cancel();
        }
    }
    async fn tick(&self) -> Result<()> {
        let _gate = self.gate.lock().await;
        let mut control = self.control()?;
        if control.paused {
            return Ok(());
        }
        let c = self.config()?;
        c.validate(true)?;
        let tasks = self.store.list::<Task>("task")?;
        let (active, cycle_active) = {
            let rt = self.runtime.lock().unwrap();
            (rt.tasks.len(), rt.cycle.is_some())
        };
        let cleanup_due =
            chrono::Utc::now().timestamp() - self.runtime.lock().unwrap().last_retention_at >= 900;
        if active == 0 && !cycle_active && cleanup_due {
            self.retention(&c).await?;
            self.runtime.lock().unwrap().last_retention_at = chrono::Utc::now().timestamp();
        }
        if cycle_active {
            return Ok(());
        }
        let mut slots = c.execution_concurrency.saturating_sub(active);
        let mut occupied: HashSet<String> = tasks
            .iter()
            .filter(|t| t.status.active())
            .map(|t| t.proposal.target.clone())
            .filter(|b| b != &c.default_branch)
            .collect();
        for mut task in tasks
            .iter()
            .rev()
            .filter(|t| t.status == Status::Queued)
            .cloned()
        {
            if slots == 0 {
                break;
            }
            if task.proposal.dependencies.iter().any(|id| {
                !tasks
                    .iter()
                    .any(|t| t.id == *id && t.status == Status::Published)
            }) {
                if task.proposal.dependencies.iter().any(|id| {
                    tasks.iter().any(|t| {
                        t.id == *id
                            && matches!(
                                t.status,
                                Status::Cancelled | Status::Blocked | Status::Failed
                            )
                    })
                }) {
                    task.error =
                        Some("A dependency is unresolved; retry after it is published".into());
                    self.transition(&mut task, Status::Blocked)?;
                }
                continue;
            }
            if task.proposal.target != c.default_branch && occupied.contains(&task.proposal.target)
            {
                continue;
            }
            occupied.insert(task.proposal.target.clone());
            slots -= 1;
            let cancel = self.shutdown.child_token();
            self.runtime
                .lock()
                .unwrap()
                .tasks
                .insert(task.id.clone(), cancel.clone());
            self.transition(&mut task, Status::Executing)?;
            let app = self.clone();
            tokio::spawn(async move {
                let result = tokio::time::timeout(
                    Duration::from_secs(task.config.task_timeout_seconds),
                    app.execute(&mut task, &cancel),
                )
                .await;
                let error = match result {
                    Ok(Ok(())) => None,
                    Ok(Err(e)) => Some(format!("{e:#}")),
                    Err(_) => Some("Task time limit exceeded".into()),
                };
                if let Some(error) = error {
                    task.error = Some(redact(&error));
                    for session in &mut task.sessions {
                        if session.status == "running" {
                            session.status = "failed".into();
                            if session.summary.is_empty() {
                                session.summary = redact(&error);
                            }
                        }
                    }
                    let status = if cancel.is_cancelled()
                        && !app.shutdown.is_cancelled()
                        && task.output_commit.is_none()
                    {
                        Status::Cancelled
                    } else {
                        Status::Blocked
                    };
                    let _ = app.transition(&mut task, status);
                    let _ = app.store.event(&task.id, "error", &error);
                }
                app.runtime.lock().unwrap().tasks.remove(&task.id);
                let detail = json!({"task_id":task.id,"title":task.proposal.title,"branch":task.branch,"pr_url":task.pr_url,"pr_number":task.pr_number,"error":task.error});
                match task.status {
                    Status::Published => app.notify("task_published", detail),
                    Status::Blocked => app.notify("task_blocked", detail),
                    _ => {}
                }
            });
        }
        // Finish the current queue before the next planning cycle, so grounding includes its results.
        let rt_busy = {
            let rt = self.runtime.lock().unwrap();
            !rt.tasks.is_empty() || rt.cycle.is_some()
        };
        if !rt_busy
            && !tasks.iter().any(|t| t.status == Status::Queued)
            && chrono::Utc::now().timestamp() >= control.next_cycle_at
        {
            let cancel = self.shutdown.child_token();
            {
                let mut rt = self.runtime.lock().unwrap();
                rt.cycle = Some(cancel.clone());
                rt.cycle_mode = Some(CycleMode::Execution);
            }
            control.cycle_number += 1;
            control.next_cycle_at =
                chrono::Utc::now().timestamp() + c.cycle_interval_seconds as i64;
            self.store.put("settings", "control", &control)?;
            let app = self.clone();
            tokio::spawn(async move {
                let result = app
                    .cycle(&c, control.cycle_number, CycleMode::Execution, &cancel)
                    .await;
                app.finish_cycle(&c, CycleMode::Execution, result).await;
            });
        }
        Ok(())
    }
    // Caller holds the scheduler gate; audits never unpause the execution queue.
    pub fn start_audit(&self) -> Result<()> {
        let c = self.config()?;
        c.validate_audit()?;
        let mut control = self.control()?;
        let mut rt = self.runtime.lock().unwrap();
        ensure!(
            control.paused && rt.tasks.is_empty() && rt.cycle.is_none(),
            "Pause and wait for active work before running an audit"
        );
        control.cycle_number += 1;
        control.error = None;
        self.store.put("settings", "control", &control)?;
        let cancel = self.shutdown.child_token();
        rt.cycle = Some(cancel.clone());
        rt.cycle_mode = Some(CycleMode::Audit);
        let app = self.clone();
        tokio::spawn(async move {
            let result = app
                .cycle(&c, control.cycle_number, CycleMode::Audit, &cancel)
                .await;
            app.finish_cycle(&c, CycleMode::Audit, result).await;
        });
        Ok(())
    }
    async fn finish_cycle(&self, config: &Config, mode: CycleMode, result: Result<()>) {
        let _gate = self.gate.lock().await;
        if let Ok(mut control) = self.control() {
            control.error = result.err().map(|error| redact(&format!("{error:#}")));
            if mode == CycleMode::Execution {
                control.next_cycle_at =
                    chrono::Utc::now().timestamp() + config.cycle_interval_seconds as i64;
            }
            let _ = self.store.put("settings", "control", &control);
        }
        let mut runtime = self.runtime.lock().unwrap();
        runtime.cycle = None;
        runtime.cycle_mode = None;
    }
    async fn budget(
        &self,
        c: &Config,
        cycle_id: &str,
        task_id: Option<&str>,
        role: &str,
        route: &Route,
    ) -> Result<()> {
        let dir = self.data_dir.clone();
        let size = tokio::task::spawn_blocking(move || directory_size(&dir)).await??;
        ensure!(
            size < c.max_workspace_bytes,
            "Workspace storage limit reached ({size} bytes). Resolve retained tasks or increase the limit"
        );
        self.store.reserve_session(
            c.max_sessions_per_day,
            &crate::store::Admission::new(cycle_id, task_id, role, route),
        )
    }
    async fn retention(&self, c: &Config) -> Result<()> {
        self.store.prune_events(c.retain_events)?;
        let cutoff =
            chrono::Utc::now() - chrono::Duration::days(c.retain_completed_days.min(36500) as i64);
        for task in self.store.list::<Task>("task")? {
            if task.status == Status::Published
                && chrono::DateTime::parse_from_rfc3339(&task.updated_at).is_ok_and(|d| d < cutoff)
            {
                let path = PathBuf::from(&task.workspace);
                if !task.workspace.is_empty()
                    && path.starts_with(self.data_dir.join("tasks"))
                    && path.exists()
                {
                    tokio::fs::remove_dir_all(path.parent().unwrap()).await?;
                }
            }
        }
        for cycle in self.store.list::<Cycle>("cycle")? {
            if ["completed", "idle"].contains(&cycle.status.as_str())
                && chrono::DateTime::parse_from_rfc3339(&cycle.started_at).is_ok_and(|d| d < cutoff)
            {
                let path = self.data_dir.join("cycles").join(&cycle.id);
                if path.exists() {
                    tokio::fs::remove_dir_all(path).await?;
                }
            }
        }
        Ok(())
    }
}
fn directory_size(path: &Path) -> Result<u64> {
    let mut size = 0u64;
    for e in std::fs::read_dir(path)? {
        let e = e?;
        let meta = e.metadata()?;
        if e.file_type()?.is_symlink() {
            continue;
        }
        size = size.saturating_add(if meta.is_dir() {
            directory_size(&e.path())?
        } else {
            meta.len()
        });
    }
    Ok(size)
}
