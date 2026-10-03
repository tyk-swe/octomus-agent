package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestPlanningRolePanicFailsCycle(t *testing.T) {
	for _, role := range []string{"grounding", "discovery", "review", "consolidation"} {
		t.Run(role, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			plan := completePlan(t, fixture)
			wantSessions := 1
			reply := runnertest.Reply{Effect: func(string) error { panic("planning fixture failed") }}
			switch role {
			case "grounding":
				plan.grounding = reply
			case "discovery":
				wantSessions += int(fixture.cfg.DiscoveryAgents)
				plan.discovery[0] = reply
			case "review":
				wantSessions += int(fixture.cfg.DiscoveryAgents) + len(model.ReviewerSlots())
				plan.reviews[0] = reply
			case "consolidation":
				wantSessions += int(fixture.cfg.DiscoveryAgents) + len(model.ReviewerSlots()) + 1
				plan.consolidation = reply
			}
			plan.queue(fixture)
			app := fixture.pausedApp(t)
			id, err := app.StartAudit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cycle := waitCycle(t, fixture.state, id)
			if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "planning fixture failed") {
				t.Fatalf("panic outcome = %+v", cycle)
			}
			if len(cycle.Sessions) != wantSessions {
				t.Fatalf("sessions = %d; want all %d joined roles", len(cycle.Sessions), wantSessions)
			}
			failures := 0
			for _, session := range cycle.Sessions {
				if session.Status == model.SessionFailed {
					failures++
				}
			}
			if failures != 1 {
				t.Fatalf("failed sessions = %d; sessions: %+v", failures, cycle.Sessions)
			}
			tasks, err := store.List[model.Task](fixture.state, "task")
			if err != nil || len(tasks) != 0 {
				t.Fatalf("tasks after panic = %+v, %v", tasks, err)
			}
			app.Shutdown()
			assertNoOpenClients(t, fixture.script)
			if app.runtime.cycle != nil {
				t.Fatal("failed planning retained its worker ownership")
			}
		})
	}
}

func TestPlanningCleanupPanicFailsCycle(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	completePlan(t, fixture).queue(fixture)
	app := fixture.pausedApp(t, WithWorkspaceRemoval(func(string, string) error { panic("planning cleanup failed") }))
	id, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, id)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "planning cleanup failed") {
		t.Fatalf("cleanup panic outcome = %+v", cycle)
	}
	if len(cycle.Sessions) != 1 || cycle.Sessions[0].Status != model.SessionCompleted {
		t.Fatalf("cleanup lost completed grounding evidence: %+v", cycle.Sessions)
	}
	app.Shutdown()
	assertNoOpenClients(t, fixture.script)
	if app.runtime.cycle != nil {
		t.Fatal("failed planning retained its worker ownership")
	}
}
