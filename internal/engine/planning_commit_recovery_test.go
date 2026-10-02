package engine

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

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
