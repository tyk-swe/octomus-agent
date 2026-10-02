package store_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestCommitPlanRechecksRediscoveryWithdrawal(t *testing.T) {
	t.Parallel()
	for _, decision := range []string{model.DecisionAccepted, model.DecisionRejected} {
		for _, archived := range []bool{false, true} {
			name := decision + "/pending"
			if archived {
				name = decision + "/archived"
			}
			t.Run(name, func(t *testing.T) {
				path := statePath(t)
				s := open(t, path)
				saveConfig(t, s, func(c *config.Config) { c.GitHubRepo = "fixture/project" })
				old := reviewTask()
				old.Status = model.StatusCancelled
				old.RediscoveryRequested = true
				must(t, s.Put("task", old.ID, old))

				replacement := reviewTask()
				replacement.Proposal.Decision = decision
				replacement.Proposal.Reconsiders = []string{old.ID}
				replacement.Supersedes = []string{old.ID}
				plan := cycleFor(replacement)
				plan.Status = model.CycleCompleted
				plan.RunID = str("run-1")
				plan.DecisionMemory = []any{map[string]any{"id": "decision-1", "repository": old.Config.GitHubRepo}}
				replacement.CycleID, replacement.RunID = plan.ID, plan.RunID
				unrelated := reviewTask()
				unrelated.CycleID, unrelated.RunID = plan.ID, plan.RunID
				unrelated.Proposal.ID = "unrelated"
				plan.Proposals = append(plan.Proposals, unrelated.Proposal)
				tasks := []model.Task{unrelated}
				if decision == model.DecisionAccepted {
					tasks = append(tasks, replacement)
				}
				originalCycle := plan.Clone()
				originalCycle.Status = model.CycleRunning
				originalCycle.Proposals = nil
				originalCycle.DecisionMemory = nil
				must(t, s.Put("cycle", plan.ID, originalCycle))
				control := model.DefaultControl()
				control.SetMode(model.OperatingModeRunOnce)
				control.Batch = &model.RunBatch{ID: "run-1", Phase: model.BatchPhasePlanning, CycleID: &plan.ID}
				must(t, s.SaveControl(control))
				must(t, s.ReserveSession(0, store.NewAdmission(plan.ID, nil, "consolidation", config.NewRoute("fixture", "low"))))

				// The plan has already captured this request when the operator archives it.
				if archived {
					old.Lifecycle.ArchivedAt = str(model.Now())
					must(t, s.Put("task", old.ID, old))
				}
				err := s.CommitPlan(plan, tasks)
				if !archived {
					must(t, err)
					resolved, err := store.Get[model.Task](s, "task", old.ID)
					must(t, err)
					if resolved == nil || resolved.RediscoveryRequested || resolved.RediscoveryResult == nil || *resolved.RediscoveryResult != decision+": "+replacement.Proposal.Reason {
						t.Fatalf("pending request did not resolve: %+v", resolved)
					}
					wantLineage := []string{}
					if decision == model.DecisionAccepted {
						wantLineage = append(wantLineage, replacement.ID)
					}
					if !reflect.DeepEqual(resolved.SupersededBy, wantLineage) {
						t.Fatalf("resolved lineage=%v; want %v", resolved.SupersededBy, wantLineage)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "archived") {
					t.Errorf("plan referencing an archived request committed: %v", err)
				}
				saved, err := store.Get[model.Task](s, "task", old.ID)
				must(t, err)
				if saved == nil || !reflect.DeepEqual(*saved, old) {
					t.Error("rejection changed the archived request's saved evidence or lineage")
				}
				savedCycle, err := store.Get[model.Cycle](s, "cycle", plan.ID)
				must(t, err)
				if savedCycle == nil || !equalJSON(t, savedCycle, originalCycle) {
					t.Error("rejection changed the saved cycle")
				}
				savedControl, err := store.Get[model.Control](s, "settings", "control")
				must(t, err)
				if savedControl == nil || !reflect.DeepEqual(*savedControl, control) {
					t.Errorf("rejection changed control: %+v", savedControl)
				}
				for _, task := range tasks {
					if saved, err := store.Get[model.Task](s, "task", task.ID); err != nil || saved != nil {
						t.Errorf("task %s survived rejection: present=%t, %v", task.ID, saved != nil, err)
					}
				}
				if memory, err := s.DecisionMemory(old.Config.GitHubRepo); err != nil || len(memory) != 0 {
					t.Errorf("decision memory survived rejection: %+v, %v", memory, err)
				}
				if pending, unresolved, err := s.BatchCounts("run-1"); err != nil || pending != 0 || unresolved != 0 {
					t.Errorf("batch membership survived rejection: pending=%d unresolved=%d, %v", pending, unresolved, err)
				}
				if reservations, err := s.PrReservations(old.Config.GitHubRepo); err != nil || len(reservations) != 0 {
					t.Errorf("rejection reserved PR capacity: %+v, %v", reservations, err)
				}
				if count := queryString(t, raw(t, path), "SELECT count(*) FROM admissions"); count != "1" {
					t.Errorf("rejection changed spent planning admissions: %s; want 1", count)
				}
			})
		}
	}
}
