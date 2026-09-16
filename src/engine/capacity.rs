use super::App;
use crate::config::Config;
use crate::model::{OpenPrInventory, PrCapacity, Status, Task};
use crate::store::error_message;
use anyhow::{Result, ensure};
use std::collections::HashSet;
use std::path::{Path, PathBuf};
use tokio::task::JoinHandle;
use tokio_util::sync::CancellationToken;

#[derive(Debug, Clone)]
pub struct PrIdentity {
    pub repository: PathBuf,
    pub github_repo: String,
    pub default_branch: String,
    pub branch_prefix: String,
}
impl PrIdentity {
    pub fn of(c: &Config) -> Self {
        Self {
            repository: c.repository.clone(),
            github_repo: c.github_repo.to_lowercase(),
            default_branch: c.default_branch.clone(),
            branch_prefix: c.branch_prefix.clone(),
        }
    }
    pub fn matches(&self, c: &Config) -> bool {
        self.repository == c.repository
            && c.github_repo.eq_ignore_ascii_case(&self.github_repo)
            && self.default_branch == c.default_branch
            && self.branch_prefix == c.branch_prefix
    }
}

pub struct PrRefresh {
    pub id: String,
    pub identity: PrIdentity,
    pub handle: JoinHandle<()>,
    pub cancel: CancellationToken,
    pub result: Option<OpenPrInventory>,
    pub error: Option<String>,
    pub last_attempt: i64,
}

