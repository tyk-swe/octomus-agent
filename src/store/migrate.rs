//! Schema creation and one-time upgrades. Every statement here is idempotent:
//! opening an existing database re-runs this and changes nothing. The projection
//! triggers below keep record_meta in step with records, so queries never scan
//! JSON documents.
use super::*;

const TASK_SUMMARY: &str = "json_object('id',NEW.id,'cycle_id',json_extract(NEW.data,'$.cycle_id'),'title',substr(json_extract(NEW.data,'$.proposal.title'),1,200),'category',json_extract(NEW.data,'$.proposal.category'),'tier',json_extract(NEW.data,'$.proposal.tier'),'target',json_extract(NEW.data,'$.proposal.target'),'branch',json_extract(NEW.data,'$.branch'),'status',json_extract(NEW.data,'$.status'),'pr_url',json_extract(NEW.data,'$.pr_url'),'pr_number',json_extract(NEW.data,'$.pr_number'),'error',substr(json_extract(NEW.data,'$.error'),1,512),'blocked_reason',json_extract(NEW.data,'$.blocked_reason'),'created_at',json_extract(NEW.data,'$.created_at'),'updated_at',json_extract(NEW.data,'$.updated_at'),'lifecycle',json(COALESCE(json_extract(NEW.data,'$.lifecycle'),'{}')),'superseded_by',json(COALESCE(json_extract(NEW.data,'$.superseded_by'),'[]')))";
const CYCLE_SUMMARY: &str = "json_object('id',NEW.id,'number',json_extract(NEW.data,'$.number'),'mode',COALESCE(json_extract(NEW.data,'$.mode'),'execution'),'status',json_extract(NEW.data,'$.status'),'started_at',json_extract(NEW.data,'$.started_at'),'completed_at',json_extract(NEW.data,'$.completed_at'),'error',substr(json_extract(NEW.data,'$.error'),1,512),'session_count',json_array_length(NEW.data,'$.sessions'),'decisions',json_object('accepted',(SELECT count(*) FROM json_each(NEW.data,'$.proposals') WHERE json_extract(value,'$.decision')='accepted'),'rejected',(SELECT count(*) FROM json_each(NEW.data,'$.proposals') WHERE json_extract(value,'$.decision')='rejected'),'deferred',(SELECT count(*) FROM json_each(NEW.data,'$.proposals') WHERE json_extract(value,'$.decision')='deferred')),'lifecycle',json(COALESCE(json_extract(NEW.data,'$.lifecycle'),'{}')))";
const PR_SUMMARY: &str = "json_set(json_remove(NEW.data,'$.pr.body'),'$.pr.title',substr(json_extract(NEW.data,'$.pr.title'),1,200))";
// Match str::trim's Unicode whitespace, including tabs and newlines. SQLite's
// default trim only removes ASCII spaces. Keep this identical in the index/query.
const TITLE_WHITESPACE: &str = "char(9,10,11,12,13,32,133,160,5760,8192,8193,8194,8195,8196,8197,8198,8199,8200,8201,8202,8232,8233,8239,8287,12288)";

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
    c.execute_batch("BEGIN IMMEDIATE;
        CREATE TABLE IF NOT EXISTS pr_reservations(task_id TEXT PRIMARY KEY,repository TEXT NOT NULL,branch TEXT NOT NULL,admitted_at TEXT NOT NULL);
        CREATE INDEX IF NOT EXISTS pr_reservations_repository ON pr_reservations(repository);
        CREATE TRIGGER IF NOT EXISTS release_pr_reservation_insert AFTER INSERT ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status') IN ('blocked','failed','cancelled') AND json_extract(NEW.data,'$.output_commit') IS NULL BEGIN DELETE FROM pr_reservations WHERE task_id=NEW.id; END;
        CREATE TRIGGER IF NOT EXISTS release_pr_reservation_update AFTER UPDATE ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status') IN ('blocked','failed','cancelled') AND json_extract(NEW.data,'$.output_commit') IS NULL BEGIN DELETE FROM pr_reservations WHERE task_id=NEW.id; END;
        COMMIT;")?;
    Ok(())
}
