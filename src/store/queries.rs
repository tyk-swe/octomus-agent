//! Indexed operational views. Canonical evidence remains in records.data.
use super::*;
use crate::model::{
    BaselineCheck, BatchPhase, Control, Cycle, OperatingMode, RunBatch, Status, Task,
};
use serde_json::{Value, json};

/// Quotes statuses as a SQL IN-list literal derived from the model vocabulary.
pub(super) fn status_list(statuses: &[&'static str]) -> String {
    statuses
        .iter()
        .map(|s| format!("'{s}'"))
        .collect::<Vec<_>>()
        .join(",")
}

#[derive(Debug, Default, Clone, Deserialize)]
pub struct HistoryQuery {
    pub before: Option<i64>,
    pub limit: Option<usize>,
    pub status: Option<String>,
    pub q: Option<String>,
    pub cycle: Option<String>,
}
#[derive(Debug, Serialize)]
pub struct Page {
    pub items: Vec<Value>,
    pub counts: Value,
    pub next_cursor: Option<i64>,
}
fn page(c: &Connection, kind: &str, query: &HistoryQuery) -> Result<Page> {
    let limit = query.limit.unwrap_or(50).clamp(1, 100);
    let mut sql = "SELECT summary,seq FROM record_meta WHERE kind=?1 AND seq<?2".to_owned();
    let status = query
        .status
        .as_deref()
        .filter(|s| *s != "all")
        .unwrap_or("");
    if status == "attention" {
        sql = sql.replace(
            "FROM record_meta",
            "FROM record_meta INDEXED BY meta_attention",
        );
    } else if !status.is_empty() {
        sql = sql.replace(
            "FROM record_meta",
            "FROM record_meta INDEXED BY meta_status",
        );
    }
    if status == "active" {
        sql.push_str(&format!(
            " AND status IN ({})",
            status_list(&Status::ACTIVE)
        ));
    } else if status == "attention" {
        sql.push_str(&format!(
            " AND status IN ({}) AND archived IS NULL",
            status_list(&Status::ATTENTION)
        ));
    } else if !status.is_empty() {
        sql.push_str(" AND status=?3");
    } else {
        sql.push_str(" AND ?3=?3");
    }
    // Bind ?3 even for named aggregate filters.
    if status == "active" || status == "attention" {
        sql.push_str(" AND ?3=?3");
    }
    sql.push_str(" AND (?4='' OR instr(lower(title || ' ' || target || ' ' || summary),lower(?4))>0) ORDER BY seq DESC LIMIT ?5");
    let rows = c
        .prepare(&sql)?
        .query_map(
            params![
                kind,
                query.before.unwrap_or(i64::MAX),
                status,
                query.q.as_deref().unwrap_or(""),
                (limit + 1) as i64
            ],
            |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)?)),
        )?
        .collect::<rusqlite::Result<Vec<_>>>()?;
    decode_page(rows, limit)
}
fn decode_page(mut rows: Vec<(String, i64)>, limit: usize) -> Result<Page> {
    let next_cursor = if rows.len() > limit {
        rows.truncate(limit);
        rows.last().map(|r| r.1)
    } else {
        None
    };
    Ok(Page {
        counts: json!({}),
        items: rows
            .into_iter()
            .map(|r| serde_json::from_str(&r.0))
            .collect::<serde_json::Result<_>>()?,
        next_cursor,
    })
}
impl Store {
    pub fn history_page(&self, kind: &str, query: &HistoryQuery) -> Result<Page> {
        page(&self.conn(), kind, query)
    }
    pub fn proposal_page(&self, q: &HistoryQuery) -> Result<Page> {
        let c = self.conn();
        let limit = q.limit.unwrap_or(50).clamp(1, 100);
        let rows = c
            .prepare(
                "SELECT json_set(json_remove(data,'$.prompt','$.evidence'),'$.content_revision',content_revision,'$.cycle',number,'$.cycle_id',cycle_id,'$.mode',mode,'$.prompt','','$.evidence',json('[]'),'$.problem',substr(json_extract(data,'$.problem'),1,2000),'$.reason',substr(json_extract(data,'$.reason'),1,2000)),seq FROM proposal_records WHERE seq<?1 AND (?2='' OR decision=?2) AND (?3='' OR cycle_id=?3) AND (?4='' OR instr(lower(title || ' ' || json_extract(data,'$.problem')),lower(?4))>0) ORDER BY seq DESC LIMIT ?5",
            )?
            .query_map(
                params![
                    q.before.unwrap_or(i64::MAX),
                    q.status.as_deref().filter(|s| *s != "all").unwrap_or(""),
                    q.cycle.as_deref().filter(|s| *s != "all").unwrap_or(""),
                    q.q.as_deref().unwrap_or(""),
                    (limit + 1) as i64
                ],
                |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)?)),
            )?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        let mut page = decode_page(rows, limit)?;
        let mut counts = serde_json::Map::new();
        let mut stmt = c.prepare(
            "SELECT decision,count(*) FROM proposal_records WHERE (?1='' OR cycle_id=?1) GROUP BY decision",
        )?;
        for row in stmt.query_map(
            [q.cycle.as_deref().filter(|v| *v != "all").unwrap_or("")],
            |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)?)),
        )? {
            let (decision, count) = row?;
            counts.insert(decision, json!(count));
        }
        page.counts = Value::Object(counts);
        Ok(page)
    }
    pub fn proposal_detail(&self, cycle: &str, id: &str) -> Result<Option<Value>> {
        let c = self.conn();
        let value: Option<String> = c
            .query_row(
                "SELECT json_set(data,'$.content_revision',content_revision) FROM proposal_records WHERE cycle_id=?1 AND proposal_id=?2",
                params![cycle, id],
                |r| r.get(0),
            )
            .optional()?;
        value.map(|v| Ok(serde_json::from_str(&v)?)).transpose()
    }
    pub fn scheduling_tasks(&self, run_id: Option<&str>) -> Result<Vec<Task>> {
        let c = self.conn();
        // Every active writer must be visible, regardless of the queued history size or batch.
        let mut stmt = c.prepare(&format!(
            "WITH candidates AS (
                SELECT id,seq FROM record_meta WHERE kind='task' AND archived IS NULL
                    AND status IN ({})
                UNION
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM record_meta m JOIN records r ON r.kind='task' AND r.id=m.id
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued'
                            AND (?1 IS NULL OR m.run_id=?1)
                            AND (json_extract(r.data,'$.proposal.target') != json_extract(r.data,'$.config.default_branch')
                                OR EXISTS(SELECT 1 FROM pr_reservations p WHERE p.task_id=m.id))
                        ORDER BY m.seq ASC LIMIT 500
                )
                UNION
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM record_meta m JOIN records r ON r.kind='task' AND r.id=m.id
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued'
                            AND (?1 IS NULL OR m.run_id=?1)
                            AND json_extract(r.data,'$.proposal.target') = json_extract(r.data,'$.config.default_branch')
                            AND NOT EXISTS(SELECT 1 FROM pr_reservations p WHERE p.task_id=m.id)
                        ORDER BY m.seq ASC LIMIT 500
                )
            )
            SELECT r.data FROM candidates m JOIN records r ON r.kind='task' AND r.id=m.id
            ORDER BY m.seq ASC",
            status_list(&Status::ACTIVE)
        ))?;
        stmt.query_map([run_id], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
    pub fn tasks_with_status(&self, statuses: &[&str]) -> Result<Vec<Task>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.status IN (SELECT value FROM json_each(?1)) AND m.archived IS NULL ORDER BY m.seq ASC LIMIT 500",
        )?;
        let rows = s.query_map([serde_json::to_string(statuses)?], |r| {
            r.get::<_, String>(0)
        })?;
        rows.map(|r| Ok(serde_json::from_str(&r?)?)).collect()
    }
    pub fn running_cycles(&self) -> Result<Vec<Cycle>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='cycle' AND m.status='running'",
        )?;
        s.query_map([], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
    pub fn running_baselines(&self) -> Result<Vec<BaselineCheck>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')='running'",
        )?;
        s.query_map([], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
    pub fn baseline_cleanup_candidates(&self) -> Result<Vec<BaselineCheck>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')!='running' AND json_extract(data,'$.workspace_removed')=0 ORDER BY rowid LIMIT 100",
        )?;
        s.query_map([], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
    pub fn latest_baseline(&self) -> Result<Option<BaselineCheck>> {
        let Some(id) = self.get::<String>("settings", "baseline_latest")? else {
            return Ok(None);
        };
        self.get("baseline", &id)
    }
    pub fn tasks_for_cycle(&self, id: &str) -> Result<Vec<Task>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.cycle_id=?1 ORDER BY m.seq",
        )?;
        s.query_map([id], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
    /// Recorded run evidence for one cycle. The cycle and every task naming it are read
    /// inside one transaction, so the result always describes a single consistent
    /// snapshot rather than the dashboard's recent task window.
    pub fn run_evidence(&self, cycle_id: &str) -> Result<Option<Value>> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        let snapshot = crate::evidence::read_snapshot(&tx, cycle_id)?;
        tx.commit()?;
        // Assemble and redact without holding the database lock.
        drop(c);
        snapshot
            .map(|(cycle, tasks)| crate::evidence::evidence_value(&cycle, &tasks))
            .transpose()
    }
    pub fn duplicate_tasks(
        &self,
        repository: &str,
        proposals: &[crate::model::Proposal],
    ) -> Result<Vec<Task>> {
        let mut targets = std::collections::HashMap::<_, Vec<_>>::new();
        for p in proposals.iter().filter(|p| p.decision == "accepted") {
            targets
                .entry(p.target.as_str())
                .or_default()
                .push((p.title.trim(), p.problem_identity()));
        }
        let c = self.conn();
        let mut found = std::collections::HashSet::new();
        {
            // Read the covering index once per target, regardless of proposal count
            // or evidence size. SQLite lower() cannot normalize Unicode identities.
            let mut s = c.prepare(
                "SELECT id,json_extract(data,'$.proposal.title'),COALESCE(json_extract(data,'$.proposal.problem_key'),'') FROM records INDEXED BY task_problem_identity WHERE kind='task' AND json_extract(data,'$.config.github_repo')=?1 COLLATE NOCASE AND json_extract(data,'$.proposal.target')=?2 AND json_extract(data,'$.status')!='cancelled' AND json_extract(data,'$.lifecycle.archived_at') IS NULL",
            )?;
            for (target, identities) in targets {
                let mut rows = s.query(params![repository, target])?;
                while let Some(row) = rows.next()? {
                    let title = row.get_ref(1)?.as_str()?;
                    let key = row.get_ref(2)?.as_str()?;
                    let identity = crate::model::problem_identity(title, key);
                    if identities
                        .iter()
                        .any(|(proposed_title, proposed_identity)| {
                            title.trim().eq_ignore_ascii_case(proposed_title)
                                || identity == *proposed_identity
                        })
                    {
                        found.insert(row.get::<_, String>(0)?);
                    }
                }
            }
        }
        let records = {
            let mut s = c.prepare("SELECT data FROM records WHERE kind='task' AND id=?1")?;
            found
                .into_iter()
                .map(|id| s.query_row([id], |r| r.get::<_, String>(0)))
                .collect::<rusqlite::Result<Vec<_>>>()?
        };
        drop(c);
        records
            .into_iter()
            .map(|row| Ok(serde_json::from_str(&row)?))
            .collect()
    }
    pub fn has_unresolved_tasks(&self) -> Result<bool> {
        let c = self.conn();
        Ok(c.query_row(&format!("SELECT EXISTS(SELECT 1 FROM record_counts WHERE kind='task' AND status NOT IN ({}) AND archived=0 AND count>0)", status_list(&Status::TERMINAL)),[],|r|r.get(0))?)
    }
    pub fn start_batch(&self, control: &mut Control) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        let id = crate::model::id();
        control.set_mode(OperatingMode::RunOnce);
        control.batch = Some(RunBatch {
            id: id.clone(),
            phase: BatchPhase::Draining,
            cycle_id: None,
        });
        control.error = None;
        control.next_cycle_at = 0;
        tx.execute("UPDATE records SET data=json_set(data,'$.run_id',?1) WHERE kind='task' AND id IN (SELECT id FROM record_meta WHERE kind='task' AND status='queued' AND archived IS NULL)",[id])?;
        tx_put(&tx, "settings", "control", control)?;
        tx.commit()?;
        Ok(())
    }
    pub fn begin_cycle(&self, cycle: &Cycle, control: &Control) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        tx_put(&tx, "cycle", &cycle.id, cycle)?;
        tx_put(&tx, "settings", "control", control)?;
        tx.commit()?;
        Ok(())
    }
    pub fn batch_counts(&self, id: &str) -> Result<(u64, u64)> {
        let c = self.conn();
        // Pending work is queued plus every active status; the second list holds the
        // unresolved-terminal statuses.
        let pending = format!("'queued',{}", status_list(&Status::ACTIVE));
        Ok(c.query_row(&format!("SELECT COALESCE(sum(status IN ({pending})),0), COALESCE(sum(status IN ({})),0) FROM batch_members WHERE run_id=?1", status_list(&Status::UNRESOLVED)),[id],|r|Ok((r.get::<_,i64>(0)? as u64,r.get::<_,i64>(1)? as u64)))?)
    }
    pub fn dashboard(&self) -> Result<Value> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        let mut counts = serde_json::Map::new();
        {
            let mut s = tx.prepare(&format!(
                "SELECT status,sum(count) FROM record_counts WHERE kind='task' AND (status NOT IN ({}) OR archived=0) GROUP BY status HAVING sum(count)>0",
                status_list(&Status::ATTENTION)
            ))?;
            for row in s.query_map([], |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)?)))? {
                let (k, v) = row?;
                counts.insert(k, json!(v));
            }
        }
        let query = HistoryQuery {
            limit: Some(100),
            ..Default::default()
        };
        let mut tasks = page(&tx, "task", &query)?;
        for _ in 0..2 {
            if let Some(before) = tasks.next_cursor {
                let p = page(
                    &tx,
                    "task",
                    &HistoryQuery {
                        before: Some(before),
                        ..query.clone()
                    },
                )?;
                tasks.items.extend(p.items);
                tasks.next_cursor = p.next_cursor;
            }
        }
        // Keep old retried work visible even when it predates the recent history window.
        let mut current = page(
            &tx,
            "task",
            &HistoryQuery {
                status: Some("active".into()),
                limit: Some(100),
                ..Default::default()
            },
        )?
        .items;
        current.extend(
            page(
                &tx,
                "task",
                &HistoryQuery {
                    status: Some("queued".into()),
                    limit: Some(100),
                    ..Default::default()
                },
            )?
            .items,
        );
        let ids: std::collections::HashSet<_> = current
            .iter()
            .filter_map(|t| t["id"].as_str().map(str::to_owned))
            .collect();
        current.extend(
            tasks
                .items
                .into_iter()
                .filter(|t| t["id"].as_str().is_none_or(|id| !ids.contains(id))),
        );
        current.truncate(300);
        tasks.items = current;
        let cycles = page(
            &tx,
            "cycle",
            &HistoryQuery {
                limit: Some(20),
                ..Default::default()
            },
        )?;
        let prs = page(&tx, "pr", &query)?;
        let events = {
            let mut s = tx.prepare(
                "SELECT id,at,entity_id,kind,substr(message,1,512) FROM events ORDER BY id DESC LIMIT 200",
            )?;
            s.query_map([],|r|Ok(json!({"id":r.get::<_,i64>(0)?,"at":r.get::<_,String>(1)?,"entity_id":r.get::<_,String>(2)?,"kind":r.get::<_,String>(3)?,"message":r.get::<_,String>(4)?})))?.collect::<rusqlite::Result<Vec<_>>>()?
        };
        let sessions: i64 = tx
            .query_row(
                "SELECT sessions FROM usage WHERE day=?1",
                [crate::model::today()],
                |r| r.get(0),
            )
            .optional()?
            .unwrap_or(0);
        let merged: i64 = tx.query_row(
            "SELECT COALESCE(sum(count),0) FROM record_counts WHERE kind='pr' AND status='merged'",
            [],
            |r| r.get(0),
        )?;
        let attention = page(
            &tx,
            "task",
            &HistoryQuery {
                status: Some("attention".into()),
                limit: Some(5),
                ..Default::default()
            },
        )?;
        let result = json!({"tasks":tasks.items,"cycles":cycles.items,"prs":prs.items,"events":events,"counts":counts,"attention_tasks":attention.items,"merged_prs":merged,"sessions_today":sessions});
        tx.commit()?;
        Ok(result)
    }
    pub fn cleanup_candidates(&self, kind: &str, cutoff: &str) -> Result<Vec<String>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT id FROM record_meta WHERE kind=?1 AND discarded IS NULL AND (archived IS NOT NULL OR (?1='task' AND status='published') OR (?1='cycle' AND status IN ('completed','idle'))) AND julianday(COALESCE(archived,json_extract(summary,'$.updated_at'),json_extract(summary,'$.started_at')))<julianday(?2) ORDER BY seq LIMIT 100",
        )?;
        Ok(s.query_map(params![kind, cutoff], |r| r.get(0))?
            .collect::<rusqlite::Result<_>>()?)
    }
    pub fn latest_pr_output(&self, repository: &str, number: u64) -> Result<Option<String>> {
        latest_pr_output_at(&self.conn(), repository, number)
    }
    pub fn pr_observation(
        &self,
        repository: &str,
        number: u64,
    ) -> Result<Option<(String, crate::model::PrObservation)>> {
        pr_observation_at(&self.conn(), repository, number)
    }
    /// Records a PR observation atomically with the delivery-baseline merge. The
    /// previous observation and latest published output are read under the same
    /// store lock as the write, so a stale poll can never overwrite a newer
    /// `delivered_head` recorded by a concurrent publication.
    pub fn record_pr_observation(
        &self,
        repository: &str,
        p: crate::model::PullRequest,
        delivered_now: bool,
    ) -> Result<()> {
        let c = self.conn();
        let previous = pr_observation_at(&c, repository, p.number)?;
        let record_id = previous
            .as_ref()
            .map(|(id, _)| id.clone())
            .unwrap_or_else(|| format!("{}:{}", repository.to_ascii_lowercase(), p.number));
        let delivered = if delivered_now {
            Some(p.head.clone())
        } else if let Some(head) = previous.and_then(|(_, p)| p.delivered_head) {
            Some(head)
        } else {
            latest_pr_output_at(&c, repository, p.number)?
        };
        let observation = crate::model::PrObservation {
            repository: repository.to_owned(),
            observed_at: crate::model::now(),
            external_head_movement: delivered.as_ref().is_some_and(|head| head != &p.head),
            delivered_head: delivered,
            pr: p,
        };
        tx_put(&c, "pr", &record_id, &observation)
    }
    pub fn decision_memory(&self, repository: &str) -> Result<Vec<Value>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT data FROM records WHERE kind='decision' AND json_extract(data,'$.repository')=?1 COLLATE NOCASE ORDER BY rowid DESC LIMIT 100",
        )?;
        s.query_map([repository], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
    pub fn rediscovery_requests(&self, repository: &str) -> Result<Vec<Value>> {
        let c = self.conn();
        let mut s = c.prepare(
            "SELECT json_object('id',r.id,'title',json_extract(r.data,'$.proposal.title'),'target',json_extract(r.data,'$.proposal.target'),'problem',json_extract(r.data,'$.proposal.problem'),'scope',json_extract(r.data,'$.proposal.scope')) FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.status='cancelled' AND json_extract(r.data,'$.rediscovery_requested')=1 AND json_array_length(r.data,'$.superseded_by')=0 ORDER BY m.seq DESC LIMIT 100",
        )?;
        s.query_map([repository], |r| r.get::<_, String>(0))?
            .map(|r| Ok(serde_json::from_str(&r?)?))
            .collect()
    }
}
fn latest_pr_output_at(c: &Connection, repository: &str, number: u64) -> Result<Option<String>> {
    Ok(c.query_row("SELECT json_extract(r.data,'$.output_commit') FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.status='published' AND json_extract(m.summary,'$.pr_number')=?2 ORDER BY json_extract(m.summary,'$.updated_at') DESC LIMIT 1",params![repository,number as i64],|r|r.get(0)).optional()?.flatten())
}
fn pr_observation_at(
    c: &Connection,
    repository: &str,
    number: u64,
) -> Result<Option<(String, crate::model::PrObservation)>> {
    let saved: Option<(String, String)> = c.query_row(
        "SELECT m.id,r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE AND json_extract(m.summary,'$.pr.number')=?2 ORDER BY m.seq DESC LIMIT 1",
        params![repository, number as i64],
        |r| Ok((r.get(0)?, r.get(1)?)),
    ).optional()?;
    saved
        .map(|(id, data)| Ok((id, serde_json::from_str(&data)?)))
        .transpose()
}
