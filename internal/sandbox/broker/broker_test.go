// The broker's lifecycle against the fake engine: startup checks, create refusals, kill reports, shutdown and sweep.

package broker

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
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

func TestStartupIsolatedGateway(t *testing.T) {
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

func discard([]byte) error { return nil }

func TestCreateWarnings(t *testing.T) {
	e := newFakeEngine(t)
	warning := "Your kernel does not support swap limit capabilities or the cgroup is not mounted. Memory limited without swap."
	e.create = func(string, *http.Request) (int, []string) { return 0, []string{warning} }
	b := e.broker(t, testConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := b.runSandbox(ctx, plan{kind: wire.KindProbe, probe: wire.ProbeVersions, timeout: time.Minute}, discard, discard, nil)
	if err == nil || !strings.Contains(err.Error(), "swap limit") {
		t.Fatalf("create with a dropped limit = %v; want a refusal naming the warning", err)
	}
	created := e.Created()
	if len(created) != 1 || created[0].Running() || len(e.Remaining()) != 0 {
		t.Fatalf("a sandbox whose limits Docker dropped was started or left behind (remaining %v)", e.Remaining())
	}
}

func probePlan(timeout time.Duration) plan {
	return plan{kind: wire.KindProbe, probe: wire.ProbeVersions, timeout: timeout}
}

func TestKillReport(t *testing.T) {
	run := func(t *testing.T, e *fakeEngine, timeout time.Duration) wire.ExitReport {
		t.Helper()
		// The daemon reports the exit a moment late, so the kill arrives after the sandbox already ended.
		e.waitDelay = 300 * time.Millisecond
		b := e.broker(t, testConfig(t))
		controls := make(chan control)
		type ended struct {
			report wire.ExitReport
			err    error
		}
		done := make(chan ended, 1)
		go func() {
			report, err := b.runSandbox(context.Background(), probePlan(timeout), discard, discard, controls)
			done <- ended{report, err}
		}()
		<-e.WaitCreated(t, 1).Exited()
		var got ended
		select {
		case controls <- control{signal: wire.SignalKill}:
			got = <-done
		case got = <-done:
			// The exit reached the broker before the kill could: the report must be the same.
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.report
	}
	t.Run("exited before the kill", func(t *testing.T) {
		e := newFakeEngine(t)
		e.run = func(c *fakeContainer) { c.End(3) }
		if report := run(t, e, time.Minute); report.Killed || report.Code != 3 || report.Error != "" {
			t.Fatalf("kill after a natural exit = %+v; want the exit's own status", report)
		}
	})
	t.Run("time limit before a kill", func(t *testing.T) {
		e := newFakeEngine(t)
		e.run = func(*fakeContainer) {}
		if report := run(t, e, 100*time.Millisecond); !report.Killed || report.Error != "Sandbox time limit reached" {
			t.Fatalf("kill after the time limit stopped it = %+v; want the time limit named", report)
		}
	})
}

// A wait answer that carries a daemon error reports no exit: the sandbox failed, it did not succeed with status 0.
func TestWaitErrorFailsSandbox(t *testing.T) {
	e := newFakeEngine(t)
	e.run = func(c *fakeContainer) { c.End(0) }
	e.waitError = "daemon lost the container"
	b := e.broker(t, testConfig(t))
	report, err := b.runSandbox(context.Background(), probePlan(time.Minute), discard, discard, nil)
	if err == nil || !strings.Contains(err.Error(), "daemon lost the container") {
		t.Fatalf("wait error = %v, report %+v; want the sandbox to fail with the daemon's message", err, report)
	}
}

func TestServeShutdown(t *testing.T) {
	creating, cancelled := make(chan struct{}), make(chan struct{})
	var removed atomic.Bool
	prefix := "/v" + engineapi.APIVersion + "/containers/"
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+prefix+"create", func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(creating)
		<-r.Context().Done()
		close(cancelled)
	})
	mux.HandleFunc("GET "+prefix+"json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"Id":"leftover","Labels":{"octomus.sandbox.instance":"octomus"}}]`)
	})
	mux.HandleFunc("DELETE "+prefix+"leftover", func(w http.ResponseWriter, _ *http.Request) {
		removed.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v"+engineapi.APIVersion+"/images/{ref}/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Id":"sha256:test"}`)
	})
	b := brokerOn(t, testutil.UnixHTTPServer(t, mux), 1)
	b.info.ImageID = "sha256:test"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx, listener) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		req, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v1/sandboxes",
			bytes.NewBufferString(`{"kind":"probe","mode":"versions"}`))
		req.Header.Set("Upgrade", wire.UpgradeProtocol)
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-creating:
	case <-time.After(2 * time.Second):
		t.Fatal("Docker create did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel preparation")
	}
	<-requestDone
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Docker create was not cancelled")
	}
	if !removed.Load() || len(b.slots) != 0 {
		t.Fatal("shutdown left a sandbox or admission slot behind")
	}
}

func TestSweep(t *testing.T) {
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
