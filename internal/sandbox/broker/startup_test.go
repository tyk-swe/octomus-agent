package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// startupConfig is a deployment whose directories exist, against the fake engine.
func startupConfig(t *testing.T, e *fakeEngine) (Config, string) {
	t.Helper()
	cfg := testConfig(t)
	cfg.DockerSocket = e.Socket()
	cfg.RunnerDir, cfg.ToolsDir, cfg.LeaseDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.EgressProxy = "egress:3128"
	executable := filepath.Join(t.TempDir(), "octomus-agent")
	if err := os.WriteFile(executable, []byte("helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, executable
}

func TestStartupRefusesEnginesWithoutIsolatedGateways(t *testing.T) {
	e := newFakeEngine(t)
	cfg, executable := startupConfig(t, e)
	if _, err := New(context.Background(), cfg, executable); err != nil {
		t.Fatalf("a supported deployment was refused: %v", err)
	}
	// Engine 26 and 27 store gateway_mode_ipv4=isolated as an unknown option without enforcing it.
	e.mu.Lock()
	e.api = "1.47"
	e.mu.Unlock()
	if _, err := New(context.Background(), cfg, executable); err == nil || !strings.Contains(err.Error(), "Docker Engine 28") {
		t.Fatalf("API 1.47 with the isolated option = %v; want a refusal naming Docker Engine 28", err)
	}
}
