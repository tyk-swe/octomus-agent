package engine

// The scheduler: dispatch order, writer serialization and the planning preflight.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestRunOnceAcceptsPublishedDependency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		runID *string
	}{
		{name: "published in Continuous"},
		{name: "published in earlier RunOnce", runID: new("earlier-batch")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
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
			app := New(state, t.TempDir())
			app.supervise = func(_ context.Context, task model.Task) error {
				task.Status = model.StatusPublished
				return state.Put("task", task.ID, task)
			}
			cleanupApp(t, app)
			deferHousekeeping(app)
			if err := control(app, "cycle"); err != nil {
				t.Fatal(err)
			}
			if err := app.tick(); err != nil {
				t.Fatal(err)
			}
			waitApp(t, app)
			saved, err := store.Get[model.Task](state, "task", dependent.ID)
			if err != nil || saved == nil || saved.Status != model.StatusPublished {
				t.Fatalf("published prerequisite blocked its RunOnce successor: %+v, %v", saved, err)
			}
		})
	}
}

// A runner that refuses to connect fails the preflight before any cycle, admission or workspace exists.
func TestPreflightRefusesNoAuth(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"audit", "run_once", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			for range 2 {
				f.script.FailConnect(config.BackendCodex, errors.New("Codex authentication required: run codex login"))
			}
			app := f.pausedApp(t)
			if mode == "audit" {
				id, err := app.startAudit(context.Background())
				if err == nil || !strings.Contains(err.Error(), "authentication") || id != "" {
					t.Fatalf("unauthenticated audit passed preflight: id=%q err=%v", id, err)
				}
			} else {
				action := "cycle"
				if mode == "continuous" {
					action = "resume"
				}
				if err := control(app, action); err != nil {
					t.Fatal(err)
				}
				if err := app.tick(); err != nil {
					t.Fatal(err)
				}
				waitApp(t, app)
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
			cycles, err := store.List[model.Cycle](f.state, "cycle")
			if err != nil || len(cycles) != 0 {
				t.Fatalf("unauthenticated preflight created cycles: %d, %v", len(cycles), err)
			}
			assertAdmissions(t, f.state, 0, "unauthenticated preflight")
			waitApp(t, app)
			if !app.Drained() {
				t.Fatal("failed preflight retained runtime work")
			}
		})
	}
}

func TestOneWriterPerBranch(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.ExecutionConcurrency = 2
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = int64(^uint64(0) >> 1)
	saveSettings(t, state, cfg, control)
	first := queuedTask(cfg, "first", "octomus/existing", "octomus/existing")
	second := queuedTask(cfg, "second", "octomus/existing", "octomus/existing")
	second.Proposal.Dependencies = []string{first.ID}
	if err := state.Put("task", first.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("task", second.ID, second); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	release := make(chan struct{})
	published := make(chan string, 2)
	runner := func(_ context.Context, task model.Task) error {
		started <- task.ID
		current, err := store.Get[model.Task](state, "task", task.ID)
		if err != nil || current == nil {
			return fmt.Errorf("reload task: %v", err)
		}
		current.Status = model.StatusPublished
		current.UpdatedAt = model.Now()
		if err := state.Put("task", current.ID, *current); err != nil {
			return err
		}
		published <- task.ID
		<-release
		return nil
	}
	a := New(state, t.TempDir())
	a.supervise = runner
	cleanupApp(t, a)
	deferHousekeeping(a)
	if err := a.tick(); err != nil {
		t.Fatal(err)
	}
	if id := <-started; id != first.ID {
		t.Fatalf("started %s before prerequisite %s", id, first.ID)
	}
	select {
	case id := <-started:
		t.Fatalf("started same-branch writer concurrently: %s", id)
	default:
	}
	if id := <-published; id != first.ID {
		t.Fatalf("published unexpected task %s", id)
	}
	if err := a.tick(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		t.Fatalf("started same-branch writer before the first runner exited: %s", id)
	default:
	}
	close(release)
	waitApp(t, a)
	if err := a.tick(); err != nil {
		t.Fatal(err)
	}
	if id := <-started; id != second.ID {
		t.Fatalf("did not start dependent after publication: %s", id)
	}
	<-published
	waitApp(t, a)
}
