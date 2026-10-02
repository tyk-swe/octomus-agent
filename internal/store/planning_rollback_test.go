package store_test

import (
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestCommitPlanRollsBackLateWriteFailureAndRetriesAfterReopen(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	old := reviewTask()
	old.Status, old.RediscoveryRequested = model.StatusCancelled, true
	must(t, s.Put("task", old.ID, old))
	replacement := reviewTask()
	replacement.Supersedes, replacement.Proposal.Reconsiders = []string{old.ID}, []string{old.ID}
	plan := cycleFor(replacement)
	plan.Status, plan.CompletedAt, plan.RunID = model.CycleCompleted, str(model.Now()), str("run")
	replacement.CycleID, replacement.RunID = plan.ID, plan.RunID
	decision := map[string]any{"id": "decision", "repository": plan.Repository}
	plan.DecisionMemory = []any{decision}
	running := plan.Clone()
	running.Status, running.CompletedAt, running.Proposals, running.DecisionMemory = model.CycleRunning, nil, nil, nil
	must(t, s.Put("cycle", running.ID, running))
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeRunOnce)
	control.IdleStreak = 3
	control.Batch = &model.RunBatch{ID: *plan.RunID, Phase: model.BatchPhasePlanning, CycleID: &plan.ID}
	must(t, s.SaveControl(control))

	assertState := func(s *store.Store, committed bool) {
		t.Helper()
		wantCycle, wantOld, wantControl := running.Clone(), old.Clone(), control.Clone()
		var wantTask, wantDecision any
		var queued int64
		if committed {
			wantCycle, wantTask, wantDecision, queued = plan, replacement, decision, 1
			wantOld.SupersededBy = []string{replacement.ID}
			wantOld.RediscoveryRequested = false
			wantOld.RediscoveryResult = str(replacement.Proposal.Decision + ": " + replacement.Proposal.Reason)
			wantControl.Batch.Phase, wantControl.IdleStreak = model.BatchPhaseExecuting, 0
		}
		for _, record := range []struct {
			kind, id string
			want     any
		}{
			{"cycle", plan.ID, wantCycle},
			{"task", old.ID, wantOld},
			{"task", replacement.ID, wantTask},
			{"decision", "decision", wantDecision},
			{"settings", "control", wantControl},
		} {
			saved, found, err := s.GetValue(record.kind, record.id)
			must(t, err)
			if found != (record.want != nil) || !equalJSON(t, saved, record.want) {
				t.Errorf("%s %s: got %s, want %s", record.kind, record.id, canonical(t, saved), canonical(t, record.want))
			}
		}
		// Exercise the projections used by scheduling and history, as well as the records.
		tasks, err := s.HistoryPage("task", store.HistoryQuery{})
		must(t, err)
		if int64(len(tasks.Items)) != 1+queued || tasks.Counts["all"] != 1+queued || tasks.Counts["cancelled"] != 1 || tasks.Counts["queued"] != queued {
			t.Errorf("task history: %d items, counts %v; want one cancelled and %d queued", len(tasks.Items), tasks.Counts, queued)
		}
		cycles, err := s.RunningCycles()
		must(t, err)
		if int64(len(cycles)) != 1-queued || (len(cycles) == 1 && cycles[0].ID != plan.ID) {
			t.Errorf("running cycles: %+v; committed=%t", cycles, committed)
		}
		proposals, err := s.ProposalPage(store.HistoryQuery{Cycle: &plan.ID})
		must(t, err)
		if int64(len(proposals.Items)) != queued || proposals.Counts[model.DecisionAccepted] != queued {
			t.Errorf("proposal history: %d items, counts %v; want %d accepted", len(proposals.Items), proposals.Counts, queued)
		}
		if pending, unresolved, err := s.BatchCounts(*plan.RunID); err != nil || pending != uint64(queued) || unresolved != 0 {
			t.Errorf("batch counts: pending=%d unresolved=%d, %v; want %d pending", pending, unresolved, err, queued)
		}
	}

	// The final control write follows the cycle, queued task, lineage, decision
	// memory and rediscovery result. ABORT rejects only that statement; CommitPlan
	// must roll back all preceding writes and their trigger-maintained projections.
	db := raw(t, path)
	exec(t, db, `CREATE TRIGGER refuse_plan_control AFTER UPDATE ON records
		WHEN NEW.kind='settings' AND NEW.id='control'
		BEGIN SELECT RAISE(ABORT, 'injected plan control refusal'); END`)
	if err := s.CommitPlan(plan, []model.Task{replacement}); err == nil || !strings.Contains(err.Error(), "injected plan control refusal") {
		t.Fatalf("late plan write error = %v; want injected control refusal", err)
	}
	assertState(s, false)
	must(t, s.Close())
	s = open(t, path)
	assertState(s, false)

	exec(t, db, "DROP TRIGGER refuse_plan_control")
	must(t, s.CommitPlan(plan, []model.Task{replacement}))
	assertState(s, true)
	must(t, s.Close())
	s = open(t, path)
	assertState(s, true)
}
