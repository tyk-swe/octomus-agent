package engine

import (
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestBaselineAdmissionRollsBackLatestWriteFailure(t *testing.T) {
	for _, previous := range []bool{false, true} {
		name := "first check"
		if previous {
			name = "replace latest"
		}
		t.Run(name, func(t *testing.T) {
			f := newPlanningFixture(t)
			app := New(f.state, f.dataDir)
			t.Cleanup(app.Shutdown)
			var previousID string
			if previous {
				check := makeCheck(f.cfg, model.BaselineStatusPassed)
				previousID = check.ID
				if err := f.state.Put("baseline", check.ID, check); err != nil {
					t.Fatal(err)
				}
				if err := f.state.Put("settings", "baseline_latest", check.ID); err != nil {
					t.Fatal(err)
				}
			}
			schedulerSQL(t, f.state, `CREATE TEMP TRIGGER refuse_latest BEFORE INSERT ON records
    WHEN NEW.kind='settings' AND NEW.id='baseline_latest'
    BEGIN SELECT RAISE(ABORT, 'synthetic latest refusal'); END`)
			fingerprint, err := f.cfg.Fingerprint()
			if err != nil {
				t.Fatal(err)
			}
			if check, err := app.StartBaseline(fingerprint); check != nil || err == nil || !strings.Contains(err.Error(), "synthetic latest refusal") {
				t.Fatalf("start = %+v, %v", check, err)
			}
			checks, err := store.List[model.BaselineCheck](f.state, "baseline")
			want := 0
			if previous {
				want = 1
			}
			if err != nil || len(checks) != want {
				t.Fatalf("failed admission retained baseline records: %+v, %v", checks, err)
			}
			latest, err := f.state.LatestBaseline()
			if err != nil || previous && (latest == nil || latest.ID != previousID) || !previous && latest != nil {
				t.Fatalf("latest after refusal = %+v, %v", latest, err)
			}
			app.runtimeMu.Lock()
			active := app.runtime.baseline != nil
			app.runtimeMu.Unlock()
			if active {
				t.Fatal("failed admission started a worker")
			}
			schedulerSQL(t, f.state, "DROP TRIGGER refuse_latest")
			admitted, err := app.StartBaseline(fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			latest, err = f.state.LatestBaseline()
			if err != nil || latest == nil || latest.ID != admitted.ID || latest.Status != model.BaselineStatusPassed {
				t.Fatalf("retry = %+v, %v", latest, err)
			}
		})
	}
}
