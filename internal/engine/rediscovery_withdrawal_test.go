package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestArchiveDuringConsolidationWithdrawsRediscovery(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{model.DecisionAccepted, model.DecisionRejected} {
		t.Run(decision, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			request := saveRediscoveryRequest(t, fixture)
			proposal := seededRediscovery(request, decision, "Decided against current context.", request.ID)
			plan := rediscoveryPlan(t, fixture, request, proposal, fixtureProposal(model.DecisionRejected, "Not needed now."))
			gate := runnertest.NewGate()
			defer gate.Release()
			plan.consolidation.Gate = gate
			plan.queue(fixture)
			app := fixture.pausedApp(t)
			if err := app.RunOnce(); err != nil {
				t.Fatal(err)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gate.Entered():
			case <-time.After(30 * time.Second):
				t.Fatal("consolidation did not reach its response barrier")
			}
			if err := app.TaskAction(context.Background(), request.ID, "archive"); err != nil {
				t.Fatal(err)
			}
			archived, err := store.Get[model.Task](fixture.state, "task", request.ID)
			if err != nil || archived == nil || archived.Lifecycle.ArchivedAt == nil {
				t.Fatalf("request was not archived: %+v, %v", archived, err)
			}
			if pending, err := fixture.state.RediscoveryRequests(fixture.cfg.GitHubRepo); err != nil || len(pending) != 0 {
				t.Fatalf("archived request remains pending: %+v, %v", pending, err)
			}
			gate.Release()
			cycle := waitOnlyCycle(t, fixture.state)
			app.wg.Wait()
			if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "archived") {
				t.Errorf("withdrawn rediscovery committed: status=%s error=%s", cycle.Status, optionalText(cycle.Error))
			}
			tasks, err := store.List[model.Task](fixture.state, "task")
			if err != nil || len(tasks) != 1 {
				t.Fatalf("withdrawal created replacement work: tasks=%d, %v", len(tasks), err)
			}
			if !reflect.DeepEqual(tasks[0], *archived) {
				t.Fatal("withdrawal changed the archived task's saved evidence or lineage")
			}
			if reservations, err := fixture.state.PrReservations(fixture.cfg.GitHubRepo); err != nil || len(reservations) != 0 {
				t.Fatalf("withdrawn rediscovery reserved PR capacity: %+v, %v", reservations, err)
			}
			if memory, err := fixture.state.DecisionMemory(fixture.cfg.GitHubRepo); err != nil || len(memory) != 0 {
				t.Fatalf("withdrawn rediscovery committed decision memory: %+v, %v", memory, err)
			}
		})
	}
}
