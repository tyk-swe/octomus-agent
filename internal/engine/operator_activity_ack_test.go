package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func refuseOperatorActivity(t *testing.T, state *store.Store) {
	t.Helper()
	schedulerSQL(t, state, `CREATE TEMP TRIGGER refuse_operator_activity BEFORE INSERT ON events
		WHEN NEW.kind='operator'
		BEGIN SELECT RAISE(ABORT, 'synthetic operator activity refusal'); END`)
}

func requireNoOperatorActivity(t *testing.T, state *store.Store) {
	t.Helper()
	events, err := state.Events(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "operator" {
			t.Fatalf("refused activity was stored: %+v", event)
		}
	}
}

func TestCommittedTaskActionSurvivesActivityFailure(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel", "archive", "supersede", "retry"} {
		t.Run(action, func(t *testing.T) {
			f := newExecutionFixture(t)
			task := executionTask(t, f, f.cfg.DefaultBranch)
			if action == "supersede" || action == "retry" || action == "archive" {
				task.Status = model.StatusBlocked
				task.BlockedReason = blockedReasonPtr(model.BlockedReasonStaleBase)
			}
			if action == "retry" {
				task.BlockedReason = blockedReasonPtr(model.BlockedReasonUnknown)
			}
			saveExecutionTask(t, f, task)
			app := New(f.state, f.dataDir)
			t.Cleanup(app.Shutdown)
			refuseOperatorActivity(t, f.state)
			err := app.TaskAction(context.Background(), task.ID, action)
			saved := loadTask(t, f.state, task.ID)
			switch action {
			case "cancel":
				if saved.Status != model.StatusCancelled {
					t.Fatalf("cancel did not persist: %s", saved.Status)
				}
			case "archive":
				if saved.Lifecycle.ArchivedAt == nil {
					t.Fatal("archive did not persist")
				}
			case "supersede":
				if saved.Status != model.StatusCancelled || !saved.RediscoveryRequested {
					t.Fatalf("rediscovery did not persist: %+v", saved)
				}
			case "retry":
				if saved.Status != model.StatusQueued || saved.Attempts != 1 {
					t.Fatalf("retry did not persist: %+v", saved)
				}
			}
			if err != nil {
				t.Fatalf("committed %s reported failure: %v", action, err)
			}
			requireNoOperatorActivity(t, f.state)
		})
	}
}

func TestCommittedCycleArchiveSurvivesActivityFailure(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	cycle := discardableCycle(t, dataDir)
	cycle.Lifecycle.ArchivedAt = nil
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	app := New(state, dataDir)
	t.Cleanup(app.Shutdown)
	refuseOperatorActivity(t, state)
	err := app.CycleAction(cycle.ID, "archive")
	saved, readErr := store.Get[model.Cycle](state, "cycle", cycle.ID)
	if readErr != nil || saved == nil || saved.Lifecycle.ArchivedAt == nil {
		t.Fatalf("archive did not persist: %+v, %v", saved, readErr)
	}
	if err != nil {
		t.Fatalf("committed cycle archive reported failure: %v", err)
	}
	requireNoOperatorActivity(t, state)
	if err := app.CycleAction(cycle.ID, "archive"); !IsActionConflict(err) {
		t.Fatalf("archive replay = %v; want conflict", err)
	}
}

func TestCommittedControlSurvivesActivityFailure(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"pause", "resume", "cycle"} {
		t.Run(action, func(t *testing.T) {
			f := newPlanningFixture(t)
			control := model.DefaultControl()
			if action == "pause" {
				control.SetMode(model.OperatingModeContinuous)
			}
			saveSettings(t, f.state, f.cfg, control)
			app := New(f.state, f.dataDir)
			t.Cleanup(app.Shutdown)
			refuseOperatorActivity(t, f.state)
			body, err := app.ControlAction(action)
			saved, readErr := app.Control()
			if readErr != nil || saved.Mode == control.Mode {
				t.Fatalf("control did not persist: %+v, %v", saved, readErr)
			}
			if err != nil {
				t.Fatalf("committed %s reported failure: %v", action, err)
			}
			if body["mode"] != saved.Mode.String() {
				t.Fatalf("response disagrees with durable control: %+v, %+v", body, saved)
			}
			requireNoOperatorActivity(t, f.state)
		})
	}
}

