// The attention outbox the schema triggers maintain.

package store_test

import (
	"fmt"
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
	must(t, s.SetNotifyPolicy(str(notifyDest), "enabled", nil))
	want := []string{}
	for r := model.BlockedReason(0); r.String() != ""; r++ {
		putNotificationTask(t, s, "insert-"+r.String(), "blocked", r.String())
		putNotificationTask(t, s, "update-"+r.String(), "queued", "")
		putNotificationTask(t, s, "update-"+r.String(), "blocked", r.String())
		want = append(want, r.String(), r.String())
	}
	if len(want) < 2*int(model.BlockedUnknown+1) {
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

func TestReleaseNotificationEvents(t *testing.T) {
	t.Parallel()
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgraded-v0.1.0"}[migrated], func(t *testing.T) {
			path := statePath(t)
			if migrated {
				path = golden(t)
			}
			s := open(t, path)
			must(t, s.SetNotifyPolicy(str(notifyDest), "enabled", nil))
			if rows := pendingRows(t, path); len(rows) != 0 {
				t.Fatalf("old events were replayed: %+v", rows)
			}
			for i, event := range []struct{ kind, mode, status, category, action string }{
				{"task", "", "published", "task_published", "inspect_task"},
				{"cycle", "execution", "failed", "cycle_failed", "inspect_cycle"},
				{"cycle", "audit", "failed", "cycle_failed", "inspect_cycle"},
				{"cycle", "audit", "completed", "audit_completed", "inspect_cycle"},
				{"cycle", "audit", "idle", "audit_completed", "inspect_cycle"},
			} {
				for _, insert := range []bool{true, false} {
					id := fmt.Sprintf("release-event-%d-%t", i, insert)
					record := map[string]any{"id": id, "mode": event.mode, "status": "planning", "repository": "fixture/project", "run_id": "run-1", "cycle_id": "cycle-1", "config": map[string]any{"github_repo": "fixture/project"}}
					before := len(pendingRows(t, path))
					if !insert {
						must(t, s.Put(event.kind, id, record))
					}
					record["status"] = event.status
					must(t, s.Put(event.kind, id, record))
					// Repeated terminal saves and lifecycle-only changes are not new episodes.
					record["lifecycle"] = map[string]any{"archived_at": model.Now()}
					must(t, s.Put(event.kind, id, record))
					rows := pendingRows(t, path)
					if len(rows) != before+1 {
						t.Fatalf("%s generated %d events", id, len(rows)-before)
					}
					row := rows[len(rows)-1]
					if row["category"] != event.category || row["action"] != event.action || row["repository"] != "fixture/project" || row["run_id"] != "run-1" {
						t.Fatalf("event: %+v", row)
					}
					if event.kind == "cycle" {
						if row["cycle_id"] != id || row["task_id"] != nil {
							t.Fatalf("cycle attribution: %+v", row)
						}
					} else if row["task_id"] != id || row["cycle_id"] != "cycle-1" {
						t.Fatalf("task attribution: %+v", row)
					}
				}
			}
			before := len(pendingRows(t, path))
			for _, status := range []string{"planning", "interrupted", "cancelled", "completed", "idle"} {
				must(t, s.Put("cycle", "no-event-"+status, map[string]any{"mode": "execution", "status": status}))
			}
			if len(pendingRows(t, path)) != before {
				t.Fatal("non-event cycle statuses generated notifications")
			}
			must(t, s.SetNotifyPolicy(nil, "disabled", nil))
			putNotificationTask(t, s, "disabled-published", "published", "")
			must(t, s.Put("cycle", "disabled-failed", map[string]any{"status": "failed"}))
			must(t, s.Put("cycle", "disabled-audit", map[string]any{"mode": "audit", "status": "completed"}))
			if len(pendingRows(t, path)) != 0 {
				t.Fatal("disabled policy queued notifications")
			}
		})
	}
}
