use crate::{
    codex,
    config::{Config, Route},
    git,
    model::*,
    process::Deadline,
    runner::{Runner, validate_route},
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

pub mod baseline;
mod capacity;
mod execution;
mod housekeeping;
use housekeeping::directory_size;
mod memory;
mod planning;

pub use baseline::{BaselineConflict, BaselineJob, baseline_fingerprint};
pub use capacity::{PrIdentity, PrRefresh};
pub use planning::{external_context, resolve_target, validate_proposals};

#[derive(Default)]
pub struct Runtime {
    pub tasks: HashMap<String, CancellationToken>,
    pub cycle: Option<CancellationToken>,
    pub cycle_mode: Option<CycleMode>,
    pub last_retention_at: i64,
    pub last_observation_at: i64,
    pub housekeeping: Option<tokio::task::JoinHandle<()>>,
    pub reconciling_publication: bool,
    pub checked_cycles: HashSet<String>,
    pub pr_refresh: Option<PrRefresh>,
    pub pr_refresh_last_attempt: i64,
    pub pr_refresh_error: Option<String>,
    pub pr_observation: Option<(capacity::PrIdentity, i64)>,
    pub baseline: Option<BaselineJob>,
    pub default_observation: Option<DefaultBranchObservation>,
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
    /// A poisoned runtime mutex means a worker panicked; the scheduler keeps
    /// operating on the surviving state rather than wedging on the poison.
    pub fn runtime(&self) -> std::sync::MutexGuard<'_, Runtime> {
        self.runtime.lock().unwrap_or_else(|e| e.into_inner())
    }
    pub(crate) fn task_workspace(&self, task_id: &str) -> PathBuf {
        self.data_dir.join("tasks").join(task_id).join("workspace")
    }
    pub(crate) fn cycle_workspace(&self, cycle_id: &str, label: &str) -> PathBuf {
        self.data_dir
            .join("cycles")
            .join(cycle_id)
            .join(label)
            .join("workspace")
    }
    pub fn task_guard(&self, id: &str) -> TaskGuard {
        TaskGuard {
            app: self.clone(),
            id: id.to_owned(),
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
        self.recover_baselines()?;
        self.seed_pr_reservations()?;
        for mut task in self.store.tasks_with_status(&Status::ACTIVE)? {
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
            // The marker only cancels work that never produced an output commit;
            // a task cancelled mid-publication keeps its commit for reconciliation.
            let operator_cancelled = task.output_commit.is_none()
                && self
                    .store
                    .get::<serde_json::Value>("cancel", &task.id)?
                    .is_some_and(|v| !v.is_null());
            if operator_cancelled {
                task.status = Status::Cancelled;
                task.error = Some("Operator cancellation preserved across restart".into());
            } else if initialized && task.attempts < task.execution_config().max_retries {
                task.attempts += 1;
                task.status = Status::Queued;
                task.error = Some("Recovering an interrupted task: inspecting the recorded workspace and reconciling remote state before continuing.".into());
            } else {
                task.status = Status::Blocked;
                task.blocked_reason = Some(if initialized {
                    BlockedReason::RetryLimit
                } else {
                    BlockedReason::WorkspaceInvalid
                });
                task.error = Some("Service interrupted before workspace initialization completed, or retry budget exhausted. Inspect the preserved task before retrying.".into());
            }
            self.save_task(&mut task)?;
            self.store.event(
                &task.id,
                "recovery",
                task.error.as_deref().unwrap_or("Recovering"),
            )?;
        }
        for mut cycle in self.store.running_cycles()? {
            if cycle.status == "running" {
                cycle.status = "interrupted".into();
                cycle.error =
                    Some("Discovery interrupted; incomplete proposals were not dispatched".into());
                cycle.completed_at = Some(now());
                for session in &mut cycle.sessions {
                    if session.status == "running" {
                        session.status = "interrupted".into();
                    }
                }
                self.store.put("cycle", &cycle.id, &cycle)?;
            }
        }
        let mut control = self.control()?;
        if control
            .batch
            .as_ref()
            .is_some_and(|b| b.phase == BatchPhase::Planning)
        {
            control.set_mode(OperatingMode::Paused);
            control.error =
                Some("One-shot planning was interrupted; incomplete work was not replayed".into());
            self.store.put("settings", "control", &control)?;
        }
        Ok(())
    }
    pub async fn doctor_for(&self, c: &Config, mode: CycleMode) -> Result<Value> {
        if mode == CycleMode::Audit {
            c.validate_audit()?;
        } else {
            c.validate(true)?;
        }
        let cancel = self.shutdown.child_token();
        git::validate_remote(c, &cancel).await?;
        let routes = c.routes_for(mode == CycleMode::Audit);
        let backends = routes
            .iter()
            .map(|(_, route)| route.backend)
            .collect::<std::collections::BTreeSet<_>>();
        let mut diagnostics = vec![];
        let mut models = vec![];
        let mut warnings = vec![];
        let mut errors = vec![];
        for backend in backends {
            let check = async {
                let mut client = Runner::connect(
                    backend,
                    c,
                    &self.data_dir,
                    self.store.clone(),
                    "system",
                    cancel.clone(),
                )
                .await?;
                let diagnostic = client.diagnostics(c, &self.data_dir, &cancel).await?;
                let catalog = client.models(&self.data_dir).await?;
                for (name, route) in routes.iter().filter(|(_, route)| route.backend == backend) {
                    if let Err(error) = validate_route(route, &catalog) {
                        errors.push(format!("{name}: {error}"));
                    }
                }
                if let Some(warning) = diagnostic["warning"].as_str() {
                    tracing::warn!("{warning}");
                    warnings.push(warning.to_owned());
                }
                models.extend(catalog);
                diagnostics.push(diagnostic);
                Ok::<_, anyhow::Error>(())
            }
            .await;
            if let Err(error) = check {
                errors.push(format!("{backend}: {error:#}"));
            }
        }
        ensure!(errors.is_empty(), "{}", errors.join("; "));
        let mut result = json!({"ok":true,"mode":mode,"models":models,"backends":diagnostics,"warnings":warnings,
            "message":format!("Repository, GitHub authentication, and {} model routes are available.{}", if mode == CycleMode::Audit { "planning" } else { "all" },
                if warnings.is_empty() { String::new() } else { format!(" Warning: {}", warnings.join(" ")) })});
        if let Some(diagnostic) = diagnostics.iter().find(|d| d["backend"] == "codex") {
            result["codex_version"] = diagnostic["version"].clone();
            result["tested_codex_version"] = codex::TESTED_VERSION.into();
        }
        Ok(result)
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
                            control.set_mode(OperatingMode::Paused);
                            let _ = self.store.put("settings", "control", &control);
                        }
                    }
                }
            }
        }
        let rt = self.runtime();
        for token in rt.tasks.values() {
            token.cancel();
        }
        if let Some(token) = &rt.cycle {
            token.cancel();
        }
        if let Some(job) = &rt.pr_refresh {
            job.cancel.cancel();
        }
        if let Some(job) = &rt.baseline {
            job.cancel.cancel();
        }
    }
    async fn tick(&self) -> Result<()> {
        let _gate = self.gate.lock().await;
        self.schedule_housekeeping();
        // Reconciliation can write a preserved PR branch. Reserve publication while
        // leaving the gate available to pause and other operator controls.
        if self.runtime().reconciling_publication {
            return Ok(());
        }
        if self.runtime().baseline.is_some() {
            return Ok(());
        }
        let mut control = self.control()?;
        if control.paused {
            return Ok(());
        }
        let c = self.config()?;
        c.validate(true)?;
        let tasks = self
            .store
            .scheduling_tasks(control.batch.as_ref().map(|b| b.id.as_str()))?;
        self.runtime()
            .checked_cycles
            .retain(|id| tasks.iter().any(|t| &t.cycle_id == id));
        let (active, cycle_active) = {
            let rt = self.runtime();
            (rt.tasks.len(), rt.cycle.is_some())
        };
        if cycle_active {
            return Ok(());
        }
        if let Some(batch) = &control.batch {
            let (pending, failed) = self.store.batch_counts(&batch.id)?;
            if pending == 0 && (batch.phase == BatchPhase::Executing || failed > 0) {
                control.set_mode(OperatingMode::Paused);
                self.store.event(
                    "system",
                    "run_complete",
                    if failed > 0 {
                        "Run once finished with unresolved work"
                    } else {
                        "Run once completed; new work paused"
                    },
                )?;
                self.store.put("settings", "control", &control)?;
                return Ok(());
            }
        }
        let mut slots = c.execution_concurrency.saturating_sub(active);
        let mut occupied: HashSet<String> = tasks
            .iter()
            .filter(|t| t.status.active())
            .map(|t| t.proposal.target.clone())
            .filter(|b| b != &c.default_branch)
            .collect();
        let fresh_inventory = self.collect_pr_refresh();
        let mut unreserved_new = false;
        for t in tasks.iter().filter(|t| {
            t.status == Status::Queued
                && control
                    .batch
                    .as_ref()
                    .is_none_or(|b| t.run_id.as_deref() == Some(&b.id))
        }) {
            if t.proposal.target == t.config.default_branch
                && t.output_commit.is_none()
                && !self.store.has_pr_reservation(&t.id)?
            {
                unreserved_new = true;
                break;
            }
        }
        if slots > 0 && unreserved_new && fresh_inventory.is_none() {
            self.schedule_pr_refresh(&c)?;
        }
        for mut task in tasks
            .iter()
            .filter(|t| {
                t.status == Status::Queued
                    && control
                        .batch
                        .as_ref()
                        .is_none_or(|b| t.run_id.as_deref() == Some(&b.id))
            })
            .cloned()
        {
            if slots == 0 {
                break;
            }
            if !self.runtime().checked_cycles.contains(&task.cycle_id) {
                let group = self.store.tasks_for_cycle(&task.cycle_id)?;
                let proposals: Vec<_> = group
                    .iter()
                    .map(|t| {
                        let mut p = t.proposal.clone();
                        p.id = t.id.clone();
                        p
                    })
                    .collect();
                if let Err(error) = planning::validate_branch_order(&task.config, &proposals) {
                    for mut invalid in group.into_iter().filter(|t| t.status == Status::Queued) {
                        invalid.blocked_reason = Some(BlockedReason::InvalidPlan);
                        invalid.error = Some(format!("{error:#}"));
                        self.transition(&mut invalid, Status::Blocked)?;
                    }
                    continue;
                }
                self.runtime().checked_cycles.insert(task.cycle_id.clone());
            }
            let dependencies: Vec<Option<Task>> = task
                .proposal
                .dependencies
                .iter()
                .map(|id| self.store.get("task", id))
                .collect::<Result<_>>()?;
            if dependencies
                .iter()
                .any(|t| t.as_ref().is_none_or(|t| t.status != Status::Published))
            {
                if dependencies.iter().any(|t| {
                    t.as_ref().is_none_or(|t| {
                        matches!(
                            t.status,
                            Status::Cancelled | Status::Blocked | Status::Failed
                        ) || (t.status != Status::Published
                            && control.batch.as_ref().is_some_and(|batch| {
                                t.run_id.as_deref() != Some(batch.id.as_str())
                            }))
                    })
                }) {
                    task.blocked_reason = Some(BlockedReason::DependencyBlocked);
                    task.error = Some("A dependency is unresolved; retry after it is published or rediscover dependent work".into());
                    self.transition(&mut task, Status::Blocked)?;
                }
                continue;
            }
            if task.proposal.target != c.default_branch && occupied.contains(&task.proposal.target)
            {
                continue;
            }
            if task.proposal.target == task.config.default_branch
                && task.output_commit.is_none()
                && !self.store.has_pr_reservation(&task.id)?
            {
                let Some(inventory) = &fresh_inventory else {
                    continue;
                };
                if !self.store.admit_new_pr_task(&mut task, inventory)? {
                    continue;
                }
            } else {
                self.transition(&mut task, Status::Executing)?;
            }
            occupied.insert(task.proposal.target.clone());
            slots -= 1;
            let cancel = self.shutdown.child_token();
            self.runtime().tasks.insert(task.id.clone(), cancel.clone());
            let app = self.clone();
            tokio::spawn(async move {
                let _guard = app.task_guard(&task.id);
                let mut timed_out = false;
                let result = {
                    let limit = Duration::from_secs(task.execution_config().task_timeout_seconds);
                    match crate::process::with_deadline(
                        limit,
                        &cancel,
                        app.execute(&mut task, &cancel),
                    )
                    .await
                    {
                        Deadline::Done(result) => Ok(result),
                        Deadline::Expired { already_cancelled } => {
                            timed_out = !already_cancelled;
                            Err(())
                        }
                    }
                };
                let error = match result {
                    Ok(Ok(())) => None,
                    Ok(Err(e)) => {
                        task.blocked_reason = Some(BlockedReason::from_error(&e));
                        Some(format!("{e:#}"))
                    }
                    Err(_) => {
                        task.blocked_reason = Some(BlockedReason::Timeout);
                        Some("Task time limit exceeded".into())
                    }
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
                    let operator_cancelled = app
                        .store
                        .get::<serde_json::Value>("cancel", &task.id)
                        .ok()
                        .flatten()
                        .is_some_and(|v| !v.is_null());
                    let status = if cancel.is_cancelled()
                        && !timed_out
                        && task.output_commit.is_none()
                        && (operator_cancelled || !app.shutdown.is_cancelled())
                    {
                        Status::Cancelled
                    } else {
                        Status::Blocked
                    };
                    if let Err(e) = app.transition(&mut task, status) {
                        tracing::error!("Task {} final transition failed: {e:#}", task.id);
                    }
                    if let Err(e) = app.store.event(&task.id, "error", &error) {
                        tracing::error!("Task {} error event failed: {e:#}", task.id);
                    }
                }
            });
        }
        let busy = {
            let rt = self.runtime();
            !rt.tasks.is_empty() || rt.cycle.is_some()
        };
        let ready_to_plan = if let Some(batch) = &control.batch {
            let (pending, failed) = self.store.batch_counts(&batch.id)?;
            pending == 0 && failed == 0 && batch.phase == BatchPhase::Draining
        } else {
            self.store.tasks_with_status(&["queued"])?.is_empty()
        };
        if !busy && ready_to_plan && chrono::Utc::now().timestamp() >= control.next_cycle_at {
            let capacity = self.store.planning_capacity()?;
            if !capacity.available() {
                if control.mode == OperatingMode::RunOnce {
                    control.error = Some(capacity.message());
                    control.set_mode(OperatingMode::Paused);
                    self.store.put("settings", "control", &control)?;
                    self.store
                        .event("system", "planning_capacity", &capacity.message())?;
                }
                return Ok(());
            }
            self.launch_cycle(&c, CycleMode::Execution, &mut control)?;
        }
        Ok(())
    }
    fn launch_cycle(&self, config: &Config, mode: CycleMode, control: &mut Control) -> Result<()> {
        self.store.planning_capacity()?.ensure_available()?;
        control.cycle_number += 1;
        let cycle_id = id();
        let run_id = if mode == CycleMode::Execution {
            control.batch.as_ref().map(|b| b.id.clone())
        } else {
            None
        };
        if let Some(batch) = &mut control.batch {
            batch.phase = BatchPhase::Planning;
            batch.cycle_id = Some(cycle_id.clone());
        }
        let cycle = Cycle {
            mode,
            id: cycle_id,
            number: control.cycle_number,
            status: "running".into(),
            started_at: now(),
            completed_at: None,
            grounding: None,
            proposals: vec![],
            assessments: vec![],
            sessions: vec![],
            error: None,
            repository: config.github_repo.clone(),
            decision_memory: vec![],
            run_id,
            lifecycle: WorkspaceLifecycle::default(),
        };
        self.store.begin_cycle(&cycle, control)?;
        let cancel = self.shutdown.child_token();
        {
            let mut rt = self.runtime();
            rt.cycle = Some(cancel.clone());
            rt.cycle_mode = Some(mode);
        }
        let app = self.clone();
        let config = config.clone();
        tokio::spawn(async move {
            let _guard = CycleGuard {
                app: app.clone(),
                id: cycle.id.clone(),
                mode,
            };
            let result = app.cycle(&config, cycle, &cancel).await;
            app.finish_cycle(mode, result).await;
        });
        Ok(())
    }
    // Caller holds the scheduler gate; audits never unpause the execution queue.
    pub fn start_audit(&self) -> Result<()> {
        let c = self.config()?;
        c.validate_audit()?;
        let mut control = self.control()?;
        {
            let rt = self.runtime();
            ensure!(
                control.paused
                    && rt.tasks.is_empty()
                    && rt.cycle.is_none()
                    && rt.baseline.is_none(),
                "Pause and wait for active work before running an audit"
            );
        }
        control.error = None;
        self.launch_cycle(&c, CycleMode::Audit, &mut control)
    }
    async fn finish_cycle(&self, mode: CycleMode, result: Result<()>) {
        let _gate = self.gate.lock().await;
        if let Ok(mut control) = self.control() {
            control.error = result.err().map(|error| redact(&format!("{error:#}")));
            if mode == CycleMode::Execution {
                if control.error.is_some() && control.mode == OperatingMode::RunOnce {
                    control.set_mode(OperatingMode::Paused);
                }
                if let Ok(config) = self.config() {
                    control.next_cycle_at = chrono::Utc::now().timestamp()
                        + idle_delay(config.cycle_interval_seconds, control.idle_streak) as i64;
                }
            }
            let _ = self.store.put("settings", "control", &control);
        }
        let mut runtime = self.runtime();
        runtime.cycle = None;
        runtime.cycle_mode = None;
    }
    async fn budget(
        &self,
        cycle_id: &str,
        task_id: Option<&str>,
        role: &str,
        route: &Route,
    ) -> Result<()> {
        let dir = self.data_dir.clone();
        let size = tokio::task::spawn_blocking(move || directory_size(&dir)).await??;
        self.store.reserve_session(
            size,
            &crate::store::Admission::new(cycle_id, task_id, role, route),
        )
    }
}

