package broker

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
