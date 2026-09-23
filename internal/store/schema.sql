-- Fresh Go state schema. Projection triggers are assembled from model.Decisions in schema.go.

CREATE TABLE admissions (
                id TEXT PRIMARY KEY, at TEXT NOT NULL, day TEXT NOT NULL, data TEXT NOT NULL
            );

CREATE TABLE batch_members(run_id TEXT NOT NULL,task_id TEXT NOT NULL,status TEXT NOT NULL,PRIMARY KEY(run_id,task_id));

CREATE TABLE events (
                id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL,
                entity_id TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL
            );

CREATE TABLE notification_outbox (
            seq INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE,
            destination_id TEXT NOT NULL, created_at TEXT NOT NULL,
            repository TEXT NOT NULL, cycle_id TEXT, run_id TEXT, task_id TEXT,
            category TEXT NOT NULL, action TEXT NOT NULL,
            status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0,
            next_attempt_at INTEGER NOT NULL, last_attempt_at TEXT,
            delivered_at TEXT, last_error TEXT, http_status INTEGER
        );

CREATE TABLE notification_policy (
            id INTEGER PRIMARY KEY CHECK(id=1), destination_id TEXT, enabled INTEGER NOT NULL DEFAULT 0,
            state TEXT NOT NULL DEFAULT 'disabled', error TEXT
        );

CREATE TABLE pr_reservations(task_id TEXT PRIMARY KEY,repository TEXT NOT NULL,branch TEXT NOT NULL,admitted_at TEXT NOT NULL);

CREATE TABLE proposal_records(seq INTEGER PRIMARY KEY AUTOINCREMENT,cycle_id TEXT NOT NULL,proposal_id TEXT NOT NULL,mode TEXT NOT NULL,number INTEGER NOT NULL,decision TEXT NOT NULL,target TEXT NOT NULL,title TEXT NOT NULL,data TEXT NOT NULL, content_revision INTEGER NOT NULL DEFAULT 1,UNIQUE(cycle_id,proposal_id));

CREATE TABLE record_counts(kind TEXT NOT NULL,status TEXT NOT NULL,archived INTEGER NOT NULL,count INTEGER NOT NULL,PRIMARY KEY(kind,status,archived));

CREATE TABLE record_meta(kind TEXT NOT NULL,id TEXT NOT NULL,seq INTEGER NOT NULL,status TEXT NOT NULL,repository TEXT NOT NULL,target TEXT NOT NULL,title TEXT NOT NULL,cycle_id TEXT NOT NULL,run_id TEXT,archived TEXT,discarded TEXT,summary TEXT NOT NULL,PRIMARY KEY(kind,id));

CREATE TABLE records (
                kind TEXT NOT NULL, id TEXT NOT NULL, data TEXT NOT NULL,
                PRIMARY KEY(kind,id)
            );

CREATE TABLE usage (
                day TEXT PRIMARY KEY, sessions INTEGER NOT NULL
            );

CREATE INDEX admissions_day ON admissions(day);

CREATE INDEX batch_status ON batch_members(run_id,status);

CREATE INDEX memory_repository ON records(kind,json_extract(data,'$.repository') COLLATE NOCASE);

CREATE INDEX meta_attention ON record_meta(kind,seq DESC) WHERE status IN ('blocked','failed') AND archived IS NULL;

CREATE INDEX meta_counts ON record_meta(kind,archived,status);

CREATE INDEX meta_cycle ON record_meta(kind,cycle_id,seq);

CREATE INDEX meta_duplicate ON record_meta(kind,repository COLLATE NOCASE,target,trim(title,char(9,10,11,12,13,32,133,160,5760,8192,8193,8194,8195,8196,8197,8198,8199,8200,8201,8202,8232,8233,8239,8287,12288)) COLLATE NOCASE,status);

CREATE INDEX meta_history ON record_meta(kind,seq DESC);

