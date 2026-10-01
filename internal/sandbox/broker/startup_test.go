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
	cfg.DockerSocket = e.socket
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

func TestStartupAlwaysRequiresIsolatedGateways(t *testing.T) {
	e := newFakeEngine(t)
	e.options = map[string]string{}
	env := map[string]string{
		"OCTOMUS_SANDBOX_IMAGE": "octomus-sandbox:test", "OCTOMUS_DATA_DIR": t.TempDir(),
		"OCTOMUS_SANDBOX_DATA_VOLUME": "d", "OCTOMUS_SANDBOX_RUNNER_VOLUME": "r", "OCTOMUS_SANDBOX_TOOLS_VOLUME": "t",
		"OCTOMUS_SANDBOX_RUNNER_NETWORK": "rn", "OCTOMUS_SANDBOX_VERIFY_NETWORK": "vn",
		"OCTOMUS_SANDBOX_REQUIRE_ISOLATED_GATEWAY": "false",
	}
	cfg, err := LoadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	deployed, executable := startupConfig(t, e)
	cfg.DockerSocket, cfg.RunnerDir, cfg.ToolsDir, cfg.UID, cfg.GID = e.socket, deployed.RunnerDir, deployed.ToolsDir, deployed.UID, deployed.GID
	if _, err := New(context.Background(), cfg, executable); err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Fatalf("plain internal networks = %v; want a refusal whatever the environment says", err)
	}
}
