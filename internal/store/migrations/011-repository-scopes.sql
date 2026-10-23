-- Multi-repository state: attribute the daily session counters to the one
-- configured repository, and backfill projection rows that carry no
-- repository so scoped queries keep finding pre-v11 history. The registry
-- record itself is materialized lazily by RegisterRepository; until then the
-- saved configuration names the single legacy repository.
ALTER TABLE usage RENAME TO usage_v10;
CREATE TABLE usage (
                repository TEXT NOT NULL, day TEXT NOT NULL, sessions INTEGER NOT NULL,
                PRIMARY KEY (repository, day)
            );
INSERT INTO usage(repository,day,sessions)
    SELECT COALESCE((SELECT lower(json_extract(data,'$.github_repo')) FROM records WHERE kind='settings' AND id='config'),''), day, sessions FROM usage_v10;
DROP TABLE usage_v10;

UPDATE record_meta SET repository=COALESCE((SELECT json_extract(data,'$.github_repo') FROM records WHERE kind='settings' AND id='config'),'')
    WHERE repository='';

-- Admissions may outlive their task/cycle; the saved primary is the legacy
-- fallback for those records, just as it is for the usage counters above.
UPDATE admissions SET data=json_set(data,'$.repository',COALESCE(
    (SELECT NULLIF(repository,'') FROM record_meta WHERE kind='task' AND id=json_extract(admissions.data,'$.task_id')),
    (SELECT NULLIF(repository,'') FROM record_meta WHERE kind='cycle' AND id=json_extract(admissions.data,'$.cycle_id')),
    (SELECT json_extract(data,'$.github_repo') FROM records WHERE kind='settings' AND id='config'),''))
    WHERE COALESCE(json_extract(data,'$.repository'),'')='';

ALTER TABLE events ADD COLUMN repository TEXT NOT NULL DEFAULT '';
UPDATE events SET repository=COALESCE(
    (SELECT NULLIF(repository,'') FROM record_meta WHERE id=events.entity_id LIMIT 1),
    (SELECT COALESCE(json_extract(data,'$.repository'),json_extract(data,'$.config.github_repo')) FROM records WHERE id=events.entity_id AND kind IN ('request','baseline') LIMIT 1),
    (SELECT json_extract(data,'$.github_repo') FROM records WHERE kind='settings' AND id='config'),'');

CREATE TRIGGER attribute_event_insert AFTER INSERT ON events WHEN NEW.repository='' BEGIN
    UPDATE events SET repository=COALESCE(
        (SELECT NULLIF(repository,'') FROM record_meta WHERE id=NEW.entity_id LIMIT 1),
        (SELECT COALESCE(json_extract(data,'$.repository'),json_extract(data,'$.config.github_repo')) FROM records WHERE id=NEW.entity_id AND kind IN ('request','baseline') LIMIT 1),
        (SELECT json_extract(data,'$.github_repo') FROM records WHERE kind='settings' AND id='config'),'')
    WHERE id=NEW.id;
END;


DROP TRIGGER notify_control_insert;
CREATE TRIGGER notify_control_insert AFTER INSERT ON records
        WHEN NEW.kind='settings' AND (NEW.id='control' OR NEW.id LIKE 'control:%')
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
            AND COALESCE(json_extract(NEW.data,'$.paused'),0)=1
            AND json_extract(NEW.data,'$.error') IS NOT NULL
        BEGIN
            INSERT INTO notification_outbox
                (event_id,destination_id,created_at,repository,cycle_id,run_id,task_id,category,action,status,attempts,next_attempt_at)
                SELECT lower(hex(randomblob(16))), destination_id, strftime('%Y-%m-%dT%H:%M:%fZ','now'),
                    CASE WHEN NEW.id='control' THEN COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.github_repo'),'') ELSE substr(NEW.id,9) END,
                    json_extract(NEW.data,'$.batch.cycle_id'), json_extract(NEW.data,'$.batch.id'), NULL,
                    'service_error_paused', 'inspect_service', 'pending', 0, unixepoch('now')
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;


DROP TRIGGER notify_control_update;
CREATE TRIGGER notify_control_update AFTER UPDATE ON records
        WHEN NEW.kind='settings' AND (NEW.id='control' OR NEW.id LIKE 'control:%')
            AND (SELECT enabled FROM notification_policy WHERE id=1)=1
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
                FROM notification_policy WHERE id=1;
            UPDATE notification_outbox SET status='failed', last_error='queue_overflow' WHERE seq IN (SELECT seq FROM notification_outbox WHERE status='pending' ORDER BY seq DESC LIMIT -1 OFFSET 1000);
        END;
