package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

type cancelPlanOnMarshal context.CancelFunc

func (cancel cancelPlanOnMarshal) MarshalJSON() ([]byte, error) {
	cancel()
	return []byte(`{"admitted":true}`), nil
}

func TestCommitPlanFinishesAfterAdmissionDespiteCancellation(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	queued := task()
	plan := cycleFor(queued)
	queued.CycleID = plan.ID
	plan.RunID = str("run")
	plan.Status = model.CycleCompleted
	plan.CompletedAt = str(model.Now())
	decision := map[string]any{"id": "decision", "repository": plan.Repository}
	plan.DecisionMemory = []any{decision}
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeRunOnce)
	control.IdleStreak = 3
	control.Batch = &model.RunBatch{ID: *plan.RunID, Phase: model.BatchPhasePlanning, CycleID: &plan.ID}
	must(t, s.SaveControl(control))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Serialization happens inside the admitted transaction, before its first write.
	plan.Assessments = []any{cancelPlanOnMarshal(cancel)}
	must(t, s.CommitPlanContext(ctx, plan, []model.Task{queued}))
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("plan serialization did not cancel the context")
	}
	plan.Assessments = []any{map[string]any{"admitted": true}}
	if saved, err := store.Get[model.Cycle](s, "cycle", plan.ID); err != nil || saved == nil || !equalJSON(t, *saved, plan) {
		t.Fatalf("admitted cycle was not committed completely: %+v, %v", saved, err)
	}
	if saved, err := store.Get[model.Task](s, "task", queued.ID); err != nil || saved == nil || !equalJSON(t, *saved, queued) {
		t.Fatalf("admitted task was not committed completely: %+v, %v", saved, err)
	}
	if saved, found, err := s.GetValue("decision", "decision"); err != nil || !found || !equalJSON(t, saved, decision) {
		t.Fatalf("admitted decision was not committed completely: %+v, %v", saved, err)
	}
	control.IdleStreak = 0
	control.Batch.Phase = model.BatchPhaseExecuting
	if saved, err := store.Get[model.Control](s, "settings", "control"); err != nil || saved == nil || !equalJSON(t, *saved, control) {
		t.Fatalf("admitted control was not committed completely: %+v, %v", saved, err)
	}
}
