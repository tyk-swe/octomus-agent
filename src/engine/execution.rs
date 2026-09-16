use super::{App, baseline};
use crate::process::Deadline;
use crate::{config::Config, git, model::*, runner::Runners, schemas, store::redact};
use anyhow::{Context, Result, ensure};
use std::path::{Path, PathBuf};
use std::time::Duration;
use tokio_util::sync::CancellationToken;

impl App {
    pub(super) async fn execute(&self, task: &mut Task, cancel: &CancellationToken) -> Result<()> {
        let config = task.execution_config();
        task.error = None;
        task.blocked_reason = None;
        self.save_task(task)?;
        if task.output_commit.is_some() {
            return self.publish_reviewed(task, cancel).await;
        }
        self.retry_preflight(task, cancel).await?;
        let mut client = Runners::new(&config, self.store.clone(), &task.id, cancel.clone());
        client
            .validate_routes(&config, &self.data_dir, false)
            .await
            .context(BlockedReason::RunnerUnavailable)?;
        // Initialization reserves the first executor admission, including on retries.
        let admission_reserved = task.execution_session.is_none();
        if admission_reserved {
            self.initialize_task(task, &mut client, cancel).await?;
        }
        let workspace = PathBuf::from(&task.workspace);
        ensure!(
            workspace.join(".git").exists(),
            BlockedReason::WorkspaceInvalid
        );
        if task.comparison_base.is_empty() {
            return Err(anyhow::Error::new(BlockedReason::WorkspaceInvalid)
                .context("Comparison base was not persisted; cancel this task and rediscover"));
        }
        self.run_executor(task, &mut client, admission_reserved)
            .await?;
        let mut previous = String::new();
        let mut no_progress = 0;
        loop {
            let revision = git::snapshot(&config, &workspace, &task.proposal.title, cancel).await?;
            ensure!(
                revision != task.source_revision
                    && !git::git(
                        &config,
                        &workspace,
                        &["diff", "--name-only", &task.source_revision, &revision],
                        cancel
                    )
                    .await?
                    .is_empty(),
                BlockedReason::VerificationFailed
            );
            ensure!(
                task.attempt_reviews() < config.max_repair_rounds + 1,
                BlockedReason::RetryLimit
            );
            let review = self
                .review_revision(task, &mut client, &revision, cancel)
                .await?;
            let mut verification_errors = vec![];
            if review.clean() {
                verification_errors = self.verify_revision(task, &revision, cancel).await?;
                if verification_errors.is_empty() {
                    // Main movement changes the integration context; never silently publish an obsolete review.
                    if task.pr_number.is_none() {
                        ensure!(
                            git::remote_revision(&config, &config.default_branch, cancel)
                                .await?
                                .as_deref()
                                == Some(&task.source_revision),
                            BlockedReason::StaleBase
                        );
                    }
                    task.output_commit = Some(revision);
                    return self.publish_reviewed(task, cancel).await;
                }
            }
            ensure!(
                task.attempt_reviews() <= config.max_repair_rounds,
                BlockedReason::VerificationFailed
            );
            if previous == revision {
                no_progress += 1;
            } else {
                no_progress = 0;
                previous = revision;
            }
            ensure!(
                no_progress < config.max_no_progress_rounds,
                BlockedReason::VerificationFailed
            );
            self.repair(task, &mut client, &review, &verification_errors)
                .await?;
        }
    }
    /// The shared publication tail once a task's output is recorded: transition,
    /// publish, record.
    async fn publish_reviewed(&self, task: &mut Task, cancel: &CancellationToken) -> Result<()> {
        self.transition(task, Status::Publishing)?;
        let p = git::publish(task, cancel).await?;
        self.published(task, p)
    }
    /// A dependency that is recorded published; anything else blocks the dependent.
    fn published_dependency(&self, id: &str) -> Result<Task> {
        let dependency: Task = self
            .store
            .get("task", id)?
            .context(BlockedReason::DependencyBlocked)?;
        ensure!(
            dependency.status == Status::Published,
            BlockedReason::DependencyBlocked
        );
        Ok(dependency)
    }
    /// The recorded workspace must still sit cleanly at `revision`; anything else
    /// means recorded evidence does not describe the current tree.
    async fn ensure_workspace_at(
        config: &Config,
        workspace: &Path,
        revision: &str,
        cancel: &CancellationToken,
    ) -> Result<()> {
        ensure!(
            git::at(config, workspace, revision, cancel).await?,
            BlockedReason::WorkspaceInvalid
        );
        Ok(())
    }
    pub async fn retry_preflight(&self, task: &Task, cancel: &CancellationToken) -> Result<()> {
        let c = task.execution_config();
        ensure!(
            task.lifecycle.discarded_at.is_none() && task.lifecycle.archived_at.is_none(),
            BlockedReason::WorkspaceInvalid
        );
        let default = git::remote_revision(&c, &c.default_branch, cancel).await?;
        ensure!(
            default.as_deref() == Some(&task.default_revision),
            BlockedReason::StaleBase
        );
        let source = git::remote_revision(&c, &task.proposal.target, cancel).await?;
        let mut authorized = source.as_deref() == Some(&task.source_revision);
        for id in &task.proposal.dependencies {
            let dependency = self.published_dependency(id)?;
            if task.execution_session.is_none() && source == dependency.output_commit {
                authorized = true;
            }
        }
        ensure!(authorized, BlockedReason::StaleBase);
        if task.execution_session.is_some() {
            ensure!(
                !task.comparison_base.is_empty()
                    && Path::new(&task.workspace).join(".git").is_dir(),
                BlockedReason::WorkspaceInvalid
            );
        }
        Ok(())
    }
    async fn initialize_task(
        &self,
        task: &mut Task,
        client: &mut Runners,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let config = task.execution_config();
        git::fetch(&config, cancel).await?;
        let current = git::remote_revision(&config, &task.proposal.target, cancel)
            .await?
            .context(BlockedReason::StaleBase)?;
        if current != task.source_revision {
            let mut dependency_outputs = Vec::new();
            for identity in &task.proposal.dependencies {
                let dependency = self.published_dependency(identity)?;
                ensure!(
                    dependency.branch == task.proposal.target,
                    BlockedReason::DependencyBlocked
                );
                dependency_outputs.push(
                    dependency
                        .output_commit
                        .context("Dependency output revision is missing")?,
                );
            }
            ensure!(
                dependency_outputs.contains(&current),
                BlockedReason::StaleBase
            );
            for output in &dependency_outputs {
                git::git(
                    &config,
                    &config.repository,
                    &["merge-base", "--is-ancestor", output, &current],
                    cancel,
                )
                .await?;
            }
            task.source_revision = current;

            self.save_task(task)?;
        }
        ensure!(
            git::remote_revision(&config, &config.default_branch, cancel)
                .await?
                .as_deref()
                == Some(&task.default_revision),
            BlockedReason::StaleBase
        );
        if let Some(n) = task.pr_number {
            let p = git::pr(&config, n, cancel).await?;
            ensure!(
                p.owned && p.state == "open" && p.base == config.default_branch,
                BlockedReason::StaleBase
            );
        }
        self.budget(&task.cycle_id, Some(&task.id), "executor", &task.route)
            .await?;
        uuid::Uuid::parse_str(&task.id).context("Invalid task workspace identity")?;
        let workspace = self.task_workspace(&task.id);
        if task.workspace.is_empty() {
            task.workspace = workspace.to_string_lossy().into_owned();
            self.save_task(task)?;
            git::clone_at(&config, &workspace, &task.source_revision, cancel).await?;
            task.comparison_base = if task.pr_number.is_some() {
                let base = git::remote_revision(&config, &config.default_branch, cancel)
                    .await?
                    .context("Default branch missing")?;
                git::git(
                    &config,
                    &workspace,
                    &["merge-base", &base, &task.source_revision],
                    cancel,
                )
                .await?
            } else {
                task.source_revision.clone()
            };
            self.save_task(task)?;
        } else {
            // A failed runner start can be retried in a fully initialized clone. Partial clones
            // and edits made before a session was recorded are preserved for operator inspection.
            ensure!(
                Path::new(&task.workspace) == workspace
                    && !task.comparison_base.is_empty()
                    && workspace.join(".git").exists(),
                BlockedReason::WorkspaceInvalid
            );
            Self::ensure_workspace_at(&config, &workspace, &task.source_revision, cancel).await?;
        }
        let session = client.start(&task.route, &workspace, None).await?;
        task.execution_session = Some(session.clone());
        task.sessions
            .push(Session::new(session, "executor", task.route.clone()));
        self.save_task(task)?;
        Ok(())
    }