CREATE INDEX meta_pr_identity ON record_meta(repository COLLATE NOCASE,json_extract(summary,'$.pr.number')) WHERE kind='pr';

CREATE INDEX meta_run ON record_meta(kind,run_id,status,seq);

CREATE INDEX meta_status ON record_meta(kind,status,seq);

CREATE INDEX notification_due ON notification_outbox(destination_id,status,next_attempt_at,seq);

CREATE INDEX pr_reservations_repository ON pr_reservations(repository);

CREATE INDEX proposal_cycle ON proposal_records(cycle_id,seq DESC);

CREATE INDEX proposal_history ON proposal_records(decision,seq DESC);

CREATE INDEX task_problem_identity ON records(kind,json_extract(data,'$.config.github_repo') COLLATE NOCASE,json_extract(data,'$.proposal.target'),json_extract(data,'$.proposal.title'),COALESCE(json_extract(data,'$.proposal.problem_key'),''),json_extract(data,'$.status'),json_extract(data,'$.lifecycle.archived_at'),id);

CREATE TRIGGER batch_member_insert AFTER INSERT ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.run_id') IS NOT NULL BEGIN INSERT INTO batch_members VALUES(json_extract(NEW.data,'$.run_id'),NEW.id,json_extract(NEW.data,'$.status')) ON CONFLICT(run_id,task_id) DO UPDATE SET status=excluded.status; END;

CREATE TRIGGER batch_member_update AFTER UPDATE ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.run_id') IS NOT NULL BEGIN INSERT INTO batch_members VALUES(json_extract(NEW.data,'$.run_id'),NEW.id,json_extract(NEW.data,'$.status')) ON CONFLICT(run_id,task_id) DO UPDATE SET status=excluded.status; END;

CREATE TRIGGER cap_activity AFTER INSERT ON events BEGIN
                DELETE FROM events WHERE id <= NEW.id - COALESCE(json_extract(
                    (SELECT data FROM records WHERE kind='settings' AND id='config'),
                    '$.retain_events'
                ),10000);
            END;

CREATE TRIGGER count_record_insert AFTER INSERT ON record_meta BEGIN INSERT INTO record_counts VALUES(NEW.kind,NEW.status,NEW.archived IS NOT NULL,1) ON CONFLICT(kind,status,archived) DO UPDATE SET count=count+1; END;

CREATE TRIGGER count_record_update AFTER UPDATE OF status,archived ON record_meta WHEN OLD.status IS NOT NEW.status OR OLD.archived IS NOT NEW.archived BEGIN UPDATE record_counts SET count=count-1 WHERE kind=OLD.kind AND status=OLD.status AND archived=(OLD.archived IS NOT NULL); INSERT INTO record_counts VALUES(NEW.kind,NEW.status,NEW.archived IS NOT NULL,1) ON CONFLICT(kind,status,archived) DO UPDATE SET count=count+1; END;

CREATE TRIGGER notify_control_insert AFTER INSERT ON records
        WHEN NEW.kind='settings' AND NEW.id='control'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND COALESCE(json_extract(NEW.data,'$.paused'),0)=1
            AND json_extract(NEW.data,'$.error') IS NOT NULL
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.github_repo'),''),
                    json_extract(NEW.data,'$.batch.cycle_id'), json_extract(NEW.data,'$.batch.id'), NULL,
                    'service_error_paused', 'inspect_service', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

CREATE TRIGGER notify_control_update AFTER UPDATE ON records
        WHEN NEW.kind='settings' AND NEW.id='control'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND COALESCE(json_extract(NEW.data,'$.paused'),0)=1
            AND json_extract(NEW.data,'$.error') IS NOT NULL
            AND NOT (COALESCE(json_extract(OLD.data,'$.paused'),0)=1 AND json_extract(OLD.data,'$.error') IS NOT NULL)
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.github_repo'),''),
                    json_extract(NEW.data,'$.batch.cycle_id'), json_extract(NEW.data,'$.batch.id'), NULL,
                    'service_error_paused', 'inspect_service', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

