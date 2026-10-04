// The attention outbox the schema triggers maintain.

package store_test

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const notifyDest = "destination-a"

func putNotificationTask(t *testing.T, s *store.Store, id, status string, reason string) {
	t.Helper()
	task := map[string]any{
		"id":       id,
		"cycle_id": "cycle-1",
		"run_id":   "run-1",
		"status":   status,
		"config":   map[string]any{"github_repo": "fixture/project"},
	}
	if reason != "" {
		task["blocked_reason"] = reason
	}
	must(t, s.Put("task", id, task))
}

func outboxRows(t *testing.T, path, status string) []map[string]any {
	t.Helper()
	db := raw(t, path)
	rows, err := db.Query("SELECT event_id,task_id,category,action,repository,cycle_id,run_id,last_error FROM notification_outbox WHERE status=? ORDER BY seq", status)
	must(t, err)
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var eventID, category, action, repository string
		var taskID, cycleID, runID, lastError *string
		must(t, rows.Scan(&eventID, &taskID, &category, &action, &repository, &cycleID, &runID, &lastError))
		out = append(out, map[string]any{
			"event_id": eventID, "task_id": strValue(taskID), "category": category,
			"action": action, "repository": repository, "cycle_id": strValue(cycleID),
			"run_id": strValue(runID), "last_error": strValue(lastError),
		})
	}
	return out
}

func strValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func pendingRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	return outboxRows(t, path, "pending")
}

func TestAttentionCategories(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	want := []string{}
	for r := model.BlockedReason(0); r.String() != ""; r++ {
		putNotificationTask(t, s, "insert-"+r.String(), "blocked", r.String())
		putNotificationTask(t, s, "update-"+r.String(), "queued", "")
		putNotificationTask(t, s, "update-"+r.String(), "blocked", r.String())
		want = append(want, r.String(), r.String())
	}
	if len(want) < 2*int(model.BlockedReasonUnknown+1) {
		t.Fatalf("only %d reasons were enumerated", len(want)/2)
	}
	rows := pendingRows(t, path)
	if len(rows) != len(want) {
		t.Fatalf("%d pending rows; want %d", len(rows), len(want))
	}
	for i, row := range rows {
		if row["category"] != want[i] || row["action"] != "inspect_task" {
			t.Fatalf("row %d %+v; want category %q", i, row, want[i])
		}
	}
}