/// Removes a task's scheduler slot on every exit path — normal completion,
/// cancellation and panic — and marks a still-active stored task blocked so a
/// dead worker never leaves work looking runnable.
pub struct TaskGuard {
    app: App,
    id: String,
}
impl Drop for TaskGuard {
    fn drop(&mut self) {
        let _ = (|| -> Result<()> {
            if let Some(mut task) = self.app.store.get::<crate::model::Task>("task", &self.id)?
                && task.status.active()
            {
                task.status = Status::Blocked;
                task.blocked_reason = Some(BlockedReason::Unknown);
                task.error =
                    Some("Task worker exited unexpectedly; inspect the preserved workspace".into());
                self.app.save_task(&mut task)?;
            }
            Ok(())
        })();
        // A successor must not reserve this task until fallback writes finish.
        self.app.runtime().tasks.remove(&self.id);
    }
}
/// A panicked cycle worker must not wedge the scheduler: on unwind this clears
/// the cycle slot and marks a still-running stored cycle interrupted. Normal
/// completion is owned by `finish_cycle`, so the guard only acts on panic —
/// clearing unconditionally could erase a successor cycle's token.
struct CycleGuard {
    app: App,
    id: String,
    mode: CycleMode,
}
impl Drop for CycleGuard {
    fn drop(&mut self) {
        if !std::thread::panicking() {
            return;
        }
        let _ = (|| -> Result<()> {
            if let Some(mut cycle) = self
                .app
                .store
                .get::<crate::model::Cycle>("cycle", &self.id)?
                && cycle.status == "running"
            {
                cycle.status = "interrupted".into();
                cycle.completed_at = Some(now());
                cycle.error = Some("Cycle worker exited unexpectedly".into());
                for session in &mut cycle.sessions {
                    if session.status == "running" {
                        session.status = "interrupted".into();
                    }
                }
                self.app.store.put("cycle", &cycle.id, &cycle)?;
            }
            Ok(())
        })();
        // finish_cycle owns the control bookkeeping (error record, RunOnce pause,
        // backoff) and the rt.cycle clear, but cannot run inside Drop. Leave the
        // panicked token in rt.cycle so the scheduler stays blocked until the
        // spawned task clears it — clearing here would let a successor start and
        // then have its token and batch state erased by the late finish_cycle.
        match tokio::runtime::Handle::try_current() {
            Ok(handle) => {
                let app = self.app.clone();
                let mode = self.mode;
                handle.spawn(async move {
                    app.finish_cycle(
                        mode,
                        Err(anyhow::anyhow!("Cycle worker exited unexpectedly")),
                    )
                    .await;
                });
            }
            Err(_) => {
                // No runtime left to run finish_cycle; unblock the scheduler directly.
                let mut rt = self.app.runtime();
                rt.cycle = None;
                rt.cycle_mode = None;
            }
        }
    }
}
pub fn idle_delay(base: u64, streak: u32) -> u64 {
    base.saturating_mul(1u64 << streak.saturating_sub(1).min(16))
        .min(base.max(86400))
}
