use super::App;
use crate::{git, model::*, runner::Runners, schemas, store::redact};
use anyhow::{Context, Result, bail, ensure};
use std::path::{Path, PathBuf};
use tokio_util::sync::CancellationToken;

impl App {
    pub(super) async fn execute(&self, task: &mut Task, cancel: &CancellationToken) -> Result<()> {
        let config = task.config.clone();
        task.error = None;
        self.save_task(task)?;
        if task.output_commit.is_some() {
            self.transition(task, Status::Publishing)?;
            let p = git::publish(task, cancel).await?;
            return self.published(task, p);
        }
        let mut client = Runners::new(&config, self.store.clone(), &task.id, cancel.clone());
        client
            .validate_routes(&config, &self.data_dir, false)
            .await?;
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
                "Executor produced no net changes; task cannot be published"
            );
            ensure!(
                task.reviews.len() < config.max_repair_rounds + 1,
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
                    if task.pr_number.is_none() {
                        ensure!(
                            git::remote_revision(&config, &config.default_branch, cancel)
                                .await?
                                .as_deref()
                                == Some(&task.source_revision),
                            "Default branch moved during execution; preserve and reconcile before publication"
                        );
                    }
                    task.output_commit = Some(revision);
                    self.transition(task, Status::Publishing)?;
                    let p = git::publish(task, cancel).await?;
                    return self.published(task, p);
                }
            }
            ensure!(
                task.reviews.len() <= config.max_repair_rounds,
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
        client: &mut Runners,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let config = task.config.clone();
        git::fetch(&config, cancel).await?;
        let current = git::remote_revision(&config, &task.proposal.target, cancel)
            .await?
            .context("Task target disappeared")?;
        if current != task.source_revision {
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
        ensure!(
            git::remote_revision(&config, &config.default_branch, cancel)
                .await?
                .as_deref()
                == Some(&task.default_revision),
            "Default branch changed after planning; cancel and rediscover against the new context"
        );
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
        uuid::Uuid::parse_str(&task.id).context("Invalid task workspace identity")?;
        let workspace = self.data_dir.join("tasks").join(&task.id).join("workspace");
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
                "Workspace initialization was interrupted; cancel and rediscover rather than overwrite partial files"
            );
            ensure!(
                git::clean(&config, &workspace, cancel).await?
                    && git::git(&config, &workspace, &["rev-parse", "HEAD"], cancel).await?
                        == task.source_revision,
                "Workspace changed before executor session creation; preserve and inspect before retrying"
            );
        }
        let session = client.start(&task.route, &workspace, None).await?;
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
        Ok(())
    }

    async fn run_executor(
        &self,
        task: &mut Task,
        client: &mut Runners,
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
        client: &mut Runners,
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
                Some(schemas::review_schema()),
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
        client: &mut Runners,
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
