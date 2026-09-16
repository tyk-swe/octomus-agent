use super::{Store, stored_config, tx_get, tx_put};
use crate::model::{OpenPrInventory, Status, Task, now};
use anyhow::Result;
use rusqlite::{Connection, OptionalExtension, TransactionBehavior, params};
use std::collections::HashSet;

#[derive(Debug)]
pub struct PrReservation {
    pub task_id: String,
    pub repository: String,
    pub branch: String,
    pub admitted_at: String,
}

fn reservation_rows(c: &Connection, repository: &str) -> Result<Vec<PrReservation>> {
    let mut s = c.prepare(
        "SELECT task_id,repository,branch,admitted_at FROM pr_reservations WHERE repository=?1",
    )?;
    let rows = s.query_map([repository.to_lowercase()], |r| {
        Ok(PrReservation {
            task_id: r.get(0)?,
            repository: r.get(1)?,
            branch: r.get(2)?,
            admitted_at: r.get(3)?,
        })
    })?;
    rows.collect::<rusqlite::Result<_>>().map_err(Into::into)
}

fn saved_inventory(c: &Connection) -> Result<Option<OpenPrInventory>> {
    let data: Option<String> = c
        .query_row(
            "SELECT data FROM records WHERE kind='settings' AND id='pr_inventory'",
            [],
            |r| r.get(0),
        )
        .optional()?;
    data.map(|s| serde_json::from_str(&s).map_err(Into::into))
        .transpose()
}

pub fn pr_union(
    inventory: &OpenPrInventory,
    reservations: &[PrReservation],
    limit: usize,
) -> (usize, usize, usize) {
    let owned: Vec<_> = inventory.prs.iter().filter(|p| p.owned_open()).collect();
    let observed_count = owned.iter().map(|p| p.number).collect::<HashSet<_>>().len();
    let represented_branches: HashSet<_> = owned.iter().map(|p| p.branch.as_str()).collect();
    let unrepresented = reservations
        .iter()
        .filter(|r| !represented_branches.contains(r.branch.as_str()))
        .count();
    (
        observed_count,
        unrepresented,
        limit.saturating_sub(observed_count + unrepresented),
    )
}

impl Store {
    pub fn has_pr_reservation(&self, task_id: &str) -> Result<bool> {
        Ok(self
            .conn()
            .query_row(
                "SELECT 1 FROM pr_reservations WHERE task_id=?1",
                [task_id],
                |_| Ok(()),
            )
            .optional()?
            .is_some())
    }
    pub fn pr_reservations(&self, repository: &str) -> Result<Vec<PrReservation>> {
        reservation_rows(&self.conn(), repository)
    }
    pub fn pr_reservation_candidates(&self) -> Result<Vec<Task>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT r.data FROM record_meta m JOIN records r ON r.kind='task' AND r.id=m.id
                WHERE m.kind='task' AND (
                    m.status IN ('executing','reviewing','repairing','verifying','publishing')
                    OR (m.status='queued' AND json_extract(r.data,'$.execution_session') IS NOT NULL)
                    OR (m.status!='published' AND json_extract(r.data,'$.output_commit') IS NOT NULL)
                )",
        )?;
        s.query_map([], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
    pub fn seed_pr_reservation(&self, task: &Task) -> Result<()> {
        self.conn().execute(
            "INSERT OR IGNORE INTO pr_reservations(task_id,repository,branch,admitted_at) VALUES (?1,?2,?3,?4)",
            params![
                task.id,
                task.config.github_repo.to_lowercase(),
                task.branch,
                now()
            ],
        )?;
        Ok(())
    }
    pub fn admit_new_pr_task(&self, task: &mut Task, inventory: &OpenPrInventory) -> Result<bool> {
        let mut c = self.conn();
        let tx = c.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let config = stored_config(&tx)?;
        if !inventory
            .repository
            .eq_ignore_ascii_case(&config.github_repo)
        {
            return Ok(false);
        }
        if saved_inventory(&tx)?.is_none_or(|saved| {
            serde_json::to_value(&saved).ok() != serde_json::to_value(&*inventory).ok()
        }) {
            return Ok(false);
        }
        if task.proposal.target != task.config.default_branch
            || !crate::engine::PrIdentity::of(&task.config).matches(&config)
        {
            return Ok(false);
        }
        let Some(canonical): Option<Task> = tx_get(&tx, "task", &task.id)? else {
            return Ok(false);
        };
        if canonical.status != Status::Queued
            || serde_json::to_value(&canonical)? != serde_json::to_value(&*task)?
        {
            return Ok(false);
        }
        let reservations = reservation_rows(&tx, &config.github_repo)?;
        let (_, _, remaining) = pr_union(inventory, &reservations, config.max_open_prs);
        if remaining == 0 {
            return Ok(false);
        }
        task.status = Status::Executing;
        task.updated_at = now();
        tx_put(&tx, "task", &task.id, task)?;
        tx.execute(
            "INSERT INTO pr_reservations(task_id,repository,branch,admitted_at) VALUES (?1,?2,?3,?4)",
            params![
                task.id,
                config.github_repo.to_lowercase(),
                task.branch,
                now()
            ],
        )?;
        tx.execute(
            "INSERT INTO events(at,entity_id,kind,message) VALUES (?1,?2,'status','Executing')",
            params![now(), task.id],
        )?;
        tx.commit()?;
        Ok(true)
    }
    pub fn persist_pr_inventory(
        &self,
        inventory: &OpenPrInventory,
        released: &[String],
    ) -> Result<bool> {
        let mut c = self.conn();
        let tx = c.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let config = stored_config(&tx)?;
        if !inventory
            .repository
            .eq_ignore_ascii_case(&config.github_repo)
        {
            return Ok(false);
        }
        if let Some(existing) = saved_inventory(&tx)?
            .filter(|e| e.repository.eq_ignore_ascii_case(&inventory.repository))
        {
            let previous = chrono::DateTime::parse_from_rfc3339(&existing.observed_at)
                .map_err(|e| anyhow::anyhow!("Saved PR inventory timestamp is invalid: {e}"))?;
            let candidate = chrono::DateTime::parse_from_rfc3339(&inventory.observed_at)
                .map_err(|e| anyhow::anyhow!("PR inventory timestamp is invalid: {e}"))?;
            if candidate < previous {
                return Ok(false);
            }
        }
        tx_put(&tx, "settings", "pr_inventory", inventory)?;
        let represented: HashSet<&str> = inventory
            .prs
            .iter()
            .filter(|p| p.owned_open())
            .map(|p| p.branch.as_str())
            .collect();
        for reservation in reservation_rows(&tx, &config.github_repo)? {
            if released.contains(&reservation.task_id) {
                tx.execute(
                    "DELETE FROM pr_reservations WHERE task_id=?1",
                    [&reservation.task_id],
                )?;
                continue;
            }
            if represented.contains(reservation.branch.as_str()) {
                let published = tx
                    .query_row(
                        "SELECT 1 FROM record_meta WHERE kind='task' AND id=?1 AND status='published'",
                        [&reservation.task_id],
                        |_| Ok(()),
                    )
                    .optional()?
                    .is_some();
                if published {
                    tx.execute(
                        "DELETE FROM pr_reservations WHERE task_id=?1",
                        [&reservation.task_id],
                    )?;
                }
            }
        }
        tx.commit()?;
        Ok(true)
    }
}