impl App {
    pub fn schedule_pr_refresh(&self, c: &Config) -> Result<()> {
        let available = self.pr_capacity()?.remaining.is_some_and(|n| n > 0);
        let mut rt = self.runtime();
        if rt.pr_refresh.is_some() {
            return Ok(());
        }
        let now = chrono::Utc::now().timestamp();
        if !available && now - rt.pr_refresh_last_attempt < 60 {
            return Ok(());
        }
        let app = self.clone();
        let config = c.clone();
        let cancel = self.shutdown.child_token();
        let worker_cancel = cancel.clone();
        let job_id = crate::model::id();
        let record_id = job_id.clone();
        let handle = tokio::spawn(async move {
            let fetched = crate::git::open_pr_inventory(&config, &worker_cancel).await;
            let releases = match &fetched {
                Ok(inventory) => {
                    app.uncertain_reservation_releases(&config, inventory, &worker_cancel)
                        .await
                }
                Err(_) => vec![],
            };
            let _gate = app.gate.lock().await;
            app.record_pr_refresh(&record_id, &worker_cancel, fetched, releases);
        });
        rt.pr_refresh = Some(PrRefresh {
            id: job_id,
            identity: PrIdentity::of(c),
            handle,
            cancel,
            result: None,
            error: None,
            last_attempt: now,
        });
        rt.pr_refresh_last_attempt = now;
        Ok(())
    }
    pub fn collect_pr_refresh(&self) -> Option<OpenPrInventory> {
        let mut rt = self.runtime();
        let job = rt.pr_refresh.as_ref()?;
        if !job.handle.is_finished() {
            return None;
        }
        let job = rt.pr_refresh.take()?;
        if job.cancel.is_cancelled() {
            return None;
        }
        rt.pr_refresh_last_attempt = job.last_attempt;
        rt.pr_refresh_error = job.error.clone();
        let inventory = job.result?;
        let live = self.config().ok()?;
        if !job.identity.matches(&live) {
            return None;
        }
        let observed = chrono::DateTime::parse_from_rfc3339(&inventory.observed_at).ok()?;
        if chrono::Utc::now()
            .signed_duration_since(observed.with_timezone(&chrono::Utc))
            .num_seconds()
            > 60
        {
            return None;
        }
        Some(inventory)
    }
    pub fn record_pr_refresh(
        &self,
        job_id: &str,
        worker_cancel: &CancellationToken,
        fetched: Result<OpenPrInventory>,
        releases: Vec<String>,
    ) {
        let identity = {
            let rt = self.runtime();
            let Some(job) = &rt.pr_refresh else { return };
            if job.id != job_id || worker_cancel.is_cancelled() {
                return;
            }
            job.identity.clone()
        };
        let live = match self.config() {
            Ok(c) => c,
            Err(e) => {
                self.update_pr_refresh(job_id, |j| {
                    j.error = Some(error_message(&e));
                });
                return;
            }
        };
        if !identity.matches(&live) {
            self.update_pr_refresh(job_id, |j| {
                j.error = Some("Configuration changed during the open-PR refresh".into());
            });
            return;
        }
        match fetched {
            Ok(inventory) => match self.persist_pr_observation(&inventory, &releases) {
                Ok(true) => self.update_pr_refresh(job_id, |j| {
                    j.result = Some(inventory);
                    j.error = None;
                }),
                Ok(false) => self.update_pr_refresh(job_id, |j| {
                    j.error = Some("Configuration changed during the open-PR refresh".into());
                }),
                Err(e) => self.update_pr_refresh(job_id, |j| {
                    j.error = Some(error_message(&e));
                }),
            },
            Err(e) => self.update_pr_refresh(job_id, |j| {
                j.error = Some(error_message(&e));
            }),
        }
    }
    fn update_pr_refresh(&self, job_id: &str, f: impl FnOnce(&mut PrRefresh)) {
        if let Some(job) = self.runtime().pr_refresh.as_mut()
            && job.id == job_id
        {
            f(job);
        }
    }
    pub fn invalidate_pr_refresh(&self) {
        let mut rt = self.runtime();
        if let Some(job) = rt.pr_refresh.as_mut() {
            job.cancel.cancel();
            job.result = None;
            job.error = None;
        }
        rt.pr_refresh_last_attempt = 0;
        rt.pr_refresh_error = None;
    }
    pub fn persist_pr_observation(
        &self,
        inventory: &OpenPrInventory,
        released: &[String],
    ) -> Result<bool> {
        if self.store.persist_pr_inventory(inventory, released)? {
            let c = self.config()?;
            let mut rt = self.runtime();
            rt.pr_observation = Some((PrIdentity::of(&c), chrono::Utc::now().timestamp()));
            rt.pr_refresh_error = None;
            if let Some(job) = rt.pr_refresh.as_mut()
                && job.handle.is_finished()
            {
                job.error = None;
            }
            return Ok(true);
        }
        Ok(false)
    }
    pub fn pr_capacity(&self) -> Result<PrCapacity> {
        let c = self.config()?;
        let stored: Option<OpenPrInventory> = self.store.get("settings", "pr_inventory")?;
        let usable = stored.filter(|i| i.repository.eq_ignore_ascii_case(&c.github_repo));
        let reservations = self.store.pr_reservations(&c.github_repo)?;
        let (refreshing, last_error, observation) = {
            let rt = self.runtime();
            let job = rt.pr_refresh.as_ref();
            let running = job.is_some_and(|j| !j.handle.is_finished() && !j.cancel.is_cancelled());
            let finished_error = job
                .filter(|j| j.handle.is_finished() && !j.cancel.is_cancelled())
                .and_then(|j| j.error.clone());
            (
                running,
                finished_error.or(rt.pr_refresh_error.clone()),
                rt.pr_observation.clone(),
            )
        };
        let fresh_observation = observation.as_ref().is_some_and(|(identity, at)| {
            identity.matches(&c) && chrono::Utc::now().timestamp() - *at <= 300
        });
        let (owned_open, reserved, remaining) = match &usable {
            Some(inventory) => {
                let (observed, unrepresented, remaining) =
                    crate::store::pr_union(inventory, &reservations, c.max_open_prs);
                (Some(observed), unrepresented, remaining)
            }
            None => (None, reservations.len(), 0),
        };
        let fresh = last_error.is_none() && fresh_observation && usable.is_some();
        let status = if refreshing {
            "refreshing"
        } else if !fresh {
            "unavailable"
        } else if remaining == 0 {
            "full"
        } else {
            "ready"
        };
        let reason = match status {
            "refreshing" => Some(match &last_error {
                Some(e) => format!("Refreshing the open-PR inventory after a failure: {e}"),
                None => "Refreshing the open-PR inventory".into(),
            }),
            "unavailable" => Some(last_error.clone().unwrap_or_else(|| match &observation {
                None => "No complete open-PR inventory has been observed".into(),
                Some((identity, _)) if !identity.matches(&c) => {
                    "Configuration changed since the last complete open-PR inventory".into()
                }
                _ => "The last complete open-PR inventory is stale".into(),
            })),
            "full" => Some("The configured owned open-PR limit is reached; new-PR work waits for an observed closure or merge".into()),
            _ => last_error,
        };
        Ok(PrCapacity {
            limit: c.max_open_prs,
            owned_open,
            reserved,
            remaining: if fresh { Some(remaining) } else { None },
            observed_at: usable.map(|i| i.observed_at.clone()),
            status: status.into(),
            reason,
        })
    }
    pub(super) fn seed_pr_reservations(&self) -> Result<()> {
        for task in self.store.pr_reservation_candidates()? {
            if task.proposal.target != task.config.default_branch {
                continue;
            }
            let initialized_queued = task.status == Status::Queued
                && task.execution_session.is_some()
                && Path::new(&task.workspace).join(".git").exists()
                && !task.comparison_base.is_empty();
            if task.status.active()
                || initialized_queued
                || (task.status != Status::Published && task.output_commit.is_some())
            {
                self.store.seed_pr_reservation(&task)?;
            }
        }
        Ok(())
    }
    pub(super) async fn reconcile_pr_inventory(
        &self,
        c: &Config,
        inventory: &OpenPrInventory,
        cancel: &CancellationToken,
    ) -> Result<()> {
        let releases = self
            .uncertain_reservation_releases(c, inventory, cancel)
            .await;
        let _gate = self.gate.lock().await;
        let live = self.config()?;
        ensure!(
            PrIdentity::of(c).matches(&live),
            "Configuration changed while the open-PR inventory was being read"
        );
        self.persist_pr_observation(inventory, &releases)?;
        Ok(())
    }
    pub(super) async fn uncertain_reservation_releases(
        &self,
        c: &Config,
        inventory: &OpenPrInventory,
        cancel: &CancellationToken,
    ) -> Vec<String> {
        let represented: HashSet<&str> = inventory
            .prs
            .iter()
            .filter(|p| p.owned_open())
            .map(|p| p.branch.as_str())
            .collect();
        let mut released = Vec::new();
        for reservation in match self.store.pr_reservations(&c.github_repo) {
            Ok(reservations) => reservations,
            Err(e) => {
                tracing::warn!("PR reservation reconciliation failed: {e:#}");
                return released;
            }
        } {
            if represented.contains(reservation.branch.as_str()) {
                continue;
            }
            let task: Option<Task> = match self.store.get("task", &reservation.task_id) {
                Ok(task) => task,
                Err(e) => {
                    tracing::warn!("PR reservation reconciliation failed: {e:#}");
                    return released;
                }
            };
            let Some(task) = task else {
                continue;
            };
            if !(task.status == Status::Published && task.pr_number.is_some())
                && task.output_commit.is_none()
            {
                continue;
            }
            let Some(number) = task.pr_number else {
                continue;
            };
            match crate::git::pr(c, number, cancel).await {
                Ok(detail)
                    if ["closed", "merged"].contains(&detail.state.as_str())
                        && detail.head_repository.eq_ignore_ascii_case(&c.github_repo)
                        && detail.base_repository.eq_ignore_ascii_case(&c.github_repo)
                        && detail.branch == reservation.branch
                        && detail
                            .body
                            .contains(&format!("<!-- octomus:task:{} -->", task.id)) =>
                {
                    released.push(reservation.task_id)
                }
                Ok(_) => {}
                Err(e) => {
                    tracing::warn!("PR {} could not be reobserved: {e:#}", reservation.branch)
                }
            }
        }
        released
    }
}
