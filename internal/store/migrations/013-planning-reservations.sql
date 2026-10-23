CREATE TABLE planning_reservations(cycle_id TEXT PRIMARY KEY,sessions INTEGER NOT NULL);
CREATE TRIGGER release_planning_reservation AFTER UPDATE ON records
WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')!='running' BEGIN
    DELETE FROM planning_reservations WHERE cycle_id=NEW.id;
END;
CREATE TRIGGER delete_planning_reservation AFTER DELETE ON records WHEN OLD.kind='cycle' BEGIN
    DELETE FROM planning_reservations WHERE cycle_id=OLD.id;
END;
