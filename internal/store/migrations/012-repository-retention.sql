-- Attribute before pruning in the same trigger: SQLite does not promise an
-- ordering between separate AFTER INSERT triggers.
DROP TRIGGER cap_activity;
DROP TRIGGER attribute_event_insert;
CREATE INDEX events_repository ON events(repository COLLATE NOCASE,id DESC);
CREATE TRIGGER cap_activity AFTER INSERT ON events BEGIN
    UPDATE events SET repository=COALESCE(
        (SELECT NULLIF(repository,'') FROM record_meta WHERE id=NEW.entity_id LIMIT 1),
        (SELECT COALESCE(json_extract(data,'$.repository'),json_extract(data,'$.config.github_repo')) FROM records WHERE id=NEW.entity_id AND kind IN ('request','baseline') LIMIT 1),
        (SELECT json_extract(data,'$.github_repo') FROM records WHERE kind='settings' AND id='config'),'')
    WHERE id=NEW.id AND repository='';
    DELETE FROM events
    WHERE repository=(SELECT repository FROM events WHERE id=NEW.id) COLLATE NOCASE
        AND id <= (
            SELECT id FROM events
            WHERE repository=(SELECT repository FROM events WHERE id=NEW.id) COLLATE NOCASE
            ORDER BY id DESC LIMIT 1 OFFSET COALESCE((
                SELECT json_extract(data,'$.retain_events') FROM records
                WHERE kind='settings' AND (id='config' OR id LIKE 'config:%')
                    AND json_extract(data,'$.github_repo')=(SELECT repository FROM events WHERE id=NEW.id) COLLATE NOCASE
                LIMIT 1
            ),10000)
        );
END;
