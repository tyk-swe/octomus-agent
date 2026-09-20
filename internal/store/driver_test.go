package store_test

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Acceptance criterion 9: the service connection carries the storage contract
// (WAL, synchronous=FULL, 5s busy timeout) and every method runs on it.
func TestConnectionSettingsMatchTheStorageContract(t *testing.T) {
	s := open(t, statePath(t))
	must(t, s.Snapshot(func(c *sql.Conn) error {
		var mode string
		var synchronous, busy, queryOnly int64
		if err := c.QueryRowContext(store.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
			return err
		}
		if err := c.QueryRowContext(store.Background(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
			return err
		}
		if err := c.QueryRowContext(store.Background(), "PRAGMA busy_timeout").Scan(&busy); err != nil {
			return err
		}
		if err := c.QueryRowContext(store.Background(), "PRAGMA query_only").Scan(&queryOnly); err != nil {
			return err
		}
		if mode != "wal" || synchronous != 2 || busy != 5000 || queryOnly != 0 {
			t.Fatalf("journal_mode=%s synchronous=%d busy_timeout=%d query_only=%d", mode, synchronous, busy, queryOnly)
		}
		return nil
	}))
	r, err := store.OpenReadOnly(s.Path(), "probe")
	must(t, err)
	defer r.Close()
	var busy int64
	must(t, r.Conn.QueryRowContext(store.Background(), "PRAGMA busy_timeout").Scan(&busy))
	if busy != 5000 {
		t.Fatalf("read-only busy_timeout %d", busy)
	}
}

// Acceptance criterion 9: a foreign write lock delays the service write until
// it is released instead of failing immediately; the configured busy timeout
// covers the wait.
func TestBusyTimeoutWaitsForForeignWriters(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	db := raw(t, path)
	conn, err := db.Conn(store.Background())
	must(t, err)
	defer conn.Close()
	if _, err := conn.ExecContext(store.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.Put("x", "a", 1) }()
	time.Sleep(400 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("write finished while the lock was held: %v", err)
	default:
	}
	if _, err := conn.ExecContext(store.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("write never finished after the lock was released")
	}
	if time.Since(started) < 400*time.Millisecond {
		t.Fatal("write did not wait for the lock")
	}
}

// Acceptance criterion 6: a read-only export observes one snapshot even while
// the service commits, and it sees committed data that only exists in the WAL.
func TestReadOnlySnapshotIsConsistentAndIncludesWAL(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.Put("x", "a", 1))
	wal, err := os.Stat(path + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("committed data is not in the WAL: %v", err)
	}
	r, err := store.OpenReadOnly(path, "probe")
	must(t, err)
	defer r.Close()
	count := func(c *sql.Conn) int64 {
		var n int64
		must(t, c.QueryRowContext(store.Background(), "SELECT count(*) FROM records WHERE kind='x'").Scan(&n))
		return n
	}
	must(t, r.Snapshot(func(c *sql.Conn) error {
		if count(c) != 1 {
			t.Fatal("reader does not see the WAL commit")
		}
		must(t, s.Put("x", "b", 2))
		if count(c) != 1 {
			t.Fatal("snapshot observed a concurrent commit")
		}
		return nil
	}))
	must(t, r.Snapshot(func(c *sql.Conn) error {
		if count(c) != 2 {
			t.Fatal("new snapshot misses the later commit")
		}
		return nil
	}))
	if _, err := os.Stat(path + "-journal"); !os.IsNotExist(err) {
		t.Fatal("read-only access created a rollback journal")
	}
}

func usageRows(t *testing.T, path string) (int64, int64) {
	t.Helper()
	db := raw(t, path)
	return queryInt(t, db, "SELECT COALESCE(sum(sessions),0) FROM usage"), queryInt(t, db, "SELECT count(*) FROM admissions")
}

// Acceptance criteria 3 and 4: every failed admission leaves the counter and
// the ledger untouched together.
func TestFailedAdmissionsRollBackCounterAndLedgerTogether(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 1; c.MaxWorkspaceBytes = 100 })
	if err := s.ReserveSession(100, admission("2026-09-09T10:00:00Z")); err == nil || !errors.Is(err, model.BlockedReasonStorageLimit) {
		t.Fatalf("storage limit: %v", err)
	}
	if sessions, admissions := usageRows(t, path); sessions != 0 || admissions != 0 {
		t.Fatalf("storage refusal wrote %d/%d", sessions, admissions)
	}
	first := admission("2026-09-09T10:00:00Z")
	must(t, s.ReserveSession(0, first))
	if err := s.ReserveSession(0, admission("2026-09-09T11:00:00Z")); err == nil || !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("budget: %v", err)
	}
	if err := s.ReserveSession(0, first); err == nil {
		t.Fatal("duplicate admission accepted")
	}
	if sessions, admissions := usageRows(t, path); sessions != 1 || admissions != 1 {
		t.Fatalf("failed admissions moved the counter or ledger: %d/%d", sessions, admissions)
	}
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 0 })
	if err := s.ReserveSession(0, admission("2026-09-09T12:00:00Z")); err == nil || !strings.Contains(err.Error(), "Daily session budget must be positive") {
		t.Fatalf("zero budget: %v", err)
	}
	if err := s.ReserveSession(0, admission("not a time")); err == nil {
		t.Fatal("unparseable admission time accepted")
	}
	if sessions, admissions := usageRows(t, path); sessions != 1 || admissions != 1 {
		t.Fatalf("invalid admissions moved the counter or ledger: %d/%d", sessions, admissions)
	}
}

