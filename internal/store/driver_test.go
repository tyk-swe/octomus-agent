// The SQLite connection contract: pragmas, transactions, read-only connections and bounded events.

package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestConnectionSettings(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	must(t, s.Snapshot(func(c *sql.Conn) error {
		var mode string
		var synchronous, busy, queryOnly int64
		if err := c.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode); err != nil {
			return err
		}
		if err := c.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&synchronous); err != nil {
			return err
		}
		if err := c.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busy); err != nil {
			return err
		}
		if err := c.QueryRowContext(context.Background(), "PRAGMA query_only").Scan(&queryOnly); err != nil {
			return err
		}
		if mode != "wal" || synchronous != 2 || busy != 5000 || queryOnly != 0 {
			t.Fatalf("journal_mode=%s synchronous=%d busy_timeout=%d query_only=%d", mode, synchronous, busy, queryOnly)
		}
		return nil
	}))
	r, err := store.OpenReadOnly(path, "probe")
	must(t, err)
	defer r.Close()
	var busy int64
	must(t, r.Conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&busy))
	if busy != 5000 {
		t.Fatalf("read-only busy_timeout %d", busy)
	}
}

func TestPanicInsideTransactionRollsBack(t *testing.T) {
	t.Parallel()
	panics := func(snapshot func(func(*sql.Conn) error) error, statement string) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatal("the callback's panic did not propagate")
			}
		}()
		_ = snapshot(func(c *sql.Conn) error {
			if _, err := c.ExecContext(context.Background(), statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
			panic("callback failure")
		})
	}
	path := statePath(t)
	s := open(t, path)
	panics(s.Snapshot, "INSERT INTO records VALUES ('x','inside','1')")
	must(t, s.Put("x", "after", 1))
	startBatch(t, s)

	r, err := store.OpenReadOnly(path, "probe")
	must(t, err)
	defer r.Close()
	panics(r.Snapshot, "SELECT count(*) FROM records")
	must(t, r.Snapshot(func(*sql.Conn) error { return nil }))

	must(t, s.Close())
	s = open(t, path)
	if after, err := store.Get[any](s, "x", "after"); err != nil || after == nil {
		t.Fatalf("write after the panic = %v, %v; want it committed", after, err)
	}
	if inside, err := store.Get[any](s, "x", "inside"); err != nil || inside != nil {
		t.Fatalf("write inside the panicking transaction = %v, %v; want it rolled back", inside, err)
	}
}

func TestReadOnlyConnectionRefusesWrites(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "state dir ?#%25")
	must(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "state.db")
	s := open(t, path)
	must(t, s.Put("x", "a", 1))
	r, err := store.OpenReadOnly(path, "probe")
	must(t, err)
	defer r.Close()
	var seen int64
	must(t, r.Conn.QueryRowContext(context.Background(), "SELECT count(*) FROM records WHERE kind='x'").Scan(&seen))
	if seen != 1 {
		t.Fatalf("read-only handle sees %d records; it opened a different database", seen)
	}
	_, err = r.Conn.ExecContext(context.Background(), "INSERT INTO records VALUES ('x','ro','1')")
	if err == nil || !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("read-only write error = %v", err)
	}
	must(t, s.Put("x", "b", 2))
	if landed, err := store.Get[any](s, "x", "ro"); err != nil || landed != nil {
		t.Fatalf("read-only write landed: %v, %v", landed, err)
	}
	entries, err := os.ReadDir(parent)
	must(t, err)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(dir) {
		t.Fatalf("opening created stray paths: %v", entries)
	}
}

func inventory(prs ...model.PullRequest) model.OpenPRInventory {
	if prs == nil {
		prs = []model.PullRequest{}
	}
	return model.OpenPRInventory{Repository: "fixture/project", ObservedAt: "2026-01-01T00:00:00Z", PRs: prs}
}

func TestPRAdmissionTransaction(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.GitHubRepo = "fixture/project"; c.MaxOpenPRs = 1 })
	must(t, s.Put("settings", "pr_inventory", inventory()))
	first := task()
	must(t, s.Put("task", first.ID, first))
	stale := inventory()
	stale.ObservedAt = "2025-12-31T00:00:00Z"
	admitted, err := s.AdmitNewPRTask(&first, stale)
	must(t, err)
	if admitted || first.Status != model.StatusQueued {
		t.Fatal("admission with a stale inventory succeeded")
	}
	if has, _ := s.HasPRReservation(first.ID); has {
		t.Fatal("refused admission reserved a slot")
	}
	if events, _ := s.Events(&first.ID); len(events) != 0 {
		t.Fatal("refused admission recorded an event")
	}
	admitted, err = s.AdmitNewPRTask(&first, inventory())
	must(t, err)
	if !admitted || first.Status != model.StatusExecuting {
		t.Fatalf("admitted=%v status=%s", admitted, first.Status)
	}
	saved, err := store.Get[model.Task](s, "task", first.ID)
	must(t, err)
	if saved.Status != model.StatusExecuting {
		t.Fatal("admission did not persist the status")
	}
	if has, _ := s.HasPRReservation(first.ID); !has {
		t.Fatal("admission did not reserve a slot")
	}
	events, err := s.Events(&first.ID)
	must(t, err)
	if len(events) != 1 || events[0].Kind != "status" || events[0].Message != "Executing" {
		t.Fatalf("%+v", events)
	}
	reservations, err := s.PRReservations("Fixture/Project")
	must(t, err)
	if len(reservations) != 1 || reservations[0].TaskID != first.ID || reservations[0].Branch != first.Branch {
		t.Fatalf("%+v", reservations)
	}
	second := task()
	must(t, s.Put("task", second.ID, second))
	admitted, err = s.AdmitNewPRTask(&second, inventory())
	must(t, err)
	if admitted || second.Status != model.StatusQueued {
		t.Fatal("admission beyond the open-PR limit succeeded")
	}
	if has, _ := s.HasPRReservation(second.ID); has {
		t.Fatal("refused admission reserved a slot")
	}
	observed, unrepresented, remaining := store.PRUnion(inventory(), reservations, 1)
	if observed != 0 || unrepresented != 1 || remaining != 0 {
		t.Fatalf("%d %d %d", observed, unrepresented, remaining)
	}
	first.Status = model.StatusBlocked
	must(t, s.Put("task", first.ID, first))
	if has, _ := s.HasPRReservation(first.ID); has {
		t.Fatal("blocked task kept its reservation")
	}
	first.Status = model.StatusPublished
	first.OutputCommit = str("out00001")
	must(t, s.Put("task", first.ID, first))
	must(t, s.SeedPRReservation(first))
	published := inventory(model.PullRequest{Number: 7, Branch: first.Branch, State: "open", Owned: true, Head: "out00001", Base: "main"})
	published.ObservedAt = "2026-01-02T00:00:00Z"
	changed, err := s.PersistPRInventory(published, nil)
	must(t, err)
	if !changed {
		t.Fatal("newer inventory was not persisted")
	}
	if has, _ := s.HasPRReservation(first.ID); has {
		t.Fatal("published, represented task kept its reservation")
	}
	older := inventory()
	if changed, err := s.PersistPRInventory(older, nil); err != nil || changed {
		t.Fatalf("older inventory persisted: %v %v", changed, err)
	}
	invalid := inventory()
	invalid.ObservedAt = "yesterday"
	if _, err := s.PersistPRInventory(invalid, nil); err == nil || !strings.Contains(err.Error(), "PR inventory timestamp is invalid") {
		t.Fatalf("%v", err)
	}
}

func TestEventsAreRedactedAndBounded(t *testing.T) {
	t.Parallel()
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
