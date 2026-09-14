//! Indexed operational views. Canonical evidence remains in records.data.
use super::*;
use crate::model::{BatchPhase, Control, Cycle, OperatingMode, RunBatch, Status, Task};
use serde_json::{Value, json};

const TASK_SUMMARY: &str = "json_object('id',NEW.id,'cycle_id',json_extract(NEW.data,'$.cycle_id'),'title',substr(json_extract(NEW.data,'$.proposal.title'),1,200),'category',json_extract(NEW.data,'$.proposal.category'),'tier',json_extract(NEW.data,'$.proposal.tier'),'target',json_extract(NEW.data,'$.proposal.target'),'branch',json_extract(NEW.data,'$.branch'),'status',json_extract(NEW.data,'$.status'),'pr_url',json_extract(NEW.data,'$.pr_url'),'pr_number',json_extract(NEW.data,'$.pr_number'),'error',substr(json_extract(NEW.data,'$.error'),1,512),'blocked_reason',json_extract(NEW.data,'$.blocked_reason'),'created_at',json_extract(NEW.data,'$.created_at'),'updated_at',json_extract(NEW.data,'$.updated_at'),'lifecycle',json(COALESCE(json_extract(NEW.data,'$.lifecycle'),'{}')),'superseded_by',json(COALESCE(json_extract(NEW.data,'$.superseded_by'),'[]')))";
const CYCLE_SUMMARY: &str = "json_object('id',NEW.id,'number',json_extract(NEW.data,'$.number'),'mode',COALESCE(json_extract(NEW.data,'$.mode'),'execution'),'status',json_extract(NEW.data,'$.status'),'started_at',json_extract(NEW.data,'$.started_at'),'completed_at',json_extract(NEW.data,'$.completed_at'),'error',substr(json_extract(NEW.data,'$.error'),1,512),'session_count',json_array_length(NEW.data,'$.sessions'),'decisions',json_object('accepted',(SELECT count(*) FROM json_each(NEW.data,'$.proposals') WHERE json_extract(value,'$.decision')='accepted'),'rejected',(SELECT count(*) FROM json_each(NEW.data,'$.proposals') WHERE json_extract(value,'$.decision')='rejected'),'deferred',(SELECT count(*) FROM json_each(NEW.data,'$.proposals') WHERE json_extract(value,'$.decision')='deferred')),'lifecycle',json(COALESCE(json_extract(NEW.data,'$.lifecycle'),'{}')))";
const PR_SUMMARY: &str = "json_set(json_remove(NEW.data,'$.pr.body'),'$.pr.title',substr(json_extract(NEW.data,'$.pr.title'),1,200))";
// Match str::trim's Unicode whitespace, including tabs and newlines. SQLite's
// default trim only removes ASCII spaces. Keep this identical in the index/query.
const TITLE_WHITESPACE: &str = "char(9,10,11,12,13,32,133,160,5760,8192,8193,8194,8195,8196,8197,8198,8199,8200,8201,8202,8232,8233,8239,8287,12288)";

