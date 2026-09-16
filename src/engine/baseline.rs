use super::*;
use crate::process::{self, CaptureMode};
use anyhow::Context;
use sha2::{Digest, Sha256};

const COMMAND_OUTPUT_LIMIT: usize = 16 * 1024;
const AGGREGATE_OUTPUT_LIMIT: usize = 1024 * 1024;
const OBSERVATION_FRESH_SECONDS: i64 = 300;

#[derive(Debug)]
pub struct BaselineConflict(pub String);
impl std::fmt::Display for BaselineConflict {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}
impl std::error::Error for BaselineConflict {}
fn conflict(message: &str) -> anyhow::Error {
    BaselineConflict(message.to_owned()).into()
}

pub struct BaselineJob {
    pub id: String,
    pub cancel: CancellationToken,
    pub handle: tokio::task::JoinHandle<()>,
}

pub fn baseline_fingerprint(config: &Config) -> Result<String> {
    Ok(format!("{:x}", Sha256::digest(serde_json::to_vec(config)?)))
}

pub fn bounded_output(text: &str, limit: usize, diagnostic_truncated: bool) -> (String, bool) {
    if limit == 0 {
        return (String::new(), diagnostic_truncated || !text.is_empty());
    }
    let marker = "\n[output truncated]";
    if !diagnostic_truncated && text.len() <= limit {
        return (text.to_owned(), false);
    }
    if limit < marker.len() {
        return (marker[..limit].to_owned(), true);
    }
    let keep = (limit - marker.len()).min(text.len());
    let mut cut = keep;
    while !text.is_char_boundary(cut) {
        cut -= 1;
    }
    (format!("{}{}", &text[..cut], marker), true)
}

pub fn command_output(captured: &Result<process::ProcessOutput>) -> (String, bool, bool) {
    let (text, truncated, success) = match captured {
        Ok(output) => {
            let mut text = String::from_utf8_lossy(&output.stdout.bytes).into_owned();
            if !output.stderr.bytes.is_empty() {
                text.push_str("\n[stderr]\n");
                text.push_str(&String::from_utf8_lossy(&output.stderr.bytes));
            }
            if !output.status.success() {
                text.push_str(&format!("\n{}", output.status));
            }
            (
                text,
                output.stdout.truncated || output.stderr.truncated,
                output.status.success(),
            )
        }
        Err(error) => (format!("{error:#}"), false, false),
    };
    // The middle flag reports capture-level truncation only; bounded_output
    // measures and reports over-limit text itself, in the same byte unit.
    (text.clone(), truncated, success)
}
/// One verification command's captured result plus the workspace-integrity
/// check that follows it. `intact` carries the check's own failure so each
/// caller decides whether it is evidence (baseline) or fatal (task
/// verification).
pub struct CheckOutcome {
    pub captured: Result<process::ProcessOutput>,
    pub intact: Result<bool>,
}
impl CheckOutcome {
    /// The command-failure condition `process::run` reports as `Err`: a capture
    /// failure or a nonzero exit.
    pub fn failed(&self) -> bool {
        !self.captured.as_ref().is_ok_and(|o| o.status.success())
    }
    /// The text `process::run` would have returned for this capture: bounded
    /// diagnostic output on success, the error chain on failure.
    pub fn output_text(&self) -> String {
        match &self.captured {
            Ok(output) => {
                process::diagnostic_text("bash", output).unwrap_or_else(|e| format!("{e:#}"))
            }
            Err(e) => format!("{e:#}"),
        }
    }
}
/// Runs one `bash -o pipefail -c` verification command in `workspace`, then
/// checks the workspace still sits at `revision`. The integrity read is skipped
/// once `cancel` fires: it needs a live process and could only report the
/// cancellation rather than the workspace state.
pub async fn run_check_command(
    config: &Config,
    workspace: &Path,
    command: &str,
    revision: &str,
    cancel: &CancellationToken,
) -> CheckOutcome {
    let captured = process::capture(
        "bash",
        &["-o", "pipefail", "-c", command],
        workspace,
        config.command_timeout_seconds,
        cancel,
        CaptureMode::Diagnostic,
    )
    .await;
    let intact = if cancel.is_cancelled() {
        Ok(false)
    } else {
        git::at(config, workspace, revision, cancel).await
    };
    CheckOutcome { captured, intact }
}

