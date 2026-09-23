package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

const notifyDest = "destination-a"

// putNotificationTask creates the minimal record the
// attention triggers inspect.
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

func strEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func pendingRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	return outboxRows(t, path, "pending")
}

func TestDisabledPolicyCapturesNothing(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.ConfigureNotifications(nil, "disabled", nil))
	putNotificationTask(t, s, "task-1", "blocked", "verification_failed")
	health, err := s.NotificationHealth()
	must(t, err)
	if health.State != "disabled" || health.Configured || health.Pending != 0 {
		t.Fatalf("%+v", health)
	}
	if rows := pendingRows(t, path); len(rows) != 0 {
		t.Fatalf("%d pending rows", len(rows))
	}
}

func TestAttentionTriggersEnqueueOneRowPerEpisode(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	putNotificationTask(t, s, "task-1", "queued", "")
	if rows := pendingRows(t, path); len(rows) != 0 {
		t.Fatal("queued tasks capture nothing")
	}
	putNotificationTask(t, s, "task-1", "blocked", "verification_failed")
	rows := pendingRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	row := rows[0]
	if row["task_id"] != "task-1" || row["category"] != "verification_failed" ||
		row["action"] != "inspect_task" || row["repository"] != "fixture/project" ||
		row["cycle_id"] != "cycle-1" || row["run_id"] != "run-1" ||
		len(row["event_id"].(string)) != 32 {
		t.Fatalf("%+v", row)
	}
	putNotificationTask(t, s, "task-1", "blocked", "storage_limit")
	if rows := pendingRows(t, path); len(rows) != 1 {
		t.Fatal("same-state writes never duplicate")
	}
	putNotificationTask(t, s, "task-1", "executing", "")
	putNotificationTask(t, s, "task-1", "blocked", "verification_failed")
	rows = pendingRows(t, path)
	if len(rows) != 2 || rows[0]["event_id"] == rows[1]["event_id"] {
		t.Fatalf("a new attempt is a new episode: %+v", rows)
	}
	putNotificationTask(t, s, "task-2", "failed", "")
	rows = pendingRows(t, path)
	if len(rows) != 3 || rows[2]["category"] != "unknown" {
		t.Fatalf("%+v", rows)
	}
}

func TestControlErrorPauseEnqueuesOncePerPauseEpisode(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.Put("settings", "config", map[string]any{
		"github_repo": "fixture/project", "retain_events": 100,
	}))
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	control := func(paused bool, errorText string) map[string]any {
		var errValue any
		if errorText != "" {
			errValue = errorText
		}
		return map[string]any{
			"paused": paused, "error": errValue,
			"batch": map[string]any{"id": "run-9", "phase": "queued", "cycle_id": "cycle-9"},
		}
	}
	must(t, s.Put("settings", "control", control(true, "boom")))
	rows := pendingRows(t, path)
	if len(rows) != 1 || rows[0]["category"] != "service_error_paused" ||
		rows[0]["action"] != "inspect_service" || rows[0]["repository"] != "fixture/project" ||
		rows[0]["run_id"] != "run-9" || rows[0]["cycle_id"] != "cycle-9" ||
		rows[0]["task_id"] != nil {
		t.Fatalf("%+v", rows)
	}
	must(t, s.Put("settings", "control", control(true, "still broken")))
	if rows := pendingRows(t, path); len(rows) != 1 {
		t.Fatal("error changes while paused never duplicate")
	}
	must(t, s.Put("settings", "control", control(false, "")))
	must(t, s.Put("settings", "control", control(true, "again")))
	if rows := pendingRows(t, path); len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
}

func TestConfigurePreservesSameDestinationAndCancelsOnChange(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	putNotificationTask(t, s, "early", "blocked", "timeout")
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	if rows := pendingRows(t, path); len(rows) != 0 {
		t.Fatal("enabling never backfills history")
	}
	putNotificationTask(t, s, "task-1", "blocked", "timeout")
	if rows := pendingRows(t, path); len(rows) != 1 {
		t.Fatal("blocked task was not captured")
	}
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	if rows := pendingRows(t, path); len(rows) != 1 {
		t.Fatal("same destination restart keeps pending")
	}
	must(t, s.ConfigureNotifications(str("destination-b"), "enabled", nil))
	if rows := pendingRows(t, path); len(rows) != 0 {
		t.Fatal("destination change keeps pending rows")
	}
	cancelled := outboxRows(t, path, "cancelled")
	if len(cancelled) != 1 || cancelled[0]["last_error"] != "destination_changed" {
		t.Fatalf("%+v", cancelled)
	}
	putNotificationTask(t, s, "task-2", "blocked", "timeout")
	must(t, s.ConfigureNotifications(nil, "disabled", nil))
	if rows := pendingRows(t, path); len(rows) != 0 {
		t.Fatal("disabling cancels pending")
	}
}