/// Quotes statuses as a SQL IN-list literal derived from the model vocabulary.
fn status_list(statuses: &[&'static str]) -> String {
    statuses
        .iter()
        .map(|s| format!("'{s}'"))
        .collect::<Vec<_>>()
        .join(",")
}
pub(super) fn migrate(c: &Connection) -> Result<()> {
    let projection = format!(
        "INSERT INTO record_meta(kind,id,seq,status,repository,target,title,cycle_id,run_id,archived,discarded,summary) VALUES (NEW.kind,NEW.id,NEW.rowid,COALESCE(json_extract(NEW.data,'$.status'),json_extract(NEW.data,'$.pr.state'),''),COALESCE(json_extract(NEW.data,'$.config.github_repo'),json_extract(NEW.data,'$.repository'),''),COALESCE(json_extract(NEW.data,'$.proposal.target'),''),COALESCE(json_extract(NEW.data,'$.proposal.title'),json_extract(NEW.data,'$.pr.title'),''),COALESCE(json_extract(NEW.data,'$.cycle_id'),''),json_extract(NEW.data,'$.run_id'),json_extract(NEW.data,'$.lifecycle.archived_at'),json_extract(NEW.data,'$.lifecycle.discarded_at'),CASE NEW.kind WHEN 'task' THEN {TASK_SUMMARY} WHEN 'cycle' THEN {CYCLE_SUMMARY} WHEN 'pr' THEN {PR_SUMMARY} ELSE '{{}}' END) ON CONFLICT(kind,id) DO UPDATE SET status=excluded.status,repository=excluded.repository,target=excluded.target,title=excluded.title,cycle_id=excluded.cycle_id,run_id=excluded.run_id,archived=excluded.archived,discarded=excluded.discarded,summary=excluded.summary;"
    );
    c.execute_batch(&format!("BEGIN IMMEDIATE;
        CREATE TABLE IF NOT EXISTS record_meta(kind TEXT NOT NULL,id TEXT NOT NULL,seq INTEGER NOT NULL,status TEXT NOT NULL,repository TEXT NOT NULL,target TEXT NOT NULL,title TEXT NOT NULL,cycle_id TEXT NOT NULL,run_id TEXT,archived TEXT,discarded TEXT,summary TEXT NOT NULL,PRIMARY KEY(kind,id));
        CREATE INDEX IF NOT EXISTS meta_history ON record_meta(kind,seq DESC);
        CREATE INDEX IF NOT EXISTS meta_status ON record_meta(kind,status,seq);
        CREATE INDEX IF NOT EXISTS meta_counts ON record_meta(kind,archived,status);
        CREATE INDEX IF NOT EXISTS meta_attention ON record_meta(kind,seq DESC) WHERE status IN ('blocked','failed') AND archived IS NULL;
        CREATE INDEX IF NOT EXISTS meta_run ON record_meta(kind,run_id,status,seq);
        CREATE INDEX IF NOT EXISTS meta_cycle ON record_meta(kind,cycle_id,seq);
        CREATE INDEX IF NOT EXISTS meta_pr_identity ON record_meta(repository COLLATE NOCASE,json_extract(summary,'$.pr.number')) WHERE kind='pr';
        CREATE INDEX IF NOT EXISTS meta_duplicate ON record_meta(kind,repository,target,title COLLATE NOCASE,status);
        CREATE TABLE IF NOT EXISTS proposal_records(seq INTEGER PRIMARY KEY AUTOINCREMENT,cycle_id TEXT NOT NULL,proposal_id TEXT NOT NULL,mode TEXT NOT NULL,number INTEGER NOT NULL,decision TEXT NOT NULL,target TEXT NOT NULL,title TEXT NOT NULL,data TEXT NOT NULL,UNIQUE(cycle_id,proposal_id));
        CREATE INDEX IF NOT EXISTS proposal_history ON proposal_records(decision,seq DESC);
        CREATE INDEX IF NOT EXISTS proposal_cycle ON proposal_records(cycle_id,seq DESC);
        CREATE TRIGGER IF NOT EXISTS project_record_insert AFTER INSERT ON records WHEN NEW.kind IN ('task','cycle','pr') BEGIN {projection} END;
        CREATE TRIGGER IF NOT EXISTS project_record_update AFTER UPDATE ON records WHEN NEW.kind IN ('task','cycle','pr') BEGIN {projection} END;
        DROP TRIGGER IF EXISTS project_record_delete;
        COMMIT;"))?;
    let proposal_projection = "INSERT INTO proposal_records(cycle_id,proposal_id,mode,number,decision,target,title,data) SELECT NEW.id,json_extract(value,'$.id'),COALESCE(json_extract(NEW.data,'$.mode'),'execution'),json_extract(NEW.data,'$.number'),json_extract(value,'$.decision'),json_extract(value,'$.target'),json_extract(value,'$.title'),value FROM json_each(NEW.data,'$.proposals') WHERE true ON CONFLICT(cycle_id,proposal_id) DO UPDATE SET decision=excluded.decision,target=excluded.target,title=excluded.title,data=excluded.data;";
    c.execute_batch(&format!("BEGIN IMMEDIATE;
        CREATE TRIGGER IF NOT EXISTS project_proposals_insert AFTER INSERT ON records WHEN NEW.kind='cycle' BEGIN {proposal_projection} END;
        CREATE TRIGGER IF NOT EXISTS project_proposals_update AFTER UPDATE ON records WHEN NEW.kind='cycle' BEGIN {proposal_projection} END;
        COMMIT;"))?;
    c.execute_batch("BEGIN IMMEDIATE;
        CREATE TABLE IF NOT EXISTS batch_members(run_id TEXT NOT NULL,task_id TEXT NOT NULL,status TEXT NOT NULL,PRIMARY KEY(run_id,task_id));
        CREATE INDEX IF NOT EXISTS batch_status ON batch_members(run_id,status);
        CREATE TRIGGER IF NOT EXISTS batch_member_insert AFTER INSERT ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.run_id') IS NOT NULL BEGIN INSERT INTO batch_members VALUES(json_extract(NEW.data,'$.run_id'),NEW.id,json_extract(NEW.data,'$.status')) ON CONFLICT(run_id,task_id) DO UPDATE SET status=excluded.status; END;
        CREATE TRIGGER IF NOT EXISTS batch_member_update AFTER UPDATE ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.run_id') IS NOT NULL BEGIN INSERT INTO batch_members VALUES(json_extract(NEW.data,'$.run_id'),NEW.id,json_extract(NEW.data,'$.status')) ON CONFLICT(run_id,task_id) DO UPDATE SET status=excluded.status; END;
        CREATE INDEX IF NOT EXISTS memory_repository ON records(kind,json_extract(data,'$.repository'));
        CREATE INDEX IF NOT EXISTS task_problem_identity ON records(kind,json_extract(data,'$.config.github_repo'),json_extract(data,'$.proposal.target'),lower(trim(COALESCE(NULLIF(json_extract(data,'$.proposal.problem_key'),''),json_extract(data,'$.proposal.title')))));
        COMMIT;")?;
    // One transactional backfill; reopening never rewrites historical evidence.
    if c.pragma_query_value(None, "user_version", |r| r.get::<_, i64>(0))? < 1 {
        let legacy: Vec<String> = {
            let mut s = c.prepare(
                "SELECT value FROM records,json_each(records.data) WHERE kind='settings' AND records.id='prs' LIMIT 10000",
            )?;
            s.query_map([], |r| r.get(0))?
                .collect::<rusqlite::Result<_>>()?
        };
        for raw in legacy {
            let p: crate::model::PullRequest = serde_json::from_str(&raw)?;
            let repository = reqwest::Url::parse(&p.url)
                .ok()
                .and_then(|u| {
                    let pieces: Vec<_> = u
                        .path()
                        .trim_matches('/')
                        .split('/')
                        .map(str::to_owned)
                        .collect();
                    (pieces.len() >= 4).then(|| format!("{}/{}", pieces[0], pieces[1]))
                })
                .unwrap_or_default();
            let delivered_head: Option<String> = c
                .query_row(
                    "SELECT json_extract(data,'$.output_commit') FROM records WHERE kind='task' AND json_extract(data,'$.status')='published' AND json_extract(data,'$.config.github_repo')=?1 COLLATE NOCASE AND json_extract(data,'$.pr_number')=?2 ORDER BY json_extract(data,'$.updated_at') DESC LIMIT 1",
                    params![repository, p.number as i64],
                    |r| r.get(0),
                )
                .optional()?
                .flatten();
            let observation = crate::model::PrObservation {
                repository: repository.clone(),
                pr: p,
                observed_at: String::new(),
                delivered_head,
                external_head_movement: false,
            };
            c.execute(
                "INSERT OR IGNORE INTO records VALUES ('pr',?1,?2)",
                params![
                    format!("{}:{}", repository, observation.pr.number),
                    serde_json::to_string(&observation)?
                ],
            )?;
        }
        c.execute_batch("BEGIN IMMEDIATE; UPDATE records SET data=data WHERE kind IN ('task','cycle','pr'); PRAGMA user_version=1; COMMIT;")?;
    }
    c.execute_batch("BEGIN IMMEDIATE;
        CREATE TABLE IF NOT EXISTS record_counts(kind TEXT NOT NULL,status TEXT NOT NULL,archived INTEGER NOT NULL,count INTEGER NOT NULL,PRIMARY KEY(kind,status,archived));
        CREATE TRIGGER IF NOT EXISTS count_record_insert AFTER INSERT ON record_meta BEGIN INSERT INTO record_counts VALUES(NEW.kind,NEW.status,NEW.archived IS NOT NULL,1) ON CONFLICT(kind,status,archived) DO UPDATE SET count=count+1; END;
        DROP TRIGGER IF EXISTS count_record_delete;
        CREATE TRIGGER IF NOT EXISTS count_record_update AFTER UPDATE OF status,archived ON record_meta WHEN OLD.status IS NOT NEW.status OR OLD.archived IS NOT NEW.archived BEGIN UPDATE record_counts SET count=count-1 WHERE kind=OLD.kind AND status=OLD.status AND archived=(OLD.archived IS NOT NULL); INSERT INTO record_counts VALUES(NEW.kind,NEW.status,NEW.archived IS NOT NULL,1) ON CONFLICT(kind,status,archived) DO UPDATE SET count=count+1; END;
        COMMIT;")?;
    if c.pragma_query_value(None, "user_version", |r| r.get::<_, i64>(0))? < 2 {
        c.execute_batch("BEGIN IMMEDIATE; INSERT INTO record_counts SELECT kind,status,archived IS NOT NULL,count(*) FROM record_meta GROUP BY kind,status,archived IS NOT NULL ON CONFLICT(kind,status,archived) DO UPDATE SET count=excluded.count; PRAGMA user_version=2; COMMIT;")?;
    }
    if c.pragma_query_value(None, "user_version", |r| r.get::<_, i64>(0))? < 3 {
        // Upgrade projections and indexes without changing saved task/config evidence.
        let proposal_projection = "INSERT INTO proposal_records(cycle_id,proposal_id,mode,number,decision,target,title,data) SELECT NEW.id,json_extract(value,'$.id'),COALESCE(json_extract(NEW.data,'$.mode'),'execution'),json_extract(NEW.data,'$.number'),json_extract(value,'$.decision'),json_extract(value,'$.target'),json_extract(value,'$.title'),value FROM json_each(NEW.data,'$.proposals') WHERE true ON CONFLICT(cycle_id,proposal_id) DO UPDATE SET decision=excluded.decision,target=excluded.target,title=excluded.title,content_revision=proposal_records.content_revision+(proposal_records.data IS NOT excluded.data),data=excluded.data;";
        c.execute_batch(&format!("BEGIN IMMEDIATE;
            ALTER TABLE proposal_records ADD COLUMN content_revision INTEGER NOT NULL DEFAULT 1;
            DROP TRIGGER project_proposals_insert;
            DROP TRIGGER project_proposals_update;
            CREATE TRIGGER project_proposals_insert AFTER INSERT ON records WHEN NEW.kind='cycle' BEGIN {proposal_projection} END;
            CREATE TRIGGER project_proposals_update AFTER UPDATE ON records WHEN NEW.kind='cycle' BEGIN {proposal_projection} END;
            DROP INDEX meta_duplicate;
            CREATE INDEX meta_duplicate ON record_meta(kind,repository COLLATE NOCASE,target,title COLLATE NOCASE,status);
            DROP INDEX memory_repository;
            CREATE INDEX memory_repository ON records(kind,json_extract(data,'$.repository') COLLATE NOCASE);
            DROP INDEX task_problem_identity;
            CREATE INDEX task_problem_identity ON records(kind,json_extract(data,'$.config.github_repo') COLLATE NOCASE,json_extract(data,'$.proposal.target'),lower(trim(COALESCE(NULLIF(json_extract(data,'$.proposal.problem_key'),''),json_extract(data,'$.proposal.title')))));
            PRAGMA user_version=3;
            COMMIT;"))?;
    }
    if c.pragma_query_value(None, "user_version", |r| r.get::<_, i64>(0))? < 4 {
        // Reindex existing titles without rewriting canonical task evidence.
        c.execute_batch(&format!("BEGIN IMMEDIATE;
            DROP INDEX meta_duplicate;
            CREATE INDEX meta_duplicate ON record_meta(kind,repository COLLATE NOCASE,target,trim(title,{TITLE_WHITESPACE}) COLLATE NOCASE,status);
            PRAGMA user_version=4;
            COMMIT;"))?;
    }
    if c.pragma_query_value(None, "user_version", |r| r.get::<_, i64>(0))? < 5 {
        // Cover identity comparisons without reading task evidence. Keep the raw
        // strings so Rust can apply the same Unicode normalization as Proposal.
        c.execute_batch("BEGIN IMMEDIATE;
            DROP INDEX task_problem_identity;
            CREATE INDEX task_problem_identity ON records(kind,json_extract(data,'$.config.github_repo') COLLATE NOCASE,json_extract(data,'$.proposal.target'),json_extract(data,'$.proposal.title'),COALESCE(json_extract(data,'$.proposal.problem_key'),''),json_extract(data,'$.status'),json_extract(data,'$.lifecycle.archived_at'),id);
            PRAGMA user_version=5;
            COMMIT;")?;
    }
    Ok(())
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
        sql.push_str(" AND status IN ('blocked','failed') AND archived IS NULL");
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
                UNION ALL
                SELECT id,seq FROM (
                    SELECT id,seq FROM record_meta WHERE kind='task' AND archived IS NULL
                        AND status='queued' AND (?1 IS NULL OR run_id=?1)
                    ORDER BY seq ASC LIMIT 500
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
        Ok(c.query_row("SELECT EXISTS(SELECT 1 FROM record_counts WHERE kind='task' AND status NOT IN ('published','cancelled') AND archived=0 AND count>0)",[],|r|r.get(0))?)
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
        tx.execute("INSERT INTO records VALUES ('settings','control',?1) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data",[serde_json::to_string(control)?])?;
        tx.commit()?;
        Ok(())
    }
    pub fn begin_cycle(&self, cycle: &Cycle, control: &Control) -> Result<()> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        tx.execute(
            "INSERT INTO records VALUES ('cycle',?1,?2)",
            params![cycle.id, serde_json::to_string(cycle)?],
        )?;
        tx.execute("INSERT INTO records VALUES ('settings','control',?1) ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data",[serde_json::to_string(control)?])?;
        tx.commit()?;
        Ok(())
    }
    pub fn batch_counts(&self, id: &str) -> Result<(u64, u64)> {
        let c = self.conn();
        // Pending work is queued plus every active status; the second list holds the
        // unresolved-terminal statuses.
        let pending = format!("'queued',{}", status_list(&Status::ACTIVE));
        Ok(c.query_row(&format!("SELECT COALESCE(sum(status IN ({pending})),0), COALESCE(sum(status IN ('blocked','failed','cancelled')),0) FROM batch_members WHERE run_id=?1"),[id],|r|Ok((r.get::<_,i64>(0)? as u64,r.get::<_,i64>(1)? as u64)))?)
    }
    pub fn dashboard(&self) -> Result<Value> {
        let mut c = self.conn();
        let tx = c.transaction()?;
        let mut counts = serde_json::Map::new();
        {
            let mut s = tx.prepare(
                "SELECT status,sum(count) FROM record_counts WHERE kind='task' AND (status NOT IN ('blocked','failed') OR archived=0) GROUP BY status HAVING sum(count)>0",
            )?;
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
                [chrono::Utc::now().format("%F").to_string()],
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
        let c = self.conn();
        Ok(c.query_row("SELECT json_extract(r.data,'$.output_commit') FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.status='published' AND json_extract(m.summary,'$.pr_number')=?2 ORDER BY json_extract(m.summary,'$.updated_at') DESC LIMIT 1",params![repository,number as i64],|r|r.get(0)).optional()?.flatten())
    }
    pub fn pr_observation(
        &self,
        repository: &str,
        number: u64,
    ) -> Result<Option<(String, crate::model::PrObservation)>> {
        let c = self.conn();
        let saved: Option<(String, String)> = c.query_row(
            "SELECT m.id,r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE AND json_extract(m.summary,'$.pr.number')=?2 ORDER BY m.seq DESC LIMIT 1",
            params![repository, number as i64],
            |r| Ok((r.get(0)?, r.get(1)?)),
        ).optional()?;
        saved
            .map(|(id, data)| Ok((id, serde_json::from_str(&data)?)))
            .transpose()
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
