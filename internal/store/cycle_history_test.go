package store_test

import (
	"fmt"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestCycleHistoryCursorKeepsBoundaryAfterNewCyclesAndLifecycleUpdates(t *testing.T) {
	s := open(t, statePath(t))
	cycles := make([]model.Cycle, 4)
	for i := range cycles {
		cycles[i] = cycleFor(task())
		cycles[i].ID = fmt.Sprintf("cycle-%d", i+1)
		cycles[i].Number = uint64(101 + i)
		cycles[i].Status = model.CycleCompleted
	}
	put := func(cycle model.Cycle) {
		t.Helper()
		// Other record kinds share the sequence; cycle numbers are not cursors.
		must(t, s.Put("settings", cycle.ID, map[string]string{"value": "fixture"}))
		must(t, s.Put("cycle", cycle.ID, cycle))
	}
	for _, cycle := range cycles[:3] {
		put(cycle)
	}
	first, err := s.HistoryPage("cycle", store.HistoryQuery{Limit: new(2)})
	must(t, err)
	if len(first.Items) != 2 || first.NextCursor == nil || decodeMap(t, first.Items[1])["id"] != "cycle-2" {
		t.Fatalf("initial history: %+v", first)
	}
	if *first.NextCursor == int64(cycles[1].Number) {
		t.Fatal("fixture must distinguish the opaque sequence cursor from cycle number")
	}
	put(cycles[3])
	for _, lifecycle := range []model.WorkspaceLifecycle{
		{ArchivedAt: str(model.Now())},
		{ArchivedAt: str(model.Now()), DiscardedAt: str(model.Now())},
	} {
		cycles[1].Lifecycle = lifecycle
		must(t, s.Put("cycle", cycles[1].ID, cycles[1]))
		refreshed, err := s.HistoryPage("cycle", store.HistoryQuery{Limit: new(3)})
		must(t, err)
		if len(refreshed.Items) != 3 || refreshed.NextCursor == nil || *refreshed.NextCursor != *first.NextCursor {
			t.Fatalf("new cycles or lifecycle update moved the loaded boundary: %+v", refreshed)
		}
		boundary := decodeMap(t, refreshed.Items[2])
		if boundary["id"] != "cycle-2" || !equalJSON(t, boundary["lifecycle"], lifecycle) {
			t.Fatalf("loaded boundary lost its lifecycle update: %v", boundary)
		}
		older, err := s.HistoryPage("cycle", store.HistoryQuery{Before: first.NextCursor, Limit: new(2)})
		must(t, err)
		if len(older.Items) != 1 || firstItem(t, older)["id"] != "cycle-1" || older.NextCursor != nil {
			t.Fatalf("saved cursor skipped or repeated cycles: %+v", older)
		}
	}
}
