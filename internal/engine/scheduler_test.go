package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestTickAfterShutdownHasNoSideEffects(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"existing branch", "new PR", "planning", "paused"} {
		t.Run(scenario, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			if scenario != "paused" {
				control.SetMode(model.OperatingModeContinuous)
			}
			saveSettings(t, state, cfg, control)
			var queued *model.Task
			if scenario == "existing branch" || scenario == "new PR" {
				target := "tyk/existing"
				if scenario == "new PR" {
					target = cfg.DefaultBranch
				}
				task := queuedTask(cfg, "untouched", target, "tyk/existing")
				if err := state.Put("task", task.ID, task); err != nil {
					t.Fatal(err)
				}
				var err error
				queued, err = store.Get[model.Task](state, "task", task.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			started := make(chan struct{}, 1)
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
				started <- struct{}{}
				return nil
			})))
			t.Cleanup(app.Shutdown)
			app.Shutdown()
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			if len(started) != 0 {
				t.Error("dispatched work after shutdown")
			}
			if queued != nil {
				saved, err := store.Get[model.Task](state, "task", queued.ID)
				if err != nil || !reflect.DeepEqual(saved, queued) {
					t.Fatalf("shutdown tick changed queued work: %+v, %v", saved, err)
				}
			}
			live, err := app.Control()
			if err != nil || !reflect.DeepEqual(live, control) {
				t.Fatalf("shutdown tick changed control: %+v, %v", live, err)
			}
			if !app.runtime.lastRetention.IsZero() || !app.runtime.lastObserve.IsZero() || !app.runtime.lastPrAttempt.IsZero() {
				t.Fatal("shutdown tick launched background maintenance")
			}
		})
	}
}

func TestTaskCompletionCancelsContext(t *testing.T) {
	t.Parallel()
	for _, runErr := range []error{nil, errors.New("worker failed")} {
		name := "success"
		if runErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			task := queuedTask(cfg, "completed", "tyk/existing", "tyk/existing")
			task.Status = model.StatusExecuting
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			var workerCtx context.Context
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, task model.Task) error {
				workerCtx = ctx
				if runErr == nil {
					task.Status = model.StatusPublished
					return state.Put("task", task.ID, task)
				}
				return runErr
			})))
			t.Cleanup(app.Shutdown)
			app.runTask(task)
			app.wg.Wait()
			if workerCtx == nil || workerCtx.Err() != context.Canceled {
				t.Fatal("completed task retained a live child context")
			}
			if app.ctx.Err() != nil {
				t.Fatal("task completion canceled the application")
			}
		})
	}
}

func TestRunOnceAcceptsPreviouslyPublishedDependency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		runID *string
	}{
		{name: "published in Continuous"},
		{name: "published in earlier RunOnce", runID: stringPointer("earlier-batch")},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			saveSettings(t, state, cfg, model.DefaultControl())
			dependency := queuedTask(cfg, "dependency", "tyk/existing", "tyk/existing")
			dependency.Status = model.StatusPublished
			dependency.RunID = test.runID
			dependent := queuedTask(cfg, "dependent", dependency.Branch, dependency.Branch)
			dependent.Proposal.Dependencies = []string{dependency.ID}
			for _, task := range []model.Task{dependency, dependent} {
				if err := state.Put("task", task.ID, task); err != nil {
					t.Fatal(err)
				}
			}
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(_ context.Context, task model.Task) error {
				task.Status = model.StatusPublished
				return state.Put("task", task.ID, task)
			})))
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			if err := app.RunOnce(); err != nil {
				t.Fatal(err)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			saved, err := store.Get[model.Task](state, "task", dependent.ID)
			if err != nil || saved == nil || saved.Status != model.StatusPublished {
				t.Fatalf("published prerequisite blocked its RunOnce successor: %+v, %v", saved, err)
			}
		})
	}
}

func TestPlanningPreflightRejectsMissingAuthenticationBeforeSideEffects(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"audit", "run_once", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPlanningFixture(t)
			if err := os.WriteFile(filepath.Join(fixture.root, "codex-mode"), []byte("no-auth"), 0o600); err != nil {
				t.Fatal(err)
			}
			app := New(fixture.state, fixture.dataDir)
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			if mode == "audit" {
				id, err := app.StartAudit(context.Background())
				if err == nil || !strings.Contains(err.Error(), "authentication") || id != "" {
					t.Fatalf("unauthenticated audit passed preflight: id=%q err=%v", id, err)
				}
			} else {
				var err error
				if mode == "run_once" {
					err = app.RunOnce()
				} else {
					err = app.Resume()
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
				app.wg.Wait()
				control, err := app.Control()
				if err != nil || control.Error == nil || !strings.Contains(*control.Error, "authentication") {
					t.Fatalf("missing durable preflight authentication error: %+v, %v", control, err)
				}
				if mode == "run_once" && (control.Mode != model.OperatingModePaused || control.Batch != nil) {
					t.Fatalf("RunOnce did not pause after failed preflight: %+v", control)
				}
				if mode == "continuous" && (control.Mode != model.OperatingModeContinuous || control.NextCycleAt <= time.Now().Unix()) {
					t.Fatalf("Continuous did not defer after failed preflight: %+v", control)
				}
			}
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 0 {
				t.Fatalf("unauthenticated preflight created cycles: %d, %v", len(cycles), err)
			}
			used, err := fixture.state.SessionsToday()
			if err != nil || used != 0 {
				t.Fatalf("unauthenticated preflight consumed admissions: %d, %v", used, err)
			}
			if !app.runtimeIdle() {
				t.Fatal("failed preflight retained runtime work")
			}
		})
	}
}
