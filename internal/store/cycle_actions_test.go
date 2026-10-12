package store_test

import (
	"slices"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestIndexedCycleActionsFollowPersistedLifecycle(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	cycle := cycleFor(task())
	cycle.Status = model.CycleCompleted
	check := func(expected []model.CycleAction) {
		t.Helper()
		must(t, store.Put(s, store.Cycles.Key(cycle.ID), cycle))
		page, err := s.CyclePage(store.HistoryQuery{})
		must(t, err)
		if len(page.Items) != 1 || !slices.Equal(page.Items[0].AllowedActions, expected) {
			t.Fatalf("cycle actions: %+v; want %v", page.Items, expected)
		}
		dashboard, err := s.Dashboard()
		must(t, err)
		if !slices.Equal(dashboard.Cycles[0].AllowedActions, expected) {
			t.Fatalf("dashboard actions: %v", dashboard.Cycles[0].AllowedActions)
		}
	}
	check([]model.CycleAction{model.CycleActionArchive})
	cycle.Lifecycle.ArchivedAt = new(model.Now())
	check([]model.CycleAction{model.CycleActionDiscard})
	cycle.Lifecycle.DiscardedAt = new(model.Now())
	check([]model.CycleAction{})
	cycle.Lifecycle = model.WorkspaceLifecycle{}
	cycle.Status = model.CycleRunning
	check([]model.CycleAction{})
}
