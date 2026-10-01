package broker

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
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
	b.leases = &leases{dir: cfg.LeaseDir}
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
	if err := engineapi.New(e.socket).ContainerRemove(ctx, c.ID); err != nil {
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