    async fn run_executor(
        &self,
        task: &mut Task,
        client: &mut Runners,
        admission_reserved: bool,
    ) -> Result<()> {
        let config = task.execution_config();
        let workspace = PathBuf::from(&task.workspace);
        if !task
            .sessions
            .iter()
            .any(|s| s.role == "executor" && s.status == "completed")
        {
            let thread = task
                .execution_session
                .clone()
                .context("Executor session identity is missing")?;
            session_mut(task, &thread, "executor")?;
            if !admission_reserved {
                self.budget(&task.cycle_id, Some(&task.id), "executor", &task.route)
                    .await?;
                client.start(&task.route, &workspace, Some(&thread)).await?;
            }
            // A freshly created thread is already active; Codex has no resumable
            // rollout until its first turn starts.
            session_mut(task, &thread, "executor")?.status = session_status::RUNNING.into();
            self.save_task(task)?;
            let prompt = format!(
                "Implement this accepted task end to end in this workspace. Source revision: {}. Full comparison base: {}. Existing PR: {:?}. Preserve existing accumulated branch behavior; inspect its full diff. Do not push, publish, merge or deploy. Required repository verification commands: {:?}. Objective and constraints:\n{}\nProblem: {}\nBenefit: {}\nScope: {}\nEvidence: {:?}\nReturn a concise summary of actual changes, verification and material risks or migration notes.",
                task.source_revision,
                task.comparison_base,
                task.pr_url,
                config.verification_commands,
                task.proposal.prompt,
                task.proposal.problem,
                task.proposal.benefit,
                task.proposal.scope,
                task.proposal.evidence
            );
            let answer = client
                .turn(&thread, &task.route, &workspace, &prompt, None)
                .await?;
            session_mut(task, &thread, "executor")?.mark_completed(redact(&answer));
            self.save_task(task)?;
        }
        Ok(())
    }