func inventory(prs ...model.PullRequest) model.OpenPrInventory {
	if prs == nil {
		prs = []model.PullRequest{}
	}
	return model.OpenPrInventory{Repository: "fixture/project", ObservedAt: "2026-01-01T00:00:00Z", PRs: prs}
}

// Acceptance criterion 3: PR admission moves the task, its reservation and its
// status event in one transaction, and refusals write nothing.
func TestPrAdmissionAndReservationsShareOneTransaction(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.GitHubRepo = "fixture/project"; c.MaxOpenPRs = 1 })
	must(t, s.Put("settings", "pr_inventory", inventory()))
	first := task()
	must(t, s.Put("task", first.ID, first))
	stale := inventory()
	stale.ObservedAt = "2025-12-31T00:00:00Z"
	admitted, err := s.AdmitNewPrTask(&first, stale)
	must(t, err)
	if admitted || first.Status != model.StatusQueued {
		t.Fatal("admission with a stale inventory succeeded")
	}
	if has, _ := s.HasPrReservation(first.ID); has {
		t.Fatal("refused admission reserved a slot")
	}
	if events, _ := s.Events(&first.ID); len(events) != 0 {
		t.Fatal("refused admission recorded an event")
	}
	admitted, err = s.AdmitNewPrTask(&first, inventory())
	must(t, err)
	if !admitted || first.Status != model.StatusExecuting {
		t.Fatalf("admitted=%v status=%s", admitted, first.Status)
	}
	saved, err := store.Get[model.Task](s, "task", first.ID)
	must(t, err)
	if saved.Status != model.StatusExecuting {
		t.Fatal("admission did not persist the status")
	}
	if has, _ := s.HasPrReservation(first.ID); !has {
		t.Fatal("admission did not reserve a slot")
	}
	events, err := s.Events(&first.ID)
	must(t, err)
	if len(events) != 1 || events[0].Kind != "status" || events[0].Message != "Executing" {
		t.Fatalf("%+v", events)
	}
	reservations, err := s.PrReservations("Fixture/Project")
	must(t, err)
	if len(reservations) != 1 || reservations[0].TaskID != first.ID || reservations[0].Branch != first.Branch {
		t.Fatalf("%+v", reservations)
	}
	second := task()
	must(t, s.Put("task", second.ID, second))
	admitted, err = s.AdmitNewPrTask(&second, inventory())
	must(t, err)
	if admitted || second.Status != model.StatusQueued {
		t.Fatal("admission beyond the open-PR limit succeeded")
	}
	if has, _ := s.HasPrReservation(second.ID); has {
		t.Fatal("refused admission reserved a slot")
	}
	observed, unrepresented, remaining := store.PrUnion(inventory(), reservations, 1)
	if observed != 0 || unrepresented != 1 || remaining != 0 {
		t.Fatalf("%d %d %d", observed, unrepresented, remaining)
	}
	// A blocked task without output releases its reservation through the trigger.
	first.Status = model.StatusBlocked
	must(t, s.Put("task", first.ID, first))
	if has, _ := s.HasPrReservation(first.ID); has {
		t.Fatal("blocked task kept its reservation")
	}
	// Published work represented in the inventory releases on persist.
	first.Status = model.StatusPublished
	first.OutputCommit = str("out00001")
	must(t, s.Put("task", first.ID, first))
	must(t, s.SeedPrReservation(first))
	published := inventory(model.PullRequest{Number: 7, Branch: first.Branch, State: "open", Owned: true, Head: "out00001", Base: "main"})
	published.ObservedAt = "2026-01-02T00:00:00Z"
	changed, err := s.PersistPrInventory(published, nil)
	must(t, err)
	if !changed {
		t.Fatal("newer inventory was not persisted")
	}
	if has, _ := s.HasPrReservation(first.ID); has {
		t.Fatal("published, represented task kept its reservation")
	}
	older := inventory()
	if changed, err := s.PersistPrInventory(older, nil); err != nil || changed {
		t.Fatalf("older inventory persisted: %v %v", changed, err)
	}
	invalid := inventory()
	invalid.ObservedAt = "yesterday"
	if _, err := s.PersistPrInventory(invalid, nil); err == nil || !strings.Contains(err.Error(), "PR inventory timestamp is invalid") {
		t.Fatalf("%v", err)
	}
}

