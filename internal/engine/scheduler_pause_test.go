package engine

import (
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Every scheduler pause follows the Pause contract: the process-local PR
// observation stops authorizing or reporting capacity, and a refresh in flight
// is cancelled, so its result cannot authorize new-PR work after a resume.
func TestSchedulerPausesInvalidatePrObservations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase model.BatchPhase
		event string
	}{
		// An executing batch with no pending members finishes the run.
		{name: "run once completes", phase: model.BatchPhaseExecuting, event: "run_complete"},
		// A draining batch plans next; its preflight fails on the fixture
		// checkout, which has no origin.
		{name: "run once preflight fails", phase: model.BatchPhaseDraining, event: "planning_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeRunOnce)
			control.Batch = &model.RunBatch{ID: model.ID(), Phase: tc.phase}
			saveSettings(t, state, cfg, control)
			inventory := model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: []model.PullRequest{}}
			if persisted, err := state.PersistPrInventory(inventory, nil); err != nil || !persisted {
				t.Fatalf("persist inventory: %t, %v", persisted, err)
			}
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			app.runtime.lastRetention = time.Now()
			app.runtime.lastObserve = time.Now()
			cancelled := false
			app.runtimeMu.Lock()
			app.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now()}
			app.runtime.prRefresh = &prRefreshJob{cancel: func() { cancelled = true }}
			app.runtimeMu.Unlock()
			if capacity, err := app.PrCapacity(); err != nil || capacity.Status != "refreshing" {
				t.Fatalf("capacity before the pause: %+v, %v", capacity, err)
			}

			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait() // The preflight pauses from its own goroutine.

			paused, err := app.Control()
			if err != nil || paused.Mode != model.OperatingModePaused || !paused.Paused || paused.Batch != nil {
				t.Fatalf("run once did not pause: %+v, %v", paused, err)
			}
			app.runtimeMu.Lock()
			observation, refresh := app.runtime.prObservation, app.runtime.prRefresh
			app.runtimeMu.Unlock()
			if observation != nil || refresh != nil || !cancelled {
				t.Fatalf("pause kept PR authority: observation=%v refresh=%v cancelled=%t", observation != nil, refresh != nil, cancelled)
			}
			capacity, err := app.PrCapacity()
			if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil {
				t.Fatalf("paused capacity: %+v, %v", capacity, err)
			}
			system := "system"
			events, err := state.Events(&system)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, event := range events {
				if event.Kind == tc.event {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("recorded %d %s events; want one: %+v", count, tc.event, events)
			}
		})
	}
}
