CREATE TABLE notification_destinations (
            destination_id TEXT PRIMARY KEY, kind TEXT NOT NULL,
            target TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1
        );

CREATE TABLE chat_events (
            provider TEXT NOT NULL, event_id TEXT NOT NULL, received_at TEXT NOT NULL,
            PRIMARY KEY(provider, event_id)
        );

CREATE TABLE chat_context (
            provider TEXT NOT NULL, channel_id TEXT NOT NULL, thread_id TEXT NOT NULL DEFAULT '',
            repository TEXT NOT NULL DEFAULT '', task_id TEXT NOT NULL DEFAULT '',
            updated_at TEXT NOT NULL, PRIMARY KEY(provider, channel_id, thread_id)
        );

INSERT OR IGNORE INTO notification_destinations(destination_id,kind,target,enabled)
SELECT destination_id,'webhook','',enabled FROM notification_policy WHERE id=1 AND destination_id IS NOT NULL;

DROP TRIGGER notify_control_insert;

CREATE TRIGGER notify_control_insert AFTER INSERT ON records
        WHEN NEW.kind='settings' AND (NEW.id='control' OR NEW.id LIKE 'control:%')
            AND EXISTS (SELECT 1 FROM notification_destinations WHERE enabled=1)
            AND COALESCE(json_extract(NEW.data,'$.paused'),0)=1
            AND json_extract(NEW.data,'$.error') IS NOT NULL
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    CASE WHEN NEW.id='control' THEN COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.github_repo'),'') ELSE substr(NEW.id,9) END,
                    json_extract(NEW.data,'$.batch.cycle_id'), json_extract(NEW.data,'$.batch.id'), NULL,
                    'service_error_paused', 'inspect_service', 'pending', 0, unixepoch('now')
                FROM notification_destinations WHERE enabled=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

DROP TRIGGER notify_control_update;

CREATE TRIGGER notify_control_update AFTER UPDATE ON records
        WHEN NEW.kind='settings' AND (NEW.id='control' OR NEW.id LIKE 'control:%')
            AND EXISTS (SELECT 1 FROM notification_destinations WHERE enabled=1)
            AND COALESCE(json_extract(NEW.data,'$.paused'),0)=1
            AND json_extract(NEW.data,'$.error') IS NOT NULL
            AND NOT (COALESCE(json_extract(OLD.data,'$.paused'),0)=1 AND json_extract(OLD.data,'$.error') IS NOT NULL)
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    CASE WHEN NEW.id='control' THEN COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.github_repo'),'') ELSE substr(NEW.id,9) END,
                    json_extract(NEW.data,'$.batch.cycle_id'), json_extract(NEW.data,'$.batch.id'), NULL,
                    'service_error_paused', 'inspect_service', 'pending', 0, unixepoch('now')
                FROM notification_destinations WHERE enabled=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

DROP TRIGGER notify_task_insert;

CREATE TRIGGER notify_task_insert AFTER INSERT ON records
        WHEN NEW.kind='task'
            AND EXISTS (SELECT 1 FROM notification_destinations WHERE enabled=1)
            AND json_extract(NEW.data,'$.status') IN ('blocked','failed','published')
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.config.github_repo'),''),
                    json_extract(NEW.data,'$.cycle_id'), json_extract(NEW.data,'$.run_id'), NEW.id,
                    CASE WHEN json_extract(NEW.data,'$.status')='published' THEN 'task_published' WHEN COALESCE(json_extract(NEW.data,'$.blocked_reason'),'') IN ('budget_exhausted','storage_limit','stale_base','remote_conflict','publication_uncertain','runner_unavailable','invalid_review','verification_failed','dependency_blocked','invalid_plan','workspace_invalid','retry_limit','timeout') THEN json_extract(NEW.data,'$.blocked_reason') ELSE 'unknown' END, 'inspect_task', 'pending', 0, unixepoch('now')
                FROM notification_destinations WHERE enabled=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

DROP TRIGGER notify_task_update;

CREATE TRIGGER notify_task_update AFTER UPDATE ON records
        WHEN NEW.kind='task'
            AND EXISTS (SELECT 1 FROM notification_destinations WHERE enabled=1)
            AND json_extract(NEW.data,'$.status') IN ('blocked','failed','published')
            AND ((json_extract(NEW.data,'$.status')='published' AND COALESCE(json_extract(OLD.data,'$.status'),'')!='published')
                OR (json_extract(NEW.data,'$.status') IN ('blocked','failed') AND COALESCE(json_extract(OLD.data,'$.status'),'') NOT IN ('blocked','failed')))
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.config.github_repo'),''),
                    json_extract(NEW.data,'$.cycle_id'), json_extract(NEW.data,'$.run_id'), NEW.id,
                    CASE WHEN json_extract(NEW.data,'$.status')='published' THEN 'task_published' WHEN COALESCE(json_extract(NEW.data,'$.blocked_reason'),'') IN ('budget_exhausted','storage_limit','stale_base','remote_conflict','publication_uncertain','runner_unavailable','invalid_review','verification_failed','dependency_blocked','invalid_plan','workspace_invalid','retry_limit','timeout') THEN json_extract(NEW.data,'$.blocked_reason') ELSE 'unknown' END, 'inspect_task', 'pending', 0, unixepoch('now')
                FROM notification_destinations WHERE enabled=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

DROP TRIGGER notify_cycle_insert;

CREATE TRIGGER notify_cycle_insert AFTER INSERT ON records
        WHEN NEW.kind='cycle'
            AND EXISTS (SELECT 1 FROM notification_destinations WHERE enabled=1)
            AND (json_extract(NEW.data,'$.status')='failed'
                OR (json_extract(NEW.data,'$.mode')='audit' AND json_extract(NEW.data,'$.status') IN ('completed','idle')))
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.repository'),''), NEW.id,
                    json_extract(NEW.data,'$.run_id'), NULL,
                    CASE WHEN json_extract(NEW.data,'$.status')='failed' THEN 'cycle_failed' ELSE 'audit_completed' END,
                    'inspect_cycle', 'pending', 0, unixepoch('now')
                FROM notification_destinations WHERE enabled=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

DROP TRIGGER notify_cycle_update;

CREATE TRIGGER notify_cycle_update AFTER UPDATE ON records
        WHEN NEW.kind='cycle'
            AND EXISTS (SELECT 1 FROM notification_destinations WHERE enabled=1)
            AND (json_extract(NEW.data,'$.status')='failed'
                OR (json_extract(NEW.data,'$.mode')='audit' AND json_extract(NEW.data,'$.status') IN ('completed','idle')))
            AND CASE WHEN json_extract(NEW.data,'$.status')='failed'
                THEN COALESCE(json_extract(OLD.data,'$.status'),'')!='failed'
                ELSE NOT (COALESCE(json_extract(OLD.data,'$.mode'),'execution')='audit'
                    AND COALESCE(json_extract(OLD.data,'$.status'),'') IN ('completed','idle')) END
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.repository'),''), NEW.id,
                    json_extract(NEW.data,'$.run_id'), NULL,
                    CASE WHEN json_extract(NEW.data,'$.status')='failed' THEN 'cycle_failed' ELSE 'audit_completed' END,
                    'inspect_cycle', 'pending', 0, unixepoch('now')
                FROM notification_destinations WHERE enabled=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;
