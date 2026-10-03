package engine

import (
	"context"
	"path/filepath"
	"testing"
)

func TestObservationRecordsTheDefaultBranchWithItsContext(t *testing.T) {
	t.Parallel()
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	if err := app.observeRemote(context.Background(), fixture.cfg); err != nil {
		t.Fatal(err)
	}
	head := git(t, fixture.root, "--git-dir", filepath.Join(fixture.root, "remote.git"), "rev-parse", "main")
	app.runtimeMu.Lock()
	observation := app.runtime.defaultObservation
	app.runtimeMu.Unlock()
	if observation == nil || observation.Revision != head || !observation.Describes(fixture.cfg) {
		t.Fatalf("default-branch observation = %+v; want %s for the configured remote", observation, head)
	}
	control, err := app.Control()
	if err != nil || control.ContextFingerprint == "" {
		t.Fatalf("observation did not record the context fingerprint: %+v, %v", control, err)
	}
}