CREATE TRIGGER notify_task_insert AFTER INSERT ON records
        WHEN NEW.kind='task'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND json_extract(NEW.data,'$.status') IN ('blocked','failed')
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.config.github_repo'),''),
                    json_extract(NEW.data,'$.cycle_id'), json_extract(NEW.data,'$.run_id'), NEW.id,
                    CASE WHEN COALESCE(json_extract(NEW.data,'$.blocked_reason'),'') IN ('budget_exhausted','storage_limit','stale_base','remote_conflict','publication_uncertain','runner_unavailable','invalid_review','verification_failed','dependency_blocked','invalid_plan','workspace_invalid','retry_limit','timeout') THEN json_extract(NEW.data,'$.blocked_reason') ELSE 'unknown' END, 'inspect_task', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

CREATE TRIGGER notify_task_update AFTER UPDATE ON records
        WHEN NEW.kind='task'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND json_extract(NEW.data,'$.status') IN ('blocked','failed')
            AND COALESCE(json_extract(OLD.data,'$.status'),'') NOT IN ('blocked','failed')
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.config.github_repo'),''),
                    json_extract(NEW.data,'$.cycle_id'), json_extract(NEW.data,'$.run_id'), NEW.id,
                    CASE WHEN COALESCE(json_extract(NEW.data,'$.blocked_reason'),'') IN ('budget_exhausted','storage_limit','stale_base','remote_conflict','publication_uncertain','runner_unavailable','invalid_review','verification_failed','dependency_blocked','invalid_plan','workspace_invalid','retry_limit','timeout') THEN json_extract(NEW.data,'$.blocked_reason') ELSE 'unknown' END, 'inspect_task', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

CREATE TRIGGER project_proposals_insert AFTER INSERT ON records WHEN NEW.kind='cycle' BEGIN INSERT INTO proposal_records(cycle_id,proposal_id,mode,number,decision,target,title,data) SELECT NEW.id,json_extract(value,'$.id'),COALESCE(json_extract(NEW.data,'$.mode'),'execution'),json_extract(NEW.data,'$.number'),json_extract(value,'$.decision'),json_extract(value,'$.target'),json_extract(value,'$.title'),value FROM json_each(NEW.data,'$.proposals') WHERE true ON CONFLICT(cycle_id,proposal_id) DO UPDATE SET decision=excluded.decision,target=excluded.target,title=excluded.title,content_revision=proposal_records.content_revision+(proposal_records.data IS NOT excluded.data),data=excluded.data; END;

CREATE TRIGGER project_proposals_update AFTER UPDATE ON records WHEN NEW.kind='cycle' BEGIN INSERT INTO proposal_records(cycle_id,proposal_id,mode,number,decision,target,title,data) SELECT NEW.id,json_extract(value,'$.id'),COALESCE(json_extract(NEW.data,'$.mode'),'execution'),json_extract(NEW.data,'$.number'),json_extract(value,'$.decision'),json_extract(value,'$.target'),json_extract(value,'$.title'),value FROM json_each(NEW.data,'$.proposals') WHERE true ON CONFLICT(cycle_id,proposal_id) DO UPDATE SET decision=excluded.decision,target=excluded.target,title=excluded.title,content_revision=proposal_records.content_revision+(proposal_records.data IS NOT excluded.data),data=excluded.data; END;

CREATE TRIGGER release_pr_reservation_insert AFTER INSERT ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status') IN ('blocked','failed','cancelled') AND json_extract(NEW.data,'$.output_commit') IS NULL BEGIN DELETE FROM pr_reservations WHERE task_id=NEW.id; END;

CREATE TRIGGER release_pr_reservation_update AFTER UPDATE ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status') IN ('blocked','failed','cancelled') AND json_extract(NEW.data,'$.output_commit') IS NULL BEGIN DELETE FROM pr_reservations WHERE task_id=NEW.id; END;
