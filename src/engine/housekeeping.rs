use super::*;
use anyhow::Context;
use serde::Serialize;

#[derive(Serialize)]
struct StorageUsage {
    measured_at: String,
    application_bytes: u64,
    task_bytes: u64,
    planning_bytes: u64,
    runner_transcripts: Value,
}
impl App {
    pub(super) fn schedule_housekeeping(&self) {
        let now = chrono::Utc::now().timestamp();
        let (cleanup, observe) = {
            let mut rt = self.runtime();
            if rt.housekeeping.as_ref().is_some_and(|h| !h.is_finished()) {
                return;
            }
            rt.housekeeping = None;
            let cleanup = now - rt.last_retention_at >= 900;
            let observe = now - rt.last_observation_at >= 300;
            if !cleanup && !observe {
                return;
            }
            if cleanup {
                rt.last_retention_at = now;
            }
            if observe {
                rt.last_observation_at = now;
            }
            (cleanup, observe)
        };
        let app = self.clone();
        let handle = tokio::spawn(async move {
            let work = async {
                let c = app.config()?;
                if cleanup {
                    app.retention(&c).await?;
                    app.measure_storage(&c).await?;
                }
                if observe && !c.github_repo.is_empty() && c.repository.is_dir() {
                    app.observe_remote(&c).await?;
                }
                Ok::<_, anyhow::Error>(())
            }
            .await;
            if let Err(error) = work {
                let _ = app
                    .store
                    .event("system", "housekeeping_error", &format!("{error:#}"));
            }
        });
        self.runtime().housekeeping = Some(handle);
    }
    async fn measure_storage(&self, c: &Config) -> Result<()> {
        let dir = self.data_dir.clone();
        let roots = c.runner_storage_paths.clone();
        let usage=tokio::task::spawn_blocking(move || -> Result<_> {
            let mut runners=serde_json::Map::new();
            let mut total=0u64;
            let mut measured=0;
            for backend in ["codex","opencode"] {
                let (bytes,status)=match roots.get(backend) {
                    None=>(None,"unconfigured"),
                    Some(p) if !p.is_dir()=>(None,"unavailable"),
                    Some(p)=>match directory_size(p) {
                        Ok(b)=>(Some(b),"measured"),
                        Err(_)=>(None,"error"),
                    },
                };
                if let Some(b)=bytes { total=total.saturating_add(b); measured+=1; }
                runners.insert(backend.into(),json!({"bytes":bytes,"status":status}));
            }
            let status=if measured==runners.len(){"measured"}else if measured==0{"unavailable"}else{"partial"};
            Ok(StorageUsage { measured_at:now(),application_bytes:directory_size(&dir)?,
                task_bytes:directory_size(&dir.join("tasks"))?,planning_bytes:directory_size(&dir.join("cycles"))?,
                runner_transcripts:json!({"bytes":if measured>0{Some(total)}else{None},"status":status,"runners":runners,"message":"Runner storage reported separately. Application admission measures the data directory."}) })
        }).await??;
        self.store.put("settings", "storage", &usage)
    }
    async fn retention(&self, c: &Config) -> Result<()> {
        self.store.prune_events(c.retain_events)?;
        let cutoff = (chrono::Utc::now()
            - chrono::Duration::days(c.retain_completed_days.min(36500) as i64))
        .to_rfc3339();
        for mut check in self.store.baseline_cleanup_candidates()? {
            if self.shutdown.is_cancelled() {
                return Ok(());
            }
            {
                let _gate = self.gate.lock().await;
                let current: Option<crate::model::BaselineCheck> =
                    self.store.get("baseline", &check.id)?;
                let terminal = current
                    .as_ref()
                    .is_some_and(|c| c.status != crate::model::BaselineStatus::Running);
                let active = self
                    .runtime()
                    .baseline
                    .as_ref()
                    .is_some_and(|job| job.id == check.id);
                if !terminal || active {
                    continue;
                }
                if let Some(current) = current {
                    check = current;
                }
            }
            if let Err(error) = self.cleanup_baseline(&mut check).await {
                self.store
                    .event(&check.id, "cleanup_error", &format!("{error:#}"))?;
            }
        }
        for kind in ["task", "cycle"] {
            for id in self.store.cleanup_candidates(kind, &cutoff)? {
                if self.shutdown.is_cancelled() {
                    return Ok(());
                }
                let _gate = self.gate.lock().await;
                let result = if kind == "task" {
                    let mut task: Task =
                        self.store.get(kind, &id)?.context("Missing cleanup task")?;
                    if task.status.active() || self.runtime().tasks.contains_key(&id) {
                        continue;
                    }
                    self.discard_task(&mut task).await
                } else {
                    let mut cycle: Cycle = self
                        .store
                        .get(kind, &id)?
                        .context("Missing cleanup cycle")?;
                    if cycle.status == "running" {
                        continue;
                    }
                    self.discard_cycle(&mut cycle).await
                };
                if let Err(error) = result {
                    self.store
                        .event(&id, "cleanup_error", &format!("{error:#}"))?;
                }
            }
        }
        Ok(())
    }
    pub async fn discard_task(&self, task: &mut Task) -> Result<()> {
        ensure!(
            !task.status.active() && task.status != Status::Queued,
            "Active or queued workspaces cannot be discarded"
        );
        if !task.workspace.is_empty() {
            let path = Path::new(&task.workspace)
                .parent()
                .context("Invalid task workspace")?;
            let expected = self.task_workspace(&task.id);
            let expected = expected.parent().context("Invalid task workspace")?;
            let owner = path
                .file_name()
                .and_then(|s| s.to_str())
                .context("Invalid workspace owner")?;
            ensure!(
                path == expected || task.execution_session.as_deref() == Some(owner),
                "Cleanup path does not belong to this task or its legacy execution session"
            );
            remove_owned_dir(&self.data_dir.join("tasks"), path).await?;
        }
        task.lifecycle.discarded_at = Some(now());
        self.save_task(task)
    }
    pub async fn discard_cycle(&self, cycle: &mut Cycle) -> Result<()> {
        ensure!(
            cycle.status != "running",
            "Running planning work cannot be discarded"
        );
        uuid::Uuid::parse_str(&cycle.id).context("Invalid cycle workspace identity")?;
        remove_owned_dir(
            &self.data_dir.join("cycles"),
            &self.data_dir.join("cycles").join(&cycle.id),
        )
        .await?;
        cycle.lifecycle.discarded_at = Some(now());
        self.store.put("cycle", &cycle.id, cycle)
    }
    pub(crate) fn observe_pr(&self, c: &Config, p: PullRequest) -> Result<()> {
        self.persist_observation(c, p, false)
    }
    pub(crate) fn observe_delivery(&self, c: &Config, p: PullRequest) -> Result<()> {
        self.persist_observation(c, p, true)
    }
    fn persist_observation(&self, c: &Config, p: PullRequest, delivered_now: bool) -> Result<()> {
        let previous = self.store.pr_observation(&c.github_repo, p.number)?;
        let record_id = previous
            .as_ref()
            .map(|(id, _)| id.clone())
            .unwrap_or_else(|| format!("{}:{}", c.github_repo.to_ascii_lowercase(), p.number));
        let delivered = if delivered_now {
            Some(p.head.clone())
        } else if let Some(head) = previous.and_then(|(_, p)| p.delivered_head) {
            Some(head)
        } else {
            self.store.latest_pr_output(&c.github_repo, p.number)?
        };
        let observation = PrObservation {
            repository: c.github_repo.clone(),
            observed_at: now(),
            external_head_movement: delivered.as_ref().is_some_and(|head| head != &p.head),
            delivered_head: delivered,
            pr: p,
        };
        self.store.put("pr", &record_id, &observation)
    }
    async fn observe_remote(&self, c: &Config) -> Result<()> {
        let cancel = self.shutdown.child_token();
        git::validate_remote(c, &cancel).await?;
        let inventory = git::open_pr_inventory(c, &cancel).await?;
        for p in inventory.prs.iter().filter(|p| p.owned) {
            self.observe_pr(c, p.clone())?;
        }
        let open: HashSet<_> = inventory.prs.iter().map(|p| p.number).collect();
        // Poll known open PRs even when they disappear from open discovery results.
        let mut before = None;
        loop {
            let known = self.store.history_page(
                "pr",
                &crate::store::HistoryQuery {
                    before,
                    status: Some("open".into()),
                    limit: Some(100),
                    ..Default::default()
                },
            )?;
            for value in known.items {
                if self.shutdown.is_cancelled() {
                    return Ok(());
                }
                let number = value["pr"]["number"]
                    .as_u64()
                    .context("PR observation is missing its number")?;
                if value["repository"]
                    .as_str()
                    .is_some_and(|repository| repository.eq_ignore_ascii_case(&c.github_repo))
                    && !open.contains(&number)
                {
                    self.observe_pr(c, git::pr(c, number, &cancel).await?)?;
                }
            }
            before = known.next_cursor;
            if before.is_none() {
                break;
            }
        }
        self.reconcile_pr_inventory(c, &inventory, &cancel).await?;
        let observed_at = now();
        let revision = git::remote_revision(c, &c.default_branch, &cancel)
            .await?
            .unwrap_or_default();
        let fingerprint = context_fingerprint(&revision, &inventory.prs);
        if !revision.is_empty() {
            self.observe_default_branch(c, &revision, &observed_at)
                .await?;
        }
        let _gate = self.gate.lock().await;
        let live = self.config()?;
        if !live.github_repo.eq_ignore_ascii_case(&c.github_repo) {
            return Ok(());
        }
        let mut control = self.control()?;
        if !control.context_fingerprint.is_empty() && control.context_fingerprint != fingerprint {
            if control.idle_streak > 1 {
                // Reset an extended idle delay without bypassing the operator's
                // ordinary cadence (including changes from our own deliveries).
                control.next_cycle_at = control
                    .next_cycle_at
                    .min(chrono::Utc::now().timestamp() + c.cycle_interval_seconds as i64);
            }
            control.idle_streak = 0;
        }
        control.context_fingerprint = fingerprint;
        self.store.put("settings", "control", &control)
    }
}
pub(super) fn context_fingerprint(revision: &str, prs: &[PullRequest]) -> String {
    use sha2::{Digest, Sha256};
    let mut parts: Vec<_> = prs
        .iter()
        .map(|p| format!("{}:{}:{}:{}", p.number, p.head, p.base, p.state))
        .collect();
    parts.sort();
    format!(
        "{:x}",
        Sha256::digest(format!("{revision}\n{}", parts.join("\n")))
    )
}
pub(super) fn directory_size(path: &Path) -> Result<u64> {
    let entries = match std::fs::read_dir(path) {
        Ok(entries) => entries,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(0),
        Err(e) => return Err(e.into()),
    };
    let mut size = 0u64;
    for e in entries {
        let e = e?;
        let meta = match std::fs::symlink_metadata(e.path()) {
            Ok(m) => m,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
            Err(e) => return Err(e.into()),
        };
        if meta.is_symlink() {
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
pub(super) async fn remove_owned_dir(root: &Path, path: &Path) -> Result<()> {
    ensure!(
        path.parent() == Some(root),
        "Cleanup path must be a direct child of the owned workspace root"
    );
    ensure!(
        path.file_name().is_some_and(|s| s != "." && s != ".."),
        "Invalid cleanup path"
    );
    for ancestor in path.ancestors() {
        match tokio::fs::symlink_metadata(ancestor).await {
            Ok(meta) => ensure!(!meta.is_symlink(), "Cleanup refuses symlink paths"),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
    }
    match tokio::fs::remove_dir_all(path).await {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e.into()),
    }
}