    async fn review_revision(
        &self,
        task: &mut Task,
        client: &mut Runners,
        revision: &str,
        cancel: &CancellationToken,
    ) -> Result<Review> {
        let config = task.execution_config();
        let workspace = PathBuf::from(&task.workspace);
        self.transition(task, Status::Reviewing)?;
        let route = &config.roles["code_reviewer"];
        self.budget(&task.cycle_id, Some(&task.id), "reviewer", route)
            .await?;
        let thread = client.start(route, &workspace, None).await?;
        task.sessions
            .push(Session::new(thread.clone(), "reviewer", route.clone()));
        self.save_task(task)?;
        let prompt = format!(
            "Perform a fresh code review equivalent to /review of the COMPLETE change set: git diff {} HEAD. Recorded HEAD: {revision}. Include all accumulated PR changes and all repairs; do not only review the last commit. Task: {}. Scope: {}. Existing PR: {:?}. Inspect code and evidence, do not modify files. Report actionable correctness, regression, design or missing verification findings with file, priority and technical rationale. Do not invent findings. Set completed=true only after completing the review. A clean review must have an explanatory summary and zero findings.",
            task.comparison_base, task.proposal.prompt, task.proposal.scope, task.pr_url
        );
        let answer = client
            .turn(
                &thread,
                route,
                &workspace,
                &prompt,
                Some(schemas::review_schema()),
            )
            .await?;
        session_mut(task, &thread, "reviewer")?.summary = redact(&answer);
        self.save_task(task)?;
        let review: Review = serde_json::from_str(&answer)
            .context("Unparseable review is not clean")
            .context(BlockedReason::InvalidReview)?;
        ensure!(
            review.completed && !review.summary.trim().is_empty(),
            BlockedReason::InvalidReview
        );
        Self::ensure_workspace_at(&config, &workspace, revision, cancel).await?;
        session_mut(task, &thread, "reviewer")?.mark_completed(redact(&review.summary));
        task.reviews.push(ReviewRound {
            session_id: thread,
            revision: revision.into(),
            comparison_base: task.comparison_base.clone(),
            result: review.clone(),
            created_at: now(),
        });
        self.save_task(task)?;
        Ok(review)
    }

