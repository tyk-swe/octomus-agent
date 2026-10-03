package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

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
			assertAdmissions(t, fixture.state, 0, "unauthenticated preflight")
			if !app.runtimeIdle() {
				t.Fatal("failed preflight retained runtime work")
			}
		})
	}
}