impl App {
    fn baseline_cancelled(&self, id: &str) -> Result<bool> {
        Ok(self
            .store
            .get::<Value>("baseline_cancel", id)?
            .is_some_and(|v| !v.is_null()))
    }
    pub async fn observe_default_branch(
        &self,
        config: &Config,
        revision: &str,
        observed_at: &str,
    ) -> Result<()> {
        let _gate = self.gate.lock().await;
        let live = self.config()?;
        ensure!(
            live.same_remote_identity(config),
            "Configuration identity changed during remote observation"
        );
        let observed =
            chrono::DateTime::parse_from_rfc3339(observed_at)?.with_timezone(&chrono::Utc);
        let mut rt = self.runtime();
        if let Some(existing) = &rt.default_observation {
            let same_target = existing
                .repository
                .eq_ignore_ascii_case(&config.github_repo)
                && existing.default_branch == config.default_branch;
            let newer = chrono::DateTime::parse_from_rfc3339(&existing.observed_at)
                .map(|at| at.with_timezone(&chrono::Utc) >= observed)
                .unwrap_or(true);
            if same_target && newer {
                return Ok(());
            }
        }
        rt.default_observation = Some(DefaultBranchObservation {
            repository: config.github_repo.clone(),
            default_branch: config.default_branch.clone(),
            revision: revision.to_owned(),
            observed_at: observed_at.to_owned(),
        });
        Ok(())
    }
    fn baseline_eligibility(&self) -> Result<(bool, Option<String>)> {
        let control = self.control()?;
        let rt = self.runtime();
        let reason = if rt.baseline.is_some() {
            Some("A baseline check is already running".to_owned())
        } else if self.shutdown.is_cancelled() {
            Some("The service is shutting down".to_owned())
        } else if !control.paused {
            Some("Pause the service before running a baseline check".to_owned())
        } else if !rt.tasks.is_empty() {
            Some("Wait for active tasks before running a baseline check".to_owned())
        } else if rt.cycle.is_some() {
            Some("Wait for planning to finish before running a baseline check".to_owned())
        } else if rt.reconciling_publication {
            Some("Wait for publication reconciliation before running a baseline check".to_owned())
        } else if let Err(error) = self.config()?.validate_baseline() {
            Some(format!("{error:#}"))
        } else {
            None
        };
        Ok((reason.is_none(), reason))
    }
    pub fn start_baseline(&self, expected: &Config) -> Result<BaselineCheck> {
        let live = self.config()?;
        ensure!(
            serde_json::to_value(&live)? == serde_json::to_value(expected)?,
            conflict(
                "The saved configuration changed; reload settings and check the current values"
            )
        );
        live.validate_baseline()?;
        let (eligible, reason) = self.baseline_eligibility()?;
        ensure!(
            eligible,
            conflict(
                reason
                    .as_deref()
                    .unwrap_or("Baseline check is not eligible")
            )
        );
        let check = BaselineCheck {
            id: crate::model::id(),
            status: BaselineStatus::Running,
            config: live.clone(),
            config_fingerprint: baseline_fingerprint(&live)?,
            revision: None,
            started_at: now(),
            completed_at: None,
            commands: vec![],
            error: None,
            workspace_removed: false,
            cleanup_error: None,
        };
        self.store.put("baseline", &check.id, &check)?;
        self.store.put("settings", "baseline_latest", &check.id)?;
        let cancel = self.shutdown.child_token();
        let app = self.clone();
        let id = check.id.clone();
        let token = cancel.clone();
        let handle = tokio::spawn(async move { app.baseline_worker(id, token).await });
        self.runtime().baseline = Some(BaselineJob {
            id: check.id.clone(),
            cancel,
            handle,
        });
        Ok(check)
    }
    pub fn cancel_baseline(&self, id: &str) -> Result<()> {
        let check: BaselineCheck = self
            .store
            .get("baseline", id)?
            .ok_or_else(|| conflict("Baseline check not found"))?;
        ensure!(
            check.status == BaselineStatus::Running,
            conflict("The baseline check already finished")
        );
        let job = self
            .runtime()
            .baseline
            .as_ref()
            .filter(|job| job.id == id)
            .map(|job| job.cancel.clone());
        let cancel = job.ok_or_else(|| conflict("The baseline check is no longer running"))?;
        self.store.put("baseline_cancel", id, &json!(now()))?;
        cancel.cancel();
        Ok(())
    }
    pub fn baseline_config_matches(&self, check: &BaselineCheck, live: &Config) -> bool {
        baseline_fingerprint(live).is_ok_and(|f| f == check.config_fingerprint)
    }
    pub fn baseline_revision_status(
        &self,
        rt: &Runtime,
        check: &BaselineCheck,
        live: &Config,
    ) -> &'static str {
        if !live.same_remote_identity(&check.config) {
            return "unknown";
        }
        let Some(observation) = &rt.default_observation else {
            return "unknown";
        };
        let fresh = chrono::DateTime::parse_from_rfc3339(&observation.observed_at)
            .map(|at| {
                (0..=OBSERVATION_FRESH_SECONDS)
                    .contains(&(chrono::Utc::now() - at.with_timezone(&chrono::Utc)).num_seconds())
            })
            .unwrap_or(false);
        let same_target = observation
            .repository
            .eq_ignore_ascii_case(&check.config.github_repo)
            && observation.default_branch == check.config.default_branch;
        match (&check.revision, fresh && same_target) {
            (Some(revision), true) if *revision == observation.revision => {
                "matches_last_observation"
            }
            (Some(_), true) => "stale",
            _ => "unknown",
        }
    }
    pub fn baseline_view(&self, id: Option<&str>) -> Result<Value> {
        let check = match id {
            Some(id) => self.store.get::<BaselineCheck>("baseline", id)?,
            None => self.store.latest_baseline()?,
        };
        let (eligible, reason) = self.baseline_eligibility()?;
        let live = self.config()?;
        let rt = self.runtime();
        let config_matches = check
            .as_ref()
            .map(|check| self.baseline_config_matches(check, &live));
        let revision_status = check
            .as_ref()
            .map(|check| self.baseline_revision_status(&rt, check, &live))
            .unwrap_or("unknown");
        let observation = rt.default_observation.clone();
        drop(rt);
        Ok(json!({
            "check": check,
            "eligible": eligible,
            "reason": reason,
            "config_matches": config_matches,
            "revision_status": revision_status,
            "default_observation": observation,
            "caveat": "A baseline check verifies the saved commands on a clone made at its start time; it does not prove later host, tool or remote health and is not publication evidence."
        }))
    }
    pub fn recover_baselines(&self) -> Result<()> {
        for mut check in self.store.running_baselines()? {
            check.status = if self.baseline_cancelled(&check.id)? {
                BaselineStatus::Cancelled
            } else {
                BaselineStatus::Interrupted
            };
            check.completed_at = Some(now());
            check.error = Some(
                if check.status == BaselineStatus::Cancelled {
                    "The operator cancelled this check before the service stopped"
                } else {
                    "The service stopped while the baseline check was running"
                }
                .into(),
            );
            self.store.put("baseline", &check.id, &check)?;
        }
        Ok(())
    }
    pub async fn cleanup_baseline(&self, check: &mut BaselineCheck) -> Result<()> {
        let root = self.data_dir.join("baselines");
        uuid::Uuid::parse_str(&check.id).context("Invalid baseline identity")?;
        let path = root.join(&check.id);
        match housekeeping::remove_owned_dir(&root, &path).await {
            Ok(()) => {
                check.workspace_removed = true;
                check.cleanup_error = None;
            }
            Err(error) => {
                check.cleanup_error = Some(error_message(&error));
            }
        }
        self.store.put("baseline", &check.id, check)
    }
    async fn baseline_worker(&self, id: String, cancel: CancellationToken) {
        let _guard = BaselineGuard {
            app: self.clone(),
            id: id.clone(),
        };
        let mut check = match self.store.get::<BaselineCheck>("baseline", &id) {
            Ok(Some(check)) if check.status == BaselineStatus::Running => check,
            _ => return,
        };
        let c = check.config.clone();
        let limit = Duration::from_secs(c.task_timeout_seconds);
        let outcome =
            process::with_deadline(limit, &cancel, self.execute_baseline(&mut check, &cancel))
                .await;
        {
            let _gate = self.gate.lock().await;
            let mut status = match outcome {
                Deadline::Done(Ok(status)) => status,
                Deadline::Done(Err(error)) => {
                    check.error = Some(error_message(&error));
                    if cancel.is_cancelled() {
                        BaselineStatus::Interrupted
                    } else if error
                        .downcast_ref::<tokio::time::error::Elapsed>()
                        .is_some()
                    {
                        BaselineStatus::TimedOut
                    } else {
                        BaselineStatus::Failed
                    }
                }
                Deadline::Expired {
                    already_cancelled: true,
                } => BaselineStatus::Interrupted,
                Deadline::Expired {
                    already_cancelled: false,
                } => {
                    check.error = Some(format!(
                        "Baseline check exceeded the {} second overall limit",
                        c.task_timeout_seconds
                    ));
                    BaselineStatus::TimedOut
                }
            };
            match self.baseline_cancelled(&id) {
                Ok(true) => {
                    status = BaselineStatus::Cancelled;
                    check.error = Some("Cancelled by the operator".into());
                }
                Ok(false) => {
                    if self.shutdown.is_cancelled() && status != BaselineStatus::Passed {
                        status = BaselineStatus::Interrupted;
                    }
                }
                Err(error) => {
                    status = BaselineStatus::Interrupted;
                    check.error = Some(redact(&format!(
                        "Cancel state unreadable, refusing a clean result: {error:#}"
                    )));
                }
            }
            check.status = status;
            check.completed_at = Some(now());
            if let Err(error) = self.store.put("baseline", &id, &check) {
                let _ = self
                    .store
                    .event(&id, "baseline_error", &format!("{error:#}"));
            }
            let _ = self.store.event(&id, "baseline", &format!("{status:?}"));
        }
        if let Err(error) = self.cleanup_baseline(&mut check).await {
            let _ = self
                .store
                .event(&id, "cleanup_error", &format!("{error:#}"));
        }
    }
    async fn execute_baseline(
        &self,
        check: &mut BaselineCheck,
        cancel: &CancellationToken,
    ) -> Result<BaselineStatus> {
        let c = check.config.clone();
        let dir = self.data_dir.clone();
        let measured = tokio::task::spawn_blocking(move || directory_size(&dir)).await??;
        ensure!(
            measured < c.max_workspace_bytes,
            anyhow::Error::new(BlockedReason::StorageLimit).context(format!(
                "Workspace storage limit reached ({measured} bytes). Resolve retained tasks or increase the limit"
            ))
        );
        git::validate_remote(&c, cancel).await?;
        let observed_at = now();
        let revision = git::remote_revision(&c, &c.default_branch, cancel)
            .await?
            .context("Default branch missing on remote")?;
        check.revision = Some(revision.clone());
        self.store.put("baseline", &check.id, &*check)?;
        self.observe_default_branch(&c, &revision, &observed_at)
            .await?;
        git::fetch(&c, cancel).await?;
        let workspace = self
            .data_dir
            .join("baselines")
            .join(&check.id)
            .join("workspace");
        git::clone_at(&c, &workspace, &revision, cancel).await?;
        ensure!(
            git::at(&c, &workspace, &revision, cancel).await?,
            "Cloned workspace does not match the identified revision"
        );
        let mut all_ok = true;
        let mut remaining = AGGREGATE_OUTPUT_LIMIT;
        for command in &c.verification_commands {
            ensure!(!cancel.is_cancelled(), "Operation cancelled");
            ensure!(!self.baseline_cancelled(&check.id)?, "Operation cancelled");
            let outcome = run_check_command(&c, &workspace, command, &revision, cancel).await;
            let timed_out = outcome
                .captured
                .as_ref()
                .err()
                .is_some_and(|e| e.downcast_ref::<tokio::time::error::Elapsed>().is_some());
            let (mut text, diagnostic_truncated, mut success) = command_output(&outcome.captured);
            let mut failure = None;
            if cancel.is_cancelled() || self.baseline_cancelled(&check.id)? {
                success = false;
                failure = Some(anyhow::anyhow!("Operation cancelled"));
            } else {
                match outcome.intact {
                    Ok(true) => {}
                    Ok(false) => {
                        success = false;
                        text.push_str(
                            "\nWorkspace or HEAD changed during this verification command",
                        );
                        failure = Some(anyhow::anyhow!(
                            "Workspace or HEAD changed during verification"
                        ));
                    }
                    Err(error) => {
                        success = false;
                        text.push_str(&format!("\n{error:#}"));
                        failure =
                            Some(error.context("Workspace state check failed during verification"));
                    }
                }
            }
            let (output, output_truncated) = bounded_output(
                &redact(&text),
                remaining.min(COMMAND_OUTPUT_LIMIT),
                diagnostic_truncated,
            );
            remaining = remaining.saturating_sub(output.len());
            check.commands.push(BaselineCommand {
                command: command.clone(),
                success,
                output,
                output_truncated,
                created_at: now(),
            });
            self.store.put("baseline", &check.id, &*check)?;
            if timed_out {
                return Ok(BaselineStatus::TimedOut);
            }
            if let Some(error) = failure {
                return Err(error);
            }
            if !success {
                all_ok = false;
            }
        }
        Ok(if all_ok {
            BaselineStatus::Passed
        } else {
            BaselineStatus::Failed
        })
    }
}

struct BaselineGuard {
    app: App,
    id: String,
}
impl Drop for BaselineGuard {
    fn drop(&mut self) {
        let _ = (|| -> Result<()> {
            if let Some(mut check) = self.app.store.get::<BaselineCheck>("baseline", &self.id)?
                && check.status == BaselineStatus::Running
            {
                check.status = if self.app.baseline_cancelled(&self.id)? {
                    BaselineStatus::Cancelled
                } else {
                    BaselineStatus::Interrupted
                };
                check.completed_at = Some(now());
                check.error = Some(
                    if check.status == BaselineStatus::Cancelled {
                        "Cancelled by the operator; the check worker exited unexpectedly"
                    } else {
                        "Baseline check worker exited unexpectedly"
                    }
                    .into(),
                );
                self.app.store.put("baseline", &check.id, &check)?;
            }
            Ok(())
        })();
        let mut rt = self.app.runtime();
        if rt.baseline.as_ref().is_some_and(|job| job.id == self.id) {
            rt.baseline = None;
        }
    }
}