    /// Runs every configured verification command against exactly `revision`.
    /// Worktree and HEAD are checked before the first command and after each one, so a
    /// command that changes tracked state is recorded as failed evidence and stops the run
    /// instead of lending its success to the reviewed revision.
    pub async fn verify_revision(
        &self,
        task: &mut Task,
        revision: &str,
        cancel: &CancellationToken,
    ) -> Result<Vec<String>> {
        let config = task.execution_config();
        let workspace = PathBuf::from(&task.workspace);
        let mut verification_errors = Vec::new();
        self.transition(task, Status::Verifying)?;
        Self::ensure_workspace_at(&config, &workspace, revision, cancel).await?;
        for command in &config.verification_commands {
            let outcome =
                baseline::run_check_command(&config, &workspace, command, revision, cancel).await;
            ensure!(!cancel.is_cancelled(), "Operation cancelled");
            let failed = outcome.failed();
            let mut output = outcome.output_text();
            let intact = outcome.intact?;
            if !intact {
                output.push_str("\nWorkspace or HEAD changed during this verification command");
            }
            task.verification.push(Verification {
                command: command.clone(),
                success: intact && !failed,
                output: redact(&output),
                revision: revision.into(),
                created_at: now(),
            });
            self.save_task(task)?;
            ensure!(intact, BlockedReason::WorkspaceInvalid);
            if failed {
                verification_errors.push(format!("{command}: {output}"));
            }
        }
        Ok(verification_errors)
    }

    async fn repair(
        &self,
        task: &mut Task,
        client: &mut Runners,
        review: &Review,
        verification_errors: &[String],
    ) -> Result<()> {
        let config = task.execution_config();
        let workspace = PathBuf::from(&task.workspace);
        self.transition(task, Status::Repairing)?;
        if let Some(thread) = task.repair_session.clone() {
            session_mut(task, &thread, "repair")?;
        }
        let route = config.repair_route.clone();
        self.budget(&task.cycle_id, Some(&task.id), "repair", &route)
            .await?;
        let thread = client
            .start(&route, &workspace, task.repair_session.as_deref())
            .await?;
        if task.repair_session.is_none() {
            task.repair_session = Some(thread.clone());
            task.sessions
                .push(Session::new(thread.clone(), "repair", route.clone()));
            self.save_task(task)?;
        }
        session_mut(task, &thread, "repair")?.status = session_status::RUNNING.into();
        self.save_task(task)?;
        let prompt = format!(
            "Repair actionable findings and verification failures for this task. Preserve useful capabilities and meaningful tests. Do not push, publish, merge or deploy. If a finding is unsupported, explain the technical evidence in your final summary; the next fresh reviewer must independently assess it. Rerun relevant verification {:?}. Full comparison base: {}. Task: {}. Findings: {}. Verification failures: {:?}",
            config.verification_commands,
            task.comparison_base,
            task.proposal.prompt,
            serde_json::to_string(&review.findings)?,
            verification_errors
        );
        let answer = client
            .turn(&thread, &route, &workspace, &prompt, None)
            .await?;
        session_mut(task, &thread, "repair")?.mark_completed(redact(&answer));
        self.save_task(task)?;
        Ok(())
    }

    pub(crate) fn published(&self, task: &mut Task, p: PullRequest) -> Result<()> {
        task.pr_number = Some(p.number);
        task.pr_url = Some(p.url.clone());
        task.error = None;
        task.blocked_reason = None;
        self.transition(task, Status::Published)?;
        self.observe_delivery(&task.config, p)
    }
}

fn session_mut<'a>(task: &'a mut Task, thread: &str, role: &str) -> Result<&'a mut Session> {
    task.sessions
        .iter_mut()
        .find(|session| session.id == thread && session.role == role)
        .ok_or_else(|| {
            anyhow::Error::new(BlockedReason::WorkspaceInvalid).context(format!(
                "Task {} is missing its {role} session record ({thread})",
                task.id
            ))
        })
}

/// Runs one task to a terminal state and records why it ended there.
///
/// Every exit path writes a status: a deadline, a cancellation and a failure are
/// all distinguishable afterwards, because a task that simply stopped being
/// mentioned would be indistinguishable from one still running.
pub(super) async fn supervise(app: App, mut task: Task, cancel: CancellationToken) {
    let _guard = app.task_guard(&task.id);
    let mut timed_out = false;
    let result = {
        let limit = Duration::from_secs(task.execution_config().task_timeout_seconds);
        match crate::process::with_deadline(limit, &cancel, app.execute(&mut task, &cancel)).await {
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
        fail_running(&mut task.sessions, &redact(&error));
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
}
