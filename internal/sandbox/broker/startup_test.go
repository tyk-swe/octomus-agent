package broker

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	cfg.DockerSocket, cfg.RunnerDir, cfg.ToolsDir, cfg.UID, cfg.GID = e.Socket(), deployed.RunnerDir, deployed.ToolsDir, deployed.UID, deployed.GID
	if _, err := New(context.Background(), cfg, executable); err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Fatalf("plain internal networks = %v; want a refusal whatever the environment says", err)
	}
}

func TestStartupSweepsLeftoversBeforeAnyPreconditionCanFail(t *testing.T) {
	e := newFakeEngine(t)
	cfg, executable := startupConfig(t, e)
	e.leftover("octomus-octomus-runner-left", cfg.Instance)
	e.leftover("someone-elses", "other")
	lease := filepath.Join(cfg.LeaseDir, "0123.json")
	if err := os.WriteFile(lease, []byte(`{"sandbox":"octomus-octomus-runner-left","kind":"runner"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The image tag was pruned while the previous broker's sandboxes still ran.
	e.mu.Lock()
	delete(e.images, cfg.Image)
	e.mu.Unlock()
	if _, err := New(context.Background(), cfg, executable); err == nil || !strings.Contains(err.Error(), "not available locally") {
		t.Fatalf("missing image = %v", err)
	}
	if remaining := e.Remaining(); len(remaining) != 1 || remaining[0] != "someone-elses" {
		t.Fatalf("containers after a refused start = %v; want only the one this broker does not own", remaining)
	}
	if _, err := os.Stat(lease); !os.IsNotExist(err) {
		t.Fatalf("a leftover egress lease survived a refused start: %v", err)
	}
}

// inProgress is how Docker refuses a removal while another removal of the same container runs.
func inProgress(c *fakeContainer) (int, string) {
	return http.StatusConflict, "removal of container " + c.ID + " is already in progress"
}

// started runs New in the background and waits, at most 10 seconds, for its answer.
func started(t *testing.T, cfg Config, executable string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := New(context.Background(), cfg, executable)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not finish")
		return nil
	}
}

func TestStartupRemovesALeftoverWhoseEarlierRemovalFailed(t *testing.T) {
	e := newFakeEngine(t)
	left := e.leftover("octomus-octomus-runner-left", "octomus")
	// The previous broker's force-removal was still running in the daemon when this one started, and then failed.
	var refused atomic.Bool
	e.remove = func(c *fakeContainer) (int, string) {
		if c == left && refused.CompareAndSwap(false, true) {
			return inProgress(c)
		}
		return 0, ""
	}
	cfg, executable := startupConfig(t, e)
	if err := started(t, cfg, executable); err != nil {
		t.Fatalf("startup with a leftover whose earlier removal failed = %v", err)
	}
	if remaining := e.Remaining(); len(remaining) != 0 {
		t.Fatalf("containers after startup = %v; want the leftover removed", remaining)
	}
}

func TestStartupFailsOnALeftoverItCannotRemove(t *testing.T) {
	tune(t, &startupSweepWait, 300*time.Millisecond)
	e := newFakeEngine(t)
	left := e.leftover("octomus-octomus-runner-left", "octomus")
	// A removal that never finishes in the daemon.
	e.remove = inProgress
	cfg, executable := startupConfig(t, e)
	if err := started(t, cfg, executable); err == nil || !strings.Contains(err.Error(), "Removing leftover sandbox "+left.ID) {
		t.Fatalf("startup with a leftover the daemon keeps removing = %v; want it to fail naming the leftover", err)
	}
}