// Acceptance criterion 3: the notification outbox is filled by triggers inside
// the writer's transaction, so a rolled-back task write enqueues nothing.
func TestOutboxEnqueueSharesTheWriterTransaction(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	must(t, s.ConfigureNotifications(str("hook-1"), "enabled", nil))
	blocked := task()
	blocked.Status = model.StatusBlocked
	reason := model.BlockedReasonVerificationFailed
	blocked.BlockedReason = &reason
	db := raw(t, path)
	conn, err := db.Conn(store.Background())
	must(t, err)
	defer conn.Close()
	ctx := store.Background()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO records VALUES('task',?1,?2)", blocked.ID, canonical(t, blocked)); err != nil {
		t.Fatal(err)
	}
	var pending int64
	must(t, conn.QueryRowContext(ctx, "SELECT count(*) FROM notification_outbox").Scan(&pending))
	if pending != 1 {
		t.Fatalf("%d queued inside the transaction", pending)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	health, err := s.NotificationHealth()
	must(t, err)
	if health.Pending != 0 || health.State != "enabled" || !health.Configured {
		t.Fatalf("%+v", health)
	}
	if tk, _ := store.Get[model.Task](s, "task", blocked.ID); tk != nil {
		t.Fatal("rolled-back task persisted")
	}
	must(t, s.Put("task", blocked.ID, blocked))
	health, err = s.NotificationHealth()
	must(t, err)
	if health.Pending != 1 {
		t.Fatalf("%+v", health)
	}
	// The trigger stamps the first attempt with the database clock, so claims
	// are due from the current wall time onward.
	now := time.Now().UTC().Add(time.Second)
	delivery, err := s.ClaimNotification("hook-1", now)
	must(t, err)
	if delivery == nil || delivery.Attempts != 1 || delivery.Category != "verification_failed" || delivery.Action != "inspect_task" || delivery.TaskID == nil || *delivery.TaskID != blocked.ID {
		t.Fatalf("%+v", delivery)
	}
	if again, _ := s.ClaimNotification("hook-1", now); again != nil {
		t.Fatal("claimed delivery was claimed twice before its retry delay")
	}
	status := uint16(503)
	must(t, s.FinishNotificationFailure(delivery.Seq, "http_status", &status, true))
	retry, err := s.ClaimNotification("hook-1", now.Add(time.Duration(store.NotificationRetryDelays[0]+1)*time.Second))
	must(t, err)
	if retry == nil || retry.Seq != delivery.Seq || retry.Attempts != 2 {
		t.Fatalf("%+v", retry)
	}
	must(t, s.FinishNotificationDelivered(retry.Seq, now.Add(time.Hour)))
	health, err = s.NotificationHealth()
	must(t, err)
	if health.Pending != 0 || health.Failed != 0 || health.LastDeliveredAt == nil || health.LastError != nil {
		t.Fatalf("%+v", health)
	}
	// Changing the destination cancels what is still pending.
	other := task()
	other.Status = model.StatusFailed
	must(t, s.Put("task", other.ID, other))
	must(t, s.ConfigureNotifications(str("hook-2"), "enabled", nil))
	health, err = s.NotificationHealth()
	must(t, err)
	if health.Pending != 0 || health.Failed != 0 || health.LastError == nil || *health.LastError != "destination_changed" {
		t.Fatalf("%+v", health)
	}
	if cancelled := queryInt(t, raw(t, path), "SELECT count(*) FROM notification_outbox WHERE status='cancelled'"); cancelled != 1 {
		t.Fatalf("cancelled rows %d", cancelled)
	}
}

// Events feed the same retention trigger the Rust service installs.
func TestEventsAreRedactedAndBounded(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.RetainEvents = 5 })
	for i := range 8 {
		must(t, s.Event("t", "status", "token ghp_abcdefghijklmnop step "+string(rune('a'+i))))
	}
	events, err := s.Events(str("t"))
	must(t, err)
	if len(events) != 5 {
		t.Fatalf("%d events retained", len(events))
	}
	for _, e := range events {
		if strings.Contains(e.Message, "ghp_") {
			t.Fatal("event kept a token")
		}
	}
	must(t, s.PruneEvents(2))
	if events, _ := s.Events(nil); len(events) != 2 {
		t.Fatalf("%d events after pruning", len(events))
	}
}
