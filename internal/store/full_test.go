package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"modernc.org/sqlite"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func mustSQLiteFull(t *testing.T, err error) {
	t.Helper()
	var sq *sqlite.Error
	if err == nil || !errors.As(err, &sq) || sq.Code()&0xff != 13 {
		t.Fatalf("write error = %v; want SQLITE_FULL", err)
	}
}

func fullQueuedTask(cfg config.Config) model.Task {
	return model.Task{
		ID:      model.ID(),
		CycleID: "cycle",
		Proposal: model.Proposal{
			ID: "a", Title: "Concrete improvement", Problem: "Missing behavior",
			Benefit: "Useful behavior", Scope: "one file", Evidence: []string{"README.md"},
			Category: "features", Target: cfg.DefaultBranch, Tier: "M",
			Dependencies: []string{}, Prompt: "Implement the documented behavior",
			Decision: model.DecisionAccepted, Reason: "Grounded",
			RelevantPaths: []string{}, Reconsiders: []string{},
		},
		Status:          model.StatusQueued,
		Route:           config.NewRoute("fixture", "low"),
		Config:          cfg.Clone(),
		SourceRevision:  "source",
		ComparisonBase:  "source",
		DefaultRevision: "source",
		Branch:          "octomus/work",
		Sessions:        []model.Session{},
		Reviews:         []model.ReviewRound{},
		Verification:    []model.Verification{},
		CreatedAt:       model.Now(),
		UpdatedAt:       model.Now(),
		SupersededBy:    []string{},
		Supersedes:      []string{},
	}
}

func fullOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func execStore(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.conn.ExecContext(background, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func countStore(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	if err := s.conn.QueryRowContext(background, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func freeze(t *testing.T, s *Store) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var busy, log, checkpointed int64
	if err := s.conn.QueryRowContext(background, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	if _, err := s.conn.ExecContext(background, "VACUUM"); err != nil {
		t.Fatalf("VACUUM: %v", err)
	}
	var pages int64
	if err := s.conn.QueryRowContext(background, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.conn.ExecContext(background, fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal(err)
	}
}

func thaw(t *testing.T, s *Store) {
	t.Helper()
	execStore(t, s, "PRAGMA max_page_count=1073741823")
}

func TestDiskFullRecordWriteAcknowledgesNothing(t *testing.T) {
	t.Parallel()
	s := fullOpen(t)
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	if err := s.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	doomed := fullQueuedTask(cfg)
	doomed.Proposal.Prompt = strings.Repeat("x", 1<<16)
	freeze(t, s)
	mustSQLiteFull(t, s.Put("task", doomed.ID, doomed))
	var saved model.Task
	if found, err := s.Get("task", doomed.ID, &saved); err != nil || found {
		t.Fatalf("uncommitted write became readable: found=%t err=%v", found, err)
	}
	if rows := countStore(t, s, "SELECT count(*) FROM records WHERE kind='task' AND id=?1", doomed.ID); rows != 0 {
		t.Fatalf("canonical row committed: %d", rows)
	}
	if rows := countStore(t, s, "SELECT count(*) FROM record_meta WHERE kind='task' AND id=?1", doomed.ID); rows != 0 {
		t.Fatalf("projection row committed: %d", rows)
	}
	thaw(t, s)
	if err := s.Put("task", doomed.ID, doomed); err != nil {
		t.Fatalf("write after thaw: %v", err)
	}
	if found, err := s.Get("task", doomed.ID, &saved); err != nil || !found {
		t.Fatalf("write after thaw not readable: found=%t err=%v", found, err)
	}
}

func TestFailedLedgerWritesRollBackTheWholeTransaction(t *testing.T) {
	t.Parallel()
	s := fullOpen(t)
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	if err := s.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	inventory := model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: []model.PullRequest{}}
	if err := s.Put("settings", "pr_inventory", inventory); err != nil {
		t.Fatal(err)
	}
	queued := fullQueuedTask(cfg)
	if err := s.Put("task", queued.ID, queued); err != nil {
		t.Fatal(err)
	}

	freeze(t, s)
	big := strings.Repeat("r", 1<<16)
	mustSQLiteFull(t, s.ReserveSession(0, NewAdmission("cycle", nil, big, cfg.Roles["discovery"])))
	if sessions := countStore(t, s, "SELECT COALESCE(sum(sessions),0) FROM usage"); sessions != 0 {
		t.Fatalf("rolled-back admission moved the counter: %d", sessions)
	}
	if ledger := countStore(t, s, "SELECT count(*) FROM admissions"); ledger != 0 {
		t.Fatalf("rolled-back admission wrote a ledger row: %d", ledger)
	}
	thaw(t, s)
	if err := s.ReserveSession(0, NewAdmission("cycle", nil, "discovery", cfg.Roles["discovery"])); err != nil {
		t.Fatalf("admission after thaw: %v", err)
	}
	if sessions := countStore(t, s, "SELECT COALESCE(sum(sessions),0) FROM usage"); sessions != 1 {
		t.Fatalf("counter after thaw: %d", sessions)
	}
	if ledger := countStore(t, s, "SELECT count(*) FROM admissions"); ledger != 1 {
		t.Fatalf("ledger after thaw: %d", ledger)
	}

	execStore(t, s, `CREATE TRIGGER fail_reservation AFTER INSERT ON pr_reservations
        BEGIN SELECT RAISE(ABORT,'database or disk is full'); END`)
	admitted, err := s.AdmitNewPrTask(&queued, inventory)
	if err == nil || !strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("reservation write error = %v; want the injected failure", err)
	}
	if admitted {
		t.Fatal("failed admission acknowledged success")
	}
	saved, err := Get[model.Task](s, "task", queued.ID)
	if err != nil || saved == nil || saved.Status != model.StatusQueued {
		t.Fatalf("failed admission changed the canonical task: %+v, %v", saved, err)
	}
	if has, _ := s.HasPrReservation(queued.ID); has {
		t.Fatal("failed admission committed a reservation")
	}
	if events, _ := s.Events(&queued.ID); len(events) != 0 {
		t.Fatalf("failed admission recorded %d events", len(events))
	}
	execStore(t, s, "DROP TRIGGER fail_reservation")
	admitted, err = s.AdmitNewPrTask(&queued, inventory)
	if err != nil || !admitted {
		t.Fatalf("admission after the trigger dropped: admitted=%t err=%v", admitted, err)
	}

	execStore(t, s, `CREATE TRIGGER fail_meta AFTER INSERT ON record_meta
        BEGIN SELECT RAISE(ABORT,'database or disk is full'); END`)
	other := fullQueuedTask(cfg)
	err = s.Put("task", other.ID, other)
	if err == nil || !strings.Contains(err.Error(), "disk is full") {
		t.Fatalf("meta write error = %v; want the injected failure", err)
	}
	if found, _ := s.Get("task", other.ID, &model.Task{}); found {
		t.Fatal("rolled-back record became readable")
	}
	if rows := countStore(t, s, "SELECT count(*) FROM record_meta WHERE id=?1", other.ID); rows != 0 {
		t.Fatalf("rolled-back projection committed: %d", rows)
	}
	execStore(t, s, "DROP TRIGGER fail_meta")
	if err := s.Put("task", other.ID, other); err != nil {
		t.Fatalf("record write after the trigger dropped: %v", err)
	}
}