func TestClaimPreschedulesFiveAttemptsAndKeepsPayloadFrozen(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	putNotificationTask(t, s, "task-1", "blocked", "publication_uncertain")
	now := time.Now().UTC()
	delivery, err := s.ClaimNotification(notifyDest, now)
	must(t, err)
	if delivery == nil || delivery.Attempts != 1 || len(delivery.EventID) != 32 {
		t.Fatalf("%+v", delivery)
	}
	if again, _ := s.ClaimNotification(notifyDest, now); again != nil {
		t.Fatal("claimed rows reschedule before delivery")
	}
	putNotificationTask(t, s, "task-1", "blocked", "storage_limit")
	original := *delivery
	expected := int64(30)
	for attempt := int64(2); attempt <= 5; attempt++ {
		now = now.Add(time.Duration(expected) * time.Second)
		next, err := s.ClaimNotification(notifyDest, now)
		must(t, err)
		if next == nil || next.Attempts != attempt || next.EventID != delivery.EventID {
			t.Fatalf("%+v", next)
		}
		same := next.Seq == original.Seq && next.CreatedAt == original.CreatedAt &&
			next.Repository == original.Repository && next.Category == original.Category &&
			next.Action == original.Action && strEqual(next.CycleID, original.CycleID) &&
			strEqual(next.RunID, original.RunID) && strEqual(next.TaskID, original.TaskID)
		if !same {
			t.Fatalf("payload changed across retries: %+v vs %+v", next, original)
		}
		expected = []int64{120, 600, 1800, 1800}[attempt-2]
	}
	putNotificationTask(t, s, "task-1", "published", "")
	later, err := s.ClaimNotification(notifyDest, now.Add(1800*time.Second))
	must(t, err)
	if later != nil {
		t.Fatal("attempt five leaves the row pending until the next claim expires it")
	}
	health, err := s.NotificationHealth()
	must(t, err)
	if health.Failed != 1 {
		t.Fatalf("a fifth attempt surfaces as delivery_uncertain: %+v", health)
	}
}

func TestDestinationRotationNeverRoutesNewEventsToAnOldWorker(t *testing.T) {
	s := open(t, statePath(t))
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	putNotificationTask(t, s, "old", "blocked", "timeout")
	must(t, s.ConfigureNotifications(str("destination-b"), "enabled", nil))
	putNotificationTask(t, s, "new", "blocked", "timeout")
	if stale, _ := s.ClaimNotification(notifyDest, time.Now().UTC()); stale != nil {
		t.Fatal("old destination still claims rows")
	}
	delivery, err := s.ClaimNotification("destination-b", time.Now().UTC())
	must(t, err)
	if delivery == nil || delivery.TaskID == nil || *delivery.TaskID != "new" {
		t.Fatalf("%+v", delivery)
	}
	must(t, s.ConfigureNotifications(nil, "invalid", str("invalid destination")))
	health, err := s.NotificationHealth()
	must(t, err)
	if health.LastError == nil || *health.LastError != "invalid destination" || !health.Configured {
		t.Fatalf("%+v", health)
	}
}

func TestOverflowCapsPendingAtOneThousand(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	db := raw(t, path)
	conn, err := db.Conn(store.Background())
	must(t, err)
	defer conn.Close()
	ctx := store.Background()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 1005; index++ {
		id := fmt.Sprintf("task-%d", index)
		data := fmt.Sprintf(`{"id":"%s","cycle_id":"c","status":"blocked","blocked_reason":"timeout","config":{"github_repo":"fixture/project"}}`, id)
		if _, err := conn.ExecContext(ctx, "INSERT INTO records VALUES('task',?1,?2)", id, data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if rows := pendingRows(t, path); len(rows) != 1000 {
		t.Fatalf("%d pending rows", len(rows))
	}
	failed := outboxRows(t, path, "failed")
	if len(failed) != 5 {
		t.Fatalf("%d overflow failures", len(failed))
	}
	for _, row := range failed {
		if row["last_error"] != "queue_overflow" {
			t.Fatalf("%+v", row)
		}
	}
	var found0, found4 bool
	for _, row := range failed {
		found0 = found0 || row["task_id"] == "task-0"
		found4 = found4 || row["task_id"] == "task-4"
	}
	if !found0 || !found4 {
		t.Fatal("the oldest queued events overflow first")
	}
}

func TestClaimExpiresDayOldRowsAndPrunesTerminalHistory(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.Put("settings", "config", map[string]any{
		"github_repo": "fixture/project", "retain_events": 3,
	}))
	must(t, s.ConfigureNotifications(str(notifyDest), "enabled", nil))
	putNotificationTask(t, s, "old", "blocked", "timeout")
	db := raw(t, path)
	exec(t, db, "UPDATE notification_outbox SET created_at=strftime('%Y-%m-%dT%H:%M:%fZ','now','-2 days') WHERE task_id='old'")
	if expired, _ := s.ClaimNotification(notifyDest, time.Now().UTC()); expired != nil {
		t.Fatal("day-old row still claims")
	}
	failed := outboxRows(t, path, "failed")
	if len(failed) != 1 || failed[0]["last_error"] != "expired" {
		t.Fatalf("%+v", failed)
	}
	for index := 0; index < 5; index++ {
		putNotificationTask(t, s, fmt.Sprintf("t%d", index), "blocked", "timeout")
		delivery, err := s.ClaimNotification(notifyDest, time.Now().UTC())
		must(t, err)
		if delivery == nil {
			t.Fatal("claim missed a queued row")
		}
		must(t, s.FinishNotificationFailure(delivery.Seq, "transport_error", nil, false))
	}
	if later, _ := s.ClaimNotification(notifyDest, time.Now().UTC()); later != nil {
		t.Fatal("terminal rows claim again")
	}
	terminal := queryInt(t, db, "SELECT count(*) FROM notification_outbox WHERE status!='pending'")
	if terminal != 3 {
		t.Fatalf("terminal history prunes to retain_events: %d", terminal)
	}
}
