use super::App;
use crate::{
    codex::{self, Codex},
    git,
    model::*,
    store::redact,
};
use anyhow::{Context, Result, bail, ensure};
use std::path::PathBuf;
use tokio_util::sync::CancellationToken;

impl App {
    pub(super) async fn execute(&self, task: &mut Task, cancel: &CancellationToken) -> Result<()> {
        let config = task.config.clone();
        task.error = None;
        self.save_task(task)?;
        if task.output_commit.is_some() {
            // Never rebase work that already reached the remote; only refresh the recorded context.
            let pushed = task.pr_number.is_some()
                || git::remote_revision(&config, &task.branch, cancel)
                    .await?
                    .is_some();
            let rebase = if pushed {
                None
            } else {
                Some("pre_publication")
            };
            if self.reconcile_default_branch(task, rebase, cancel).await? {
                task.output_commit = None;
                self.save_task(task)?;
            } else {
                self.transition(task, Status::Publishing)?;
                let p = git::publish(task, cancel).await?;
                return self.published(task, p);
            }
        }
        let mut client = Codex::connect(
            &config,
            &self.data_dir,
            self.store.clone(),
            &task.id,
            cancel.clone(),
        )
        .await?;
        codex::validate_routes(&config, &client.models().await?)?;
        // Initialization reserves the first executor admission, including on retries.
        let admission_reserved = task.execution_session.is_none();
        if admission_reserved {
            self.initialize_task(task, &mut client, cancel).await?;
        }
        let workspace = PathBuf::from(&task.workspace);
        ensure!(
            workspace.join(".git").exists(),
            "Workspace initialization was interrupted; cancel and rediscover rather than overwrite partial files"
        );
        if task.comparison_base.is_empty() {
            bail!("Comparison base was not persisted; cancel this task and rediscover");
        }
        self.run_executor(task, &mut client, admission_reserved)
            .await?;
        let mut previous = String::new();
        let mut no_progress = 0;
        loop {
            let mut revision =
                git::snapshot(&config, &workspace, &task.proposal.title, cancel).await?;
            // Rebase before reviewing so the review covers the code that will be published.
            if self
                .reconcile_default_branch(task, Some("pre_review"), cancel)
                .await?
            {
                revision = git::git(&config, &workspace, &["rev-parse", "HEAD"], cancel).await?;
            }
            // Checked after any rebase: a moved default branch may already contain the change.
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
                if task.rebases() > 0 {
                    "Executor produced no net changes against the moved default branch; the improvement may already be present. Task cannot be published"
                } else {
                    "Executor produced no net changes; task cannot be published"
                }
            );
            ensure!(
                task.review_rounds() < config.max_repair_rounds + 1,
                "Review/repair round limit exhausted; unresolved work is preserved"
            );
            let review = self
                .review_revision(task, &mut client, &revision, cancel)
                .await?;
            let mut verification_errors = vec![];
            if review.clean() {
                verification_errors = self.verify_revision(task, &revision, cancel).await?;
                if verification_errors.is_empty() {
                    // Main movement changes the integration context; never silently publish an obsolete review.
                    if self
                        .reconcile_default_branch(task, Some("pre_publication"), cancel)
                        .await?
                    {
                        continue;
                    }
                    task.output_commit = Some(revision);
                    self.transition(task, Status::Publishing)?;
                    let p = git::publish(task, cancel).await?;
                    return self.published(task, p);
                }
            }
            ensure!(
                task.review_rounds() <= config.max_repair_rounds,
                "Repair limit exhausted; unresolved findings or verification failures remain"
            );
            if previous == revision {
                no_progress += 1;
            } else {
                no_progress = 0;
                previous = revision;
            }
            ensure!(
                no_progress < config.max_no_progress_rounds,
                "Repairs made no progress; workspace preserved"
            );
            self.repair(task, &mut client, &review, &verification_errors)
                .await?;
        }
    }
    async fn initialize_task(
        &self,
        task: &mut Task,
        client: &mut Codex,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let config = task.config.clone();
        git::fetch(&config, cancel).await?;
        let current = git::remote_revision(&config, &task.proposal.target, cancel)
            .await?
            .context("Task target disappeared")?;
        if current != task.source_revision && task.pr_number.is_none() {
            // New default-branch work has no workspace yet: adopt the moved revision outright.
            ensure!(
                config.max_reconciliations > 0,
                "Source changed outside the declared dependency chain. Cancel this stale task and rediscover against the new revision"
            );
            let from = std::mem::replace(&mut task.source_revision, current.clone());
            task.default_revision = current.clone();
            task.proposal.prompt.push_str(&format!(
                "\nThe default branch moved to {current} after planning. Inspect the current code first; if this improvement is already present, make no changes and say so."
            ));
            self.record_reconciliation(task, "initialization", &from, &current)?;
        } else if current != task.source_revision {
            let mut dependency_outputs = Vec::new();
            for identity in &task.proposal.dependencies {
                let dependency: Task = self
                    .store
                    .get("task", identity)?
                    .context("Dependency record is missing")?;
                ensure!(
                    dependency.status == Status::Published
                        && dependency.branch == task.proposal.target,
                    "Dependency has not been delivered to the target branch"
                );
                dependency_outputs.push(
                    dependency
                        .output_commit
                        .context("Dependency output revision is missing")?,
                );
            }
            ensure!(
                dependency_outputs.contains(&current),
                "Source changed outside the declared dependency chain. Cancel this stale task and rediscover against the new revision"
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
            task.proposal.prompt.push_str(&format!("\nPrerequisite work is now present in the target branch at {}. Inspect its accumulated diff before implementing this follow-up.", task.source_revision));
            self.save_task(task)?;
        }
        if task.pr_number.is_some() {
            // Existing-PR work keeps its merge-base comparison; only the recorded context moves.
            self.reconcile_default_branch(task, None, cancel).await?;
        }
        if let Some(n) = task.pr_number {
            let p = git::pr(&config, n, cancel).await?;
            ensure!(
                p.owned && p.state == "open" && p.base == config.default_branch,
                "Target PR is no longer eligible"
            );
        }
        self.budget(
            &config,
            &task.cycle_id,
            Some(&task.id),
            "executor",
            &task.route,
        )
        .await?;
        let session = client.start(&task.route, &self.data_dir, None).await?;
        let workspace = self.data_dir.join("tasks").join(&session).join("workspace");
        task.workspace = workspace.to_string_lossy().into_owned();
        task.execution_session = Some(session.clone());
        task.sessions.push(Session {
            id: session,
            role: "executor".into(),
            route: task.route.clone(),
            status: "running".into(),
            started_at: now(),
            summary: String::new(),
        });
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
        Ok(())
    }

    /// Responds to default-branch movement since the recorded context. Existing-PR work and
    /// pushed work only refresh the recorded revision (`rebase` is ignored); unpublished new-branch
    /// work is rebased onto the moved revision with the given stage. Returns true after a rebase,
    /// which always requires a fresh review before publication.
    async fn reconcile_default_branch(
        &self,
        task: &mut Task,
        rebase: Option<&str>,
        cancel: &CancellationToken,
    ) -> Result<bool> {
        let config = task.config.clone();
        git::fetch(&config, cancel).await?;
        let current = git::remote_revision(&config, &config.default_branch, cancel)
            .await?
            .context("Default branch missing on remote")?;
        if current == task.default_revision {
            return Ok(false);
        }
        ensure!(
            config.max_reconciliations > 0,
            "Default branch moved during execution; preserve and reconcile before publication"
        );
        let from = task.default_revision.clone();
        let Some(stage) = rebase.filter(|_| task.pr_number.is_none()) else {
            task.default_revision = current.clone();
            self.record_reconciliation(task, "default_refresh", &from, &current)?;
            return Ok(false);
        };
        ensure!(
            task.rebases() < config.max_reconciliations,
            "Reconciliation limit ({}) reached; the default branch keeps moving. Inspect the workspace, then retry or cancel",
            config.max_reconciliations
        );
        let workspace = PathBuf::from(&task.workspace);
        let head = git::rebase_onto(&config, &workspace, &current, cancel).await?;
        task.source_revision = current.clone();
        task.default_revision = current.clone();
        task.comparison_base = current.clone();
        self.record_reconciliation(task, stage, &from, &current)?;
        self.store.event(
            &task.id,
            "reconciliation",
            &format!("Rebased onto {current} at {head}; a fresh review and verification follow"),
        )?;
        Ok(true)
    }

    fn record_reconciliation(
        &self,
        task: &mut Task,
        stage: &str,
        from: &str,
        to: &str,
    ) -> Result<()> {
        task.reconciliations.push(Reconciliation {
            stage: stage.into(),
            from: from.into(),
            to: to.into(),
            at: now(),
        });
        self.save_task(task)?;
        self.store.event(
            &task.id,
            "reconciliation",
            &format!("Default branch moved from {from} to {to}: {stage}"),
        )
    }

    async fn run_executor(
        &self,
        task: &mut Task,
        client: &mut Codex,
        admission_reserved: bool,
    ) -> Result<()> {
        let config = task.config.clone();
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
                self.budget(
                    &config,
                    &task.cycle_id,
                    Some(&task.id),
                    "executor",
                    &task.route,
                )
                .await?;
            }
            client.start(&task.route, &workspace, Some(&thread)).await?;
            session_mut(task, &thread, "executor")?.status = "running".into();
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
            let s = session_mut(task, &thread, "executor")?;
            s.status = "completed".into();
            s.summary = redact(&answer);
            self.save_task(task)?;
        }
        Ok(())
    }

    async fn review_revision(
        &self,
        task: &mut Task,
        client: &mut Codex,
        revision: &str,
        cancel: &CancellationToken,
    ) -> Result<Review> {
        let config = task.config.clone();
        let workspace = PathBuf::from(&task.workspace);
        self.transition(task, Status::Reviewing)?;
        let route = &config.roles["code_reviewer"];
        self.budget(&config, &task.cycle_id, Some(&task.id), "reviewer", route)
            .await?;
        let thread = client.start(route, &workspace, None).await?;
        task.sessions.push(Session {
            id: thread.clone(),
            role: "reviewer".into(),
            route: route.clone(),
            status: "running".into(),
            started_at: now(),
            summary: String::new(),
        });
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
                Some(codex::review_schema()),
            )
            .await?;
        session_mut(task, &thread, "reviewer")?.summary = redact(&answer);
        self.save_task(task)?;
        let review: Review =
            serde_json::from_str(&answer).context("Unparseable review is not clean")?;
        ensure!(
            review.completed && !review.summary.trim().is_empty(),
            "Incomplete review is not clean"
        );
        ensure!(
            git::clean(&config, &workspace, cancel).await?
                && git::git(&config, &workspace, &["rev-parse", "HEAD"], cancel).await? == revision,
            "Reviewer modified the reviewed revision"
        );
        let s = session_mut(task, &thread, "reviewer")?;
        s.status = "completed".into();
        s.summary = redact(&review.summary);
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

    async fn verify_revision(
        &self,
        task: &mut Task,
        revision: &str,
        cancel: &CancellationToken,
    ) -> Result<Vec<String>> {
        let config = task.config.clone();
        let workspace = PathBuf::from(&task.workspace);
        let mut verification_errors = Vec::new();
        self.transition(task, Status::Verifying)?;
        for command in &config.verification_commands {
            let result = crate::process::run(
                "bash",
                &["-o", "pipefail", "-c", command],
                &workspace,
                config.command_timeout_seconds,
                cancel,
            )
            .await;
            let success = result.is_ok();
            let output = match result {
                Ok(s) => s,
                Err(e) => format!("{e:#}"),
            };
            if !success {
                verification_errors.push(format!("{command}: {output}"));
            }
            task.verification.push(Verification {
                command: command.clone(),
                success,
                output: redact(&output),
                revision: revision.into(),
                created_at: now(),
            });
            self.save_task(task)?;
        }
        ensure!(
            git::clean(&config, &workspace, cancel).await?
                && git::git(&config, &workspace, &["rev-parse", "HEAD"], cancel).await? == revision,
            "Verification modified the reviewed tree; inspect before retrying"
        );
        Ok(verification_errors)
    }

    async fn repair(
        &self,
        task: &mut Task,
        client: &mut Codex,
        review: &Review,
        verification_errors: &[String],
    ) -> Result<()> {
        let config = task.config.clone();
        let workspace = PathBuf::from(&task.workspace);
        self.transition(task, Status::Repairing)?;
        if let Some(thread) = task.repair_session.clone() {
            session_mut(task, &thread, "repair")?;
        }
        let route = config.repair_route.clone();
        self.budget(&config, &task.cycle_id, Some(&task.id), "repair", &route)
            .await?;
        let thread = client
            .start(&route, &workspace, task.repair_session.as_deref())
            .await?;
        if task.repair_session.is_none() {
            task.repair_session = Some(thread.clone());
            task.sessions.push(Session {
                id: thread.clone(),
                role: "repair".into(),
                route: route.clone(),
                status: "running".into(),
                started_at: now(),
                summary: String::new(),
            });
            self.save_task(task)?;
        }
        session_mut(task, &thread, "repair")?.status = "running".into();
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
        let s = session_mut(task, &thread, "repair")?;
        s.status = "completed".into();
        s.summary = redact(&answer);
        self.save_task(task)?;
        Ok(())
    }

    fn published(&self, task: &mut Task, p: PullRequest) -> Result<()> {
        task.pr_number = Some(p.number);
        task.pr_url = Some(p.url);
        task.error = None;
        self.transition(task, Status::Published)
    }
}

fn session_mut<'a>(task: &'a mut Task, thread: &str, role: &str) -> Result<&'a mut Session> {
    task.sessions
        .iter_mut()
        .find(|session| session.id == thread && session.role == role)
        .with_context(|| {
            format!(
                "Task {} is missing its {role} session record ({thread})",
                task.id
            )
        })
}