func TestCommittedDiscardSurvivesActivityFailure(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"task", "cycle"} {
		t.Run(kind, func(t *testing.T) {
			state := testStore(t)
			dataDir := t.TempDir()
			cfg := testConfig(t.TempDir())
			saveSettings(t, state, cfg, model.DefaultControl())
			app := New(state, dataDir)
			t.Cleanup(app.Shutdown)
			var id, path string
			var action func() error
			if kind == "task" {
				task := discardableTask(t, cfg, dataDir, "discard")
				id, path = task.ID, task.Workspace
				if err := state.Put(kind, id, task); err != nil {
					t.Fatal(err)
				}
				action = func() error { return app.TaskAction(context.Background(), id, "discard") }
			} else {
				cycle := discardableCycle(t, dataDir)
				id, path = cycle.ID, filepath.Join(dataDir, "cycles", cycle.ID)
				if err := state.Put(kind, id, cycle); err != nil {
					t.Fatal(err)
				}
				action = func() error { return app.CycleAction(id, "discard") }
			}
			refuseOperatorActivity(t, state)
			if err := action(); err != nil {
				t.Fatalf("committed %s discard reported failure: %v", kind, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("workspace was not removed: %v", err)
			}
			if err := action(); !IsActionConflict(err) {
				t.Fatalf("discard replay = %v; want conflict", err)
			}
			requireNoOperatorActivity(t, state)
		})
	}
}

func TestCommittedReconciliationSurvivesActivityFailure(t *testing.T) {
	t.Parallel()
	for _, checkpoint := range []bool{false, true} {
		name := "preflight"
		if checkpoint {
			name = "publication"
		}
		t.Run(name, func(t *testing.T) {
			f := newExecutionFixture(t)
			task := executionTask(t, f, f.cfg.DefaultBranch)
			if checkpoint {
				task = checkpointedTask(t, f, f.cfg.DefaultBranch)
			}
			task.Status = model.StatusBlocked
			task.BlockedReason = blockedReasonPtr(model.BlockedReasonRemoteConflict)
			saveExecutionTask(t, f, task)
			app := New(f.state, f.dataDir)
			t.Cleanup(app.Shutdown)
			refuseOperatorActivity(t, f.state)
			if err := app.TaskAction(context.Background(), task.ID, "reconcile"); err != nil {
				t.Fatalf("committed %s reconciliation reported failure: %v", name, err)
			}
			saved := loadTask(t, f.state, task.ID)
			if checkpoint {
				if saved.Status != model.StatusPublished || saved.PRNumber == nil {
					t.Fatalf("publication outcome was not saved: %+v", saved)
				}
			} else if !blockedAs(saved, model.BlockedReasonUnknown) {
				t.Fatalf("preflight outcome was not saved: %+v", saved)
			}
			requireNoOperatorActivity(t, f.state)
		})
	}
}

func TestOperatorMutationFailureIsStillReturned(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"task", "cycle", "settings"} {
		t.Run(kind, func(t *testing.T) {
			state := testStore(t)
			dataDir := t.TempDir()
			cfg := testConfig(t.TempDir())
			saveSettings(t, state, cfg, model.DefaultControl())
			app := New(state, dataDir)
			t.Cleanup(app.Shutdown)
			var id string
			var action func() error
			switch kind {
			case "task":
				task := queuedTask(cfg, "refused", cfg.DefaultBranch, "tyk/refused")
				id = task.ID
				if err := state.Put(kind, id, task); err != nil {
					t.Fatal(err)
				}
				action = func() error { return app.TaskAction(context.Background(), id, "cancel") }
			case "cycle":
				cycle := discardableCycle(t, dataDir)
				cycle.Lifecycle.ArchivedAt = nil
				id = cycle.ID
				if err := state.Put(kind, id, cycle); err != nil {
					t.Fatal(err)
				}
				action = func() error { return app.CycleAction(id, "archive") }
			default:
				id = "control"
				action = func() error { _, err := app.ControlAction("pause"); return err }
			}
			before, err := store.Get[map[string]any](state, kind, id)
			if err != nil {
				t.Fatal(err)
			}
			schedulerSQL(t, state, `CREATE TEMP TRIGGER refuse_operator_mutation BEFORE UPDATE ON records
				BEGIN SELECT RAISE(ABORT, 'synthetic mutation refusal'); END`)
			if err := action(); err == nil || !strings.Contains(err.Error(), "synthetic mutation refusal") {
				t.Fatalf("mutation error = %v; want storage refusal", err)
			}
			after, err := store.Get[map[string]any](state, kind, id)
			if err != nil || !wirejson.Equal(before, after) {
				t.Fatalf("refused mutation changed durable record: %+v, %v", after, err)
			}
			requireNoOperatorActivity(t, state)
		})
	}
}
