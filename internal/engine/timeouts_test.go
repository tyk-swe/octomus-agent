package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type timeoutBroker struct {
	sandbox.Host
	mode      sandbox.Mode
	limit     uint64
	err       error
	refreshes atomic.Int32
	cached    atomic.Int32
	starts    atomic.Int32
	bounded   bool
}

func (b *timeoutBroker) Mode() sandbox.Mode {
	if b.mode != 0 {
		return b.mode
	}
	return sandbox.ModeDocker
}

func (b *timeoutBroker) Info(context.Context) (wire.BrokerInfo, error) {
	b.cached.Add(1)
	return wire.BrokerInfo{Limits: wire.BrokerLimits{MaxSeconds: 604800}}, nil
}

func (b *timeoutBroker) RefreshInfo(ctx context.Context) (wire.BrokerInfo, error) {
	b.refreshes.Add(1)
	deadline, hasDeadline := ctx.Deadline()
	b.bounded = hasDeadline && time.Until(deadline) <= 15*time.Second
	if err := ctx.Err(); err != nil {
		return wire.BrokerInfo{}, err
	}
	return wire.BrokerInfo{Limits: wire.BrokerLimits{MaxSeconds: b.limit}}, b.err
}

func (b *timeoutBroker) Start(context.Context, sandbox.Spec) (sandbox.Child, error) {
	b.starts.Add(1)
	return nil, errors.New("Unexpected sandbox start before timeout validation")
}

type unreportedTimeoutBackend struct{ sandbox.Host }

func (unreportedTimeoutBackend) Mode() sandbox.Mode { return sandbox.ModeDocker }

func TestSandboxTimeoutBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		limit, session, command uint64
		wantError               string
	}{
		{"below session cap", 3600, 3599, 1, ""},
		{"at session cap", 3600, 3600, 1, ""},
		{"over session cap", 3600, 3601, 1, "Session timeout"},
		{"command plus grace below cap", 3600, 60, 3539, ""},
		{"command plus grace at cap", 3600, 60, 3540, ""},
		{"command plus grace over cap", 3600, 60, 3541, "verification shutdown grace"},
		{"limit cannot fit grace", 60, 10, 1, "verification shutdown grace"},
		{"smallest command fits", 61, 10, 1, ""},
		{"session overflow", 3600, ^uint64(0), 1, "Session timeout"},
		{"command addition overflow", ^uint64(0), 60, ^uint64(0), "verification shutdown grace"},
		{"largest sum fits", ^uint64(0), 60, ^uint64(0) - sandbox.VerificationGraceSeconds, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.SessionTimeoutSeconds, cfg.CommandTimeoutSeconds = tc.session, tc.command
			// The whole task can span multiple bounded containers.
			cfg.TaskTimeoutSeconds = 604800
			broker := &timeoutBroker{limit: tc.limit}
			app := New(nil, t.TempDir(), WithSandbox(broker))
			t.Cleanup(app.Shutdown)
			err := app.checkSandboxTimeouts(context.Background(), cfg)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("limit=%d, session=%d, command=%d: %v; want %q", tc.limit, tc.session, tc.command, err, tc.wantError)
			}
			if broker.refreshes.Load() != 1 || broker.cached.Load() != 0 || !broker.bounded {
				t.Fatal("timeout validation did not read bounded, fresh broker policy")
			}
		})
	}
}

