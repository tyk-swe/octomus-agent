package engine

import (
	"encoding/json"
	"testing"
)

func TestSaveConfigReturnsCommittedRevisionWhenActivityFails(t *testing.T) {
	t.Parallel()
	app, _ := controlFixture(t, "idle")
	before, err := app.Settings()
	if err != nil {
		t.Fatal(err)
	}
	schedulerSQL(t, app.Store, `CREATE TEMP TRIGGER fail_configuration_activity BEFORE INSERT ON events
 WHEN NEW.kind='configuration'
 BEGIN SELECT RAISE(ABORT, 'synthetic activity failure'); END`)
	saved, err := app.SaveConfig(before.Revision, map[string]json.RawMessage{"max_retries": json.RawMessage(`3`)})
	if err != nil {
		t.Fatalf("committed settings reported as failed: %v", err)
	}
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	revision, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxRetries != 3 || saved.Revision != revision || saved.Revision == before.Revision {
		t.Fatalf("saved response does not describe committed configuration: retries=%d response=%s revision=%s", cfg.MaxRetries, saved.Revision, revision)
	}
	if _, err := app.SaveConfig(saved.Revision, map[string]json.RawMessage{"max_retries": json.RawMessage(`4`)}); err != nil {
		t.Fatalf("next save with returned revision: %v", err)
	}
}

func TestSaveConfigReportsConfigurationWriteFailure(t *testing.T) {
	t.Parallel()
	app, _ := controlFixture(t, "idle")
	before, err := app.Settings()
	if err != nil {
		t.Fatal(err)
	}
	schedulerSQL(t, app.Store, `CREATE TEMP TRIGGER fail_configuration_write BEFORE UPDATE ON records
 WHEN NEW.kind='settings' AND NEW.id='config'
 BEGIN SELECT RAISE(ABORT, 'synthetic configuration failure'); END`)
	saved, err := app.SaveConfig(before.Revision, map[string]json.RawMessage{"max_retries": json.RawMessage(`3`)})
	if err == nil || saved != nil {
		t.Fatalf("configuration write failure = %v, %v; want no saved response", saved, err)
	}
	after, err := app.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatal("failed save changed configuration")
	}
}
