package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

type observedPlanningContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *observedPlanningContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestPlanningCommitCancellationWhileWaitingForStore(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"audit", "run once", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			cycle := model.Cycle{ID: "cycle", Number: 1, Mode: model.CycleModeExecution, Status: model.CycleRunning,
				StartedAt: model.Now(), Repository: cfg.GitHubRepo, Proposals: []model.Proposal{}, Sessions: []model.Session{}, Assessments: []any{}, DecisionMemory: []any{}}
			task := queuedTask(cfg, "planned-task", cfg.DefaultBranch, "tyk/planned-task")
			task.CycleID = cycle.ID
			tasks := []model.Task{task}
			switch mode {
			case "audit":
				cycle.Mode = model.CycleModeAudit
				tasks = nil
			case "run once":
				control.SetMode(model.OperatingModeRunOnce)
				control.Batch = &model.RunBatch{ID: "batch", Phase: model.BatchPhasePlanning, CycleID: &cycle.ID}
				cycle.RunID = &control.Batch.ID
			default:
				control.SetMode(model.OperatingModeContinuous)
			}
			saveSettings(t, state, cfg, control)
			if err := state.Put("cycle", cycle.ID, cycle); err != nil {
				t.Fatal(err)
			}
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			checked := make(chan struct{})
			app.ctx = &observedPlanningContext{Context: app.ctx, checked: checked}
			held, released := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			t.Cleanup(release)
			snapshotDone := make(chan error, 1)
			go func() {
				snapshotDone <- state.Snapshot(func(*sql.Conn) error {
					close(held)
					<-released
					return nil
				})
			}()
			waitPlanningStorage(t, held)
			plan := cycle.Clone()
			plan.Status = model.CycleCompleted
			plan.CompletedAt = stringPointer(model.Now())
			plan.DecisionMemory = []any{map[string]any{"id": "decision", "repository": cfg.GitHubRepo}}
			commitDone := make(chan error, 1)
			go func() { commitDone <- app.commitPlan(plan, tasks) }()
			// The gate's check has observed a live service, but the store still belongs to the snapshot.
			waitPlanningStorage(t, checked)
			app.cancel()
			release()
			if err := waitPlanningStorage(t, snapshotDone); err != nil {
				t.Fatal(err)
			}
			if err := waitPlanningStorage(t, commitDone); !errors.Is(err, context.Canceled) {
				t.Fatalf("plan waiting for the store ignored shutdown: %v", err)
			}
			if saved, err := store.Get[model.Cycle](state, "cycle", cycle.ID); err != nil || saved == nil || !reflect.DeepEqual(*saved, cycle) {
				t.Fatalf("cancelled commit changed the cycle: %+v, %v", saved, err)
			}
			if saved, err := store.List[model.Task](state, "task"); err != nil || len(saved) != 0 {
				t.Fatalf("cancelled commit queued work: %+v, %v", saved, err)
			}
			if saved, err := state.DecisionMemory(cfg.GitHubRepo); err != nil || len(saved) != 0 {
				t.Fatalf("cancelled commit saved decision memory: %+v, %v", saved, err)
			}
			if saved, err := app.Control(); err != nil || !reflect.DeepEqual(saved, control) {
				t.Fatalf("cancelled commit advanced control: %+v, %v", saved, err)
			}
		})
	}
}

func TestShutdownAfterConsolidationDoesNotCommitThePlan(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"audit", "run once", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			completePlan(t, fixture).queue(fixture)
			app := fixture.pausedApp(t)
			cleanup, released := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(released) })
			t.Cleanup(release)
			remove := app.removeDir
			app.removeDir = func(root, path string) error {
				if filepath.Base(path) == "consolidation" {
					close(cleanup)
					<-released
				}
				return remove(root, path)
			}
			switch mode {
			case "audit":
				if _, err := app.StartAudit(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "run once":
				if err := app.RunOnce(); err != nil {
					t.Fatal(err)
				}
			case "continuous":
				if err := app.Resume(); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "audit" {
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
			}
			waitPlanningStorage(t, cleanup)
			started, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			stopped := make(chan struct{})
			go func() {
				app.Shutdown()
				close(stopped)
			}()
			waitPlanningStorage(t, app.Context().Done())
			release()
			waitPlanningStorage(t, stopped)

			cycle := waitOnlyCycle(t, fixture.state)
			if cycle.Status != model.CycleInterrupted || cycle.Error == nil || *cycle.Error != interruptedPlanningMessage || cycle.CompletedAt == nil {
				t.Fatalf("shutdown after completed consolidation saved cycle status=%s error=%s", cycle.Status, optionalText(cycle.Error))
			}
			for _, session := range cycle.Sessions {
				if session.Status != model.SessionCompleted {
					t.Fatalf("shutdown changed the already completed role evidence: %+v", session)
				}
			}
			if len(cycle.Sessions) != int(fixture.cfg.PlanningAdmissionsRequired()) {
				t.Fatalf("shutdown lost completed planning sessions: %d", len(cycle.Sessions))
			}
			if tasks, err := store.List[model.Task](fixture.state, "task"); err != nil || len(tasks) != 0 {
				t.Fatalf("shutdown before plan commit queued work: %+v, %v", tasks, err)
			}
			if decisions, err := fixture.state.DecisionMemory(fixture.cfg.GitHubRepo); err != nil || len(decisions) != 0 {
				t.Fatalf("shutdown before plan commit wrote decision memory: %+v, %v", decisions, err)
			}
			control, err := app.Control()
			if err != nil || !reflect.DeepEqual(control, started) {
				t.Fatalf("shutdown changed planning control: got %+v; want %+v, %v", control, started, err)
			}
			if events := allPlanningErrors(t, fixture.state); len(events) != 0 {
				t.Fatalf("shutdown logged planning errors: %+v", events)
			}

			restarted := New(fixture.state, fixture.dataDir)
			t.Cleanup(restarted.Shutdown)
			if err := restarted.Recover(); err != nil {
				t.Fatal(err)
			}
			recovered, err := restarted.Control()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "run once" {
				if recovered.Mode != model.OperatingModePaused || recovered.Batch != nil || recovered.Error == nil || *recovered.Error != "Run once was interrupted before its planning transaction committed" {
					t.Fatalf("restart retained interrupted planning: %+v", recovered)
				}
			} else if !reflect.DeepEqual(recovered, started) {
				t.Fatalf("restart changed planning control: got %+v; want %+v", recovered, started)
			}
			if tasks, err := store.List[model.Task](fixture.state, "task"); err != nil || len(tasks) != 0 {
				t.Fatalf("restart queued interrupted planning: %+v, %v", tasks, err)
			}
			assertNoOpenClients(t, fixture.script)
		})
	}
}
