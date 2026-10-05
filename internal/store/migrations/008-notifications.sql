DROP TRIGGER notify_task_insert;
DROP TRIGGER notify_task_update;

CREATE TRIGGER notify_task_insert AFTER INSERT ON records
        WHEN NEW.kind='task'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND json_extract(NEW.data,'$.status') IN ('blocked','failed','published')
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    COALESCE(json_extract(NEW.data,'$.config.github_repo'),''),
                    json_extract(NEW.data,'$.cycle_id'), json_extract(NEW.data,'$.run_id'), NEW.id,
                    CASE WHEN json_extract(NEW.data,'$.status')='published' THEN 'task_published' WHEN COALESCE(json_extract(NEW.data,'$.blocked_reason'),'') IN ('budget_exhausted','storage_limit','stale_base','remote_conflict','publication_uncertain','runner_unavailable','invalid_review','verification_failed','dependency_blocked','invalid_plan','workspace_invalid','retry_limit','timeout') THEN json_extract(NEW.data,'$.blocked_reason') ELSE 'unknown' END, 'inspect_task', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

CREATE TRIGGER notify_task_update AFTER UPDATE ON records
        WHEN NEW.kind='task'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
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
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

CREATE TRIGGER notify_cycle_insert AFTER INSERT ON records
        WHEN NEW.kind='cycle'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
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
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

CREATE TRIGGER notify_cycle_update AFTER UPDATE ON records
        WHEN NEW.kind='cycle'
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
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
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;