func TestSandboxTimeoutPolicyUnavailable(t *testing.T) {
	t.Parallel()
	unavailable := errors.New("broker unavailable")
	for _, tc := range []struct {
		name    string
		backend sandbox.Backend
		cause   error
	}{
		{"unavailable", &timeoutBroker{err: unavailable}, unavailable},
		{"zero limit", &timeoutBroker{}, nil},
		{"no policy reporter", unreportedTimeoutBackend{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(nil, t.TempDir(), WithSandbox(tc.backend))
			t.Cleanup(app.Shutdown)
			err := app.checkSandboxTimeouts(context.Background(), config.Default())
			if err == nil || tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("missing policy was accepted or obscured: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app := New(nil, t.TempDir(), WithSandbox(&timeoutBroker{limit: 21600}))
	t.Cleanup(app.Shutdown)
	if err := app.checkSandboxTimeouts(ctx, config.Default()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled policy read = %v", err)
	}
}

func TestRunnerConnectionRechecksSandboxTimeouts(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.SessionTimeoutSeconds, cfg.CommandTimeoutSeconds = 600, 540
	broker := &timeoutBroker{limit: 600}
	route := cfg.Roles["orchestrator"]
	script := runnertest.New(runnertest.CatalogFor(route)...)
	baseConnect := script.Connector()
	var connections atomic.Int32
	connect := func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
		connections.Add(1)
		return baseConnect(ctx, backend, cfg, cwd)
	}
	app := New(nil, t.TempDir(), WithSandbox(broker), WithRunnerConnector(connect))
	t.Cleanup(app.Shutdown)
	clients := app.runners(context.Background(), cfg, "task")
	defer clients.Close()
	if _, err := clients.Start(route, cfg.Repository, nil); err != nil {
		t.Fatalf("initial runner at the broker limit: %v", err)
	}
	if err := clients.Release(); err != nil {
		t.Fatal(err)
	}
	// The catalog stays cached, but the next connection must use fresh broker policy.
	broker.limit = 599
	if _, err := clients.Start(route, cfg.Repository, nil); err == nil || !strings.Contains(err.Error(), "Session timeout") {
		t.Fatalf("lowered broker limit was ignored on the next connection: %v", err)
	}
	if connections.Load() != 1 || broker.refreshes.Load() != 2 || script.OpenClients() != 0 {
		t.Fatalf("unsafe reconnect: connections=%d, policy reads=%d, open clients=%d", connections.Load(), broker.refreshes.Load(), script.OpenClients())
	}
}

func TestSaveConfigChecksLiveSandboxTimeouts(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.SessionTimeoutSeconds, cfg.CommandTimeoutSeconds, cfg.TaskTimeoutSeconds = 300, 100, 7200
	saveSettings(t, state, cfg, model.DefaultControl())
	broker := &timeoutBroker{limit: 600}
	app := New(state, t.TempDir(), WithSandbox(broker))
	t.Cleanup(app.Shutdown)
	original, err := app.Settings()
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range []map[string]json.RawMessage{
		{"session_timeout_seconds": json.RawMessage(`601`)},
		{"command_timeout_seconds": json.RawMessage(`541`)},
	} {
		if _, err := app.SaveConfig(original.Revision, patch); err == nil {
			t.Fatalf("unsafe configuration saved: %v", patch)
		}
		stored, err := app.Config()
		if err != nil || !wirejson.Equal(stored, cfg) {
			t.Fatalf("rejected update changed persisted configuration: %+v, %v", stored, err)
		}
	}
	broker.err = errors.New("broker unavailable")
	if _, err := app.SaveConfig(original.Revision, map[string]json.RawMessage{"command_timeout_seconds": json.RawMessage(`540`)}); !errors.Is(err, broker.err) {
		t.Fatalf("save used cached policy after broker failure: %v", err)
	}
	stored, err := app.Config()
	if err != nil || !wirejson.Equal(stored, cfg) {
		t.Fatal("unavailable broker changed saved configuration")
	}
	broker.err = nil
	updated, err := app.SaveConfig(original.Revision, map[string]json.RawMessage{
		"session_timeout_seconds": json.RawMessage(`600`), "command_timeout_seconds": json.RawMessage(`540`),
	})
	if err != nil || updated == nil {
		t.Fatalf("exact timeout boundaries were rejected: %v", err)
	}
	stored, err = app.Config()
	if err != nil || stored.SessionTimeoutSeconds != 600 || stored.CommandTimeoutSeconds != 540 || updated.Revision == original.Revision {
		t.Fatalf("boundary update was not committed: %+v, %v", stored, err)
	}
	// Broker policy is refreshed again even though the saved settings revision still matches.
	broker.limit = 599
	if _, err := app.SaveConfig(updated.Revision, map[string]json.RawMessage{"max_retries": json.RawMessage(`3`)}); err == nil {
		t.Fatal("broker policy change was missed")
	}
	if broker.cached.Load() != 0 {
		t.Fatal("configuration updates trusted a cached timeout limit")
	}
}

func TestHostModePreservesConfiguredTimeoutRange(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	broker := &timeoutBroker{mode: sandbox.ModeOff, err: errors.New("must not query broker in host mode")}
	app := New(state, t.TempDir(), WithSandbox(broker))
	t.Cleanup(app.Shutdown)
	original, err := app.Settings()
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.SaveConfig(original.Revision, map[string]json.RawMessage{
		"session_timeout_seconds": json.RawMessage(`604800`),
		"command_timeout_seconds": json.RawMessage(`604800`),
		"task_timeout_seconds":    json.RawMessage(`604800`),
	})
	if err != nil {
		t.Fatalf("host timeout range was narrowed by Docker policy: %v", err)
	}
	if broker.refreshes.Load() != 0 || broker.cached.Load() != 0 {
		t.Fatal("host mode contacted broker")
	}
	stored, err := app.Config()
	if err != nil || stored.SessionTimeoutSeconds != 604800 || stored.CommandTimeoutSeconds != 604800 {
		t.Fatalf("host timeout settings were altered: %+v, %v", stored, err)
	}
}

func TestSavedTimeoutsRejectedBeforeWork(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"planning", "audit", "queued task", "retry", "baseline"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			state := testStore(t)
			data := t.TempDir()
			cfg := testConfig(t.TempDir())
			cfg.Repository = filepath.Join(t.TempDir(), "absent-checkout")
			cfg.SessionTimeoutSeconds, cfg.CommandTimeoutSeconds = 601, 1
			saveSettings(t, state, cfg, model.DefaultControl())
			broker := &timeoutBroker{limit: 600}
			var connections atomic.Int32
			connect := func(context.Context, config.Backend, config.Config, string) (runner.Adapter, error) {
				connections.Add(1)
				return nil, errors.New("Unexpected runner before timeout validation")
			}
			app := New(state, data, WithSandbox(broker), WithRunnerConnector(connect))
			t.Cleanup(app.Shutdown)
			task := queuedTask(cfg, model.ID(), cfg.DefaultBranch, cfg.BranchPrefix+"timeout")
			var err error
			switch entry {
			case "planning", "audit":
				err = app.preflight(context.Background(), cfg, entry == "audit")
			case "queued task":
				err = app.execute(context.Background(), &task)
			case "retry":
				err = app.retryPreflight(context.Background(), &task)
			case "baseline":
				_, err = app.executeBaseline(context.Background(), &model.BaselineCheck{ID: model.ID(), Config: cfg})
			}
			if err == nil || !strings.Contains(err.Error(), "Session timeout") {
				t.Fatalf("%s passed timeout policy into later work: %v", entry, err)
			}
			if connections.Load() != 0 || broker.starts.Load() != 0 {
				t.Fatalf("%s started untrusted work before checking timeout policy", entry)
			}
			entries, err := os.ReadDir(data)
			if err != nil || len(entries) != 0 {
				t.Fatalf("%s created workspace state before validation: %v, %v", entry, entries, err)
			}
			if got := admissionsByRole(t, state); len(got) != 0 {
				t.Fatalf("%s spent admissions before timeout validation: %v", entry, got)
			}
		})
	}
}
