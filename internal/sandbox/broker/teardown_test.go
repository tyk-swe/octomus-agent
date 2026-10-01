package broker

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestSweepTriesEverySandboxAndAlwaysClearsLeases(t *testing.T) {
	e := newFakeEngine(t)
	cfg := testConfig(t)
	cfg.LeaseDir = t.TempDir()
	stuck := e.leftover("octomus-octomus-verify-stuck", cfg.Instance)
	e.leftover("octomus-octomus-verify-fine", cfg.Instance)
	e.remove = func(c *fakeContainer) (int, string) {
		if c == stuck {
			return http.StatusInternalServerError, "fixture remove failure"
		}
		return 0, ""
	}
	lease := filepath.Join(cfg.LeaseDir, "0123.json")
	if err := os.WriteFile(lease, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b := e.broker(t, cfg)
	err := b.sweep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fixture remove failure") {
		t.Fatalf("sweep = %v; want the failed removal reported", err)
	}
	if remaining := e.Remaining(); len(remaining) != 1 || remaining[0] != stuck.Name {
		t.Fatalf("containers after sweep = %v; want only the one whose removal failed", remaining)
	}
	if _, err := os.Stat(lease); !os.IsNotExist(err) {
		t.Fatalf("a failed removal kept every egress lease: %v", err)
	}
}

func TestRemovalAlreadyInProgressIsAwaitedAndKeepsAnonymousVolumesOut(t *testing.T) {
	e := newFakeEngine(t)
	c := e.leftover("octomus-octomus-verify-racing", "octomus")
	// Another remover got there first: Docker refuses the second removal while the first one runs.
	e.remove = func(c *fakeContainer) (int, string) {
		go func() {
			time.Sleep(200 * time.Millisecond)
			e.mu.Lock()
			delete(e.containers, c.ID)
			e.mu.Unlock()
		}()
		return http.StatusConflict, "removal of container " + c.ID + " is already in progress"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engineapi.New(e.Socket()).ContainerRemove(ctx, c.ID); err != nil {
		t.Fatalf("removal in progress = %v; want it awaited", err)
	}
	if remaining := e.Remaining(); len(remaining) != 0 {
		t.Fatalf("ContainerRemove returned while %v still existed", remaining)
	}
	// An image VOLUME gets an anonymous volume per container; only v=1 removes it with the container.
	for _, query := range e.Deletes() {
		if !strings.Contains(query, "force=1") || !strings.Contains(query, "v=1") {
			t.Fatalf("removal query %q keeps the container's anonymous volumes", query)
		}
	}
}

func TestRemovalThatFailsElsewhereIsAskedForAgain(t *testing.T) {
	e := newFakeEngine(t)
	c := e.leftover("octomus-octomus-verify-stuck", "octomus")
	// Another removal was running and then failed, as a force-removal whose kill gets no exit event does: Docker
	// clears its in-progress mark and keeps the container, so only asking again removes it.
	var refused atomic.Bool
	e.remove = func(*fakeContainer) (int, string) {
		if refused.CompareAndSwap(false, true) {
			return http.StatusConflict, "removal of container " + c.ID + " is already in progress"
		}
		return 0, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := engineapi.New(e.Socket()).ContainerRemove(ctx, c.ID); err != nil {
		t.Fatalf("removal after another one failed = %v; want the container removed", err)
	}
	if remaining := e.Remaining(); len(remaining) != 0 {
		t.Fatalf("ContainerRemove returned while %v still existed", remaining)
	}
}

func TestUnconfirmedRemovalKeepsItsSlotUntilAReaperConfirmsIt(t *testing.T) {
	tune(t, &teardownBudget, 2*time.Second)
	tune(t, &removeReserve, time.Second)
	tune(t, &reapDelay, 50*time.Millisecond)
	e := newFakeEngine(t)
	var failing atomic.Bool
	failing.Store(true)
	e.remove = func(*fakeContainer) (int, string) {
		if failing.Load() {
			return http.StatusInternalServerError, "could not kill: tried to kill container, but did not receive an exit event"
		}
		return 0, ""
	}
	cfg := testConfig(t)
	cfg.Max = 1
	log := &testutil.SyncBuffer{}
	cfg.Log = log
	b := e.broker(t, cfg)
	child, err := serve(t, b).Start(context.Background(), versionsProbe)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
	if _, err := child.Wait(); err == nil || !strings.Contains(err.Error(), "Removing the sandbox failed") {
		t.Fatalf("sandbox whose removal failed = %v; want the failure reported", err)
	}
	if b.Info().Live != 1 || len(b.slots) != 1 || len(e.Remaining()) != 1 {
		t.Fatalf("an unconfirmed removal gave back its slot (live %d, slots %d)", b.Info().Live, len(b.slots))
	}
	if !strings.Contains(log.String(), "did not receive an exit event") {
		t.Fatalf("broker log = %q; want the removal failure", log.String())
	}
	failing.Store(false)
	waitUntil(t, "the reaper removed the sandbox and gave its slot back", func() bool {
		return len(e.Remaining()) == 0 && b.Info().Live == 0 && len(b.slots) == 0
	})
}
