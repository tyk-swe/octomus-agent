package store_test

import (
	"maps"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestTaskHistoryCountsFollowFiltersAfterArchiveAndRetry(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	blocked := task()
	blocked.ID, blocked.Status = "blocked", model.StatusBlocked
	failed := task()
	failed.ID, failed.Status = "failed", model.StatusFailed
	queued := task()
	queued.ID = "queued"
	for _, row := range []model.Task{blocked, failed, queued} {
		must(t, s.Put("task", row.ID, row))
	}

	check := func(want map[string]int64, operationalAttention int64) {
		t.Helper()
		snapshot, err := s.Dashboard()
		must(t, err)
		if got := snapshot.Counts["blocked"] + snapshot.Counts["failed"]; got != operationalAttention {
			t.Fatalf("operational attention = %d, want %d", got, operationalAttention)
		}
		for _, status := range []string{"all", "active", "queued", "blocked", "failed", "attention", "published", "cancelled"} {
			page, err := s.HistoryPage("task", store.HistoryQuery{Status: &status})
			must(t, err)
			if got := int64(len(page.Items)); got != want[status] {
				t.Fatalf("%s: %d history rows, want %d", status, got, want[status])
			}
			if !maps.Equal(page.Counts, want) {
				t.Fatalf("%s: history counts %v, want %v (operational counts %v)", status, page.Counts, want, snapshot.Counts)
			}
		}
	}
	check(map[string]int64{"all": 3, "active": 0, "attention": 2, "blocked": 1, "failed": 1, "queued": 1}, 2)

	blocked.Lifecycle.ArchivedAt = str(model.Now())
	must(t, s.Put("task", blocked.ID, blocked))
	check(map[string]int64{"all": 3, "active": 0, "attention": 1, "blocked": 1, "failed": 1, "queued": 1}, 1)

	failed.Status = model.StatusQueued
	must(t, s.Put("task", failed.ID, failed))
	check(map[string]int64{"all": 3, "active": 0, "attention": 0, "blocked": 1, "queued": 2}, 0)

	queued.Status = model.StatusVerifying
	must(t, s.Put("task", queued.ID, queued))
	check(map[string]int64{"all": 3, "active": 1, "attention": 0, "blocked": 1, "queued": 1, "verifying": 1}, 0)
}

func TestTaskHistoryCountsCoverAllPagesAndKeepSearchIndependent(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		row := task()
		row.ID = id
		must(t, s.Put("task", row.ID, row))
	}
	want := map[string]int64{"all": 3, "active": 0, "attention": 0, "queued": 3}
	first, err := s.HistoryPage("task", store.HistoryQuery{Limit: new(1)})
	must(t, err)
	if first.NextCursor == nil || len(first.Items) != 1 || !maps.Equal(first.Counts, want) {
		t.Fatalf("first page: %+v", first)
	}
	second, err := s.HistoryPage("task", store.HistoryQuery{Limit: new(1), Before: first.NextCursor})
	must(t, err)
	if len(second.Items) != 1 || !maps.Equal(second.Counts, want) {
		t.Fatalf("second page: %+v", second)
	}
	empty, err := s.HistoryPage("task", store.HistoryQuery{Q: str("no matching history")})
	must(t, err)
	if len(empty.Items) != 0 || !maps.Equal(empty.Counts, want) {
		t.Fatalf("search page: %+v", empty)
	}
	for _, kind := range []string{"cycle", "pr"} {
		page, err := s.HistoryPage(kind, store.HistoryQuery{})
		must(t, err)
		if len(page.Counts) != 0 {
			t.Fatalf("%s inherited task counts: %v", kind, page.Counts)
		}
	}
}
