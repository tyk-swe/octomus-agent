package store_test

import (
	"fmt"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestCancelledSessionCandidatesExcludeFinalizedHistoryBeforeLimit(t *testing.T) {
	s := open(t, statePath(t))
	for i := range 501 {
		completed := task()
		completed.ID = fmt.Sprintf("completed-%d", i)
		completed.Status = model.StatusCancelled
		completed.Sessions = []model.Session{{ID: "session", Status: model.SessionFailed}}
		must(t, s.Put("task", completed.ID, completed))
	}
	for _, id := range []string{"live", "cleanup", "orphan", "archived", "other-status"} {
		unfinished := task()
		unfinished.ID = id
		unfinished.Status = model.StatusCancelled
		unfinished.Sessions = []model.Session{{ID: "session", Status: model.SessionRunning}}
		if id == "archived" {
			unfinished.Lifecycle.ArchivedAt = str(model.Now())
			unfinished.Lifecycle.DiscardedAt = str(model.Now())
		}
		if id == "other-status" {
			unfinished.Status = model.StatusExecuting
		}
		must(t, s.Put("task", id, unfinished))
	}
	candidates, err := s.CancelledTasksWithRunningSessionsExcept([]string{"live", "cleanup"})
	must(t, err)
	if len(candidates) != 2 || candidates[0].ID != "orphan" || candidates[1].ID != "archived" {
		t.Fatalf("cancellation candidates after finalized history = %+v", candidates)
	}
}
