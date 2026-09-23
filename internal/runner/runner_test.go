package runner

// Runner tests use owned processes and deterministic HTTP/SSE peers. A local
// executable shim points each client at its temporary fixture root.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func pyString(s string) string { return strconv.Quote(s) }

// wrapper writes the runpy shim the fixture CLIs are launched through.
func wrapper(t *testing.T, root, name, fixture string) string {
	t.Helper()
	path := filepath.Join(root, name)
	fixtures := filepath.Join(repoRoot(t), "tests", "fixtures")
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os, runpy, sys\nos.environ['OCTOMUS_FIXTURE'] = %s\nsys.path.insert(0, %s)\nrunpy.run_path(%s, run_name='__main__')\n",
		pyString(root), pyString(fixtures), pyString(filepath.Join(fixtures, fixture)))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type fixture struct {
	t         *testing.T
	root      string
	workspace string
	cfg       config.Config
	state     *store.Store
}

func newFixture(t *testing.T, name string, configure func(shim string) config.Config) *fixture {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := wrapper(t, root, name, name+".py")
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return &fixture{t: t, root: root, workspace: workspace, cfg: configure(shim), state: state}
}

// opencodeFixture mirrors Fixture::new: an OpenCode shim with no Codex.
func opencodeFixture(t *testing.T) *fixture {
	return newFixture(t, "opencode", func(shim string) config.Config {
		cfg := config.Default()
		cfg.OpencodeBinary = shim
		cfg.CodexBinary = "/no-codex-installed"
		cfg.SessionTimeoutSeconds = 10
		cfg.CommandTimeoutSeconds = 2
		return cfg
	})
}

// codexFixture mirrors Fixture::codex: a Codex shim with no OpenCode.
func codexFixture(t *testing.T) *fixture {
	return newFixture(t, "codex", func(shim string) config.Config {
		cfg := config.Default()
		cfg.OpencodeBinary = "/no-opencode-installed"
		cfg.CodexBinary = shim
		cfg.SessionTimeoutSeconds = 3
		cfg.CommandTimeoutSeconds = 2
		return cfg
	})
}

func (f *fixture) mode(name, value string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name+"-mode"), []byte(value), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) clearMode(name string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.root, name+"-mode")); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) path(name string) string { return filepath.Join(f.root, name) }

func (f *fixture) exists(name string) bool {
	_, err := os.Stat(f.path(name))
	return err == nil
}

func (f *fixture) connect(ctx context.Context) (*OpenCode, error) {
	return ConnectOpenCode(ctx, f.cfg, f.workspace, f.state, "fixture")
}

func (f *fixture) connectCodex(ctx context.Context) (*Codex, error) {
	return ConnectCodex(ctx, f.cfg, f.workspace, f.state, "fixture")
}

// codexInterrupt waits for the fixture to record a turn/interrupt and returns
// its first log entry.
func (f *fixture) codexInterrupt() map[string]any {
	f.t.Helper()
	log := f.path("codex-interrupts.jsonl")
	if !waitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(log)
		return err == nil && strings.HasSuffix(string(data), "\n")
	}) {
		f.t.Fatal("the fixture never recorded a codex interrupt")
	}
	data, err := os.ReadFile(log)
	if err != nil {
		f.t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &entry); err != nil {
		f.t.Fatal(err)
	}
	return entry
}

func waitUntil(limit time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return condition()
}

func alive(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	return err == nil && !strings.Contains(string(stat), ") Z")
}

func route() config.Route {
	return config.Route{
		Backend:  config.BackendOpencode,
		Model:    "fixture-model",
		Provider: stringPtr("fixture"),
		Variant:  stringPtr("high"),
	}
}

func codexRoute() config.Route { return config.NewRoute("gpt-6-astra", "medium") }

type outcome struct {
	answer string
	err    error
}

// turnIn runs a turn on its own goroutine to exercise concurrent client calls.
func turnIn(client Adapter, session string, route config.Route, cwd, prompt string, schema schemas.Schema) chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		answer, err := client.Turn(session, route, cwd, prompt, schema)
		ch <- outcome{answer, err}
	}()
	return ch
}

func await(ch chan outcome, limit time.Duration) (outcome, error) {
	select {
	case result := <-ch:
		return result, nil
	case <-time.After(limit):
		return outcome{}, fmt.Errorf("turn was not bounded")
	}
}

func TestValidateRoute(t *testing.T) {
	models := []Model{
		{Backend: config.BackendCodex, Model: "gpt-6-astra", DisplayName: "astra", Efforts: []string{"low", "medium", "high"}, Variants: []string{}, Available: true},
		{Backend: config.BackendOpencode, Provider: stringPtr("fixture"), Model: "fixture-model", DisplayName: "fixture", Efforts: []string{}, Variants: []string{"low", "high"}, Available: true},
		{Backend: config.BackendOpencode, Provider: stringPtr("offline"), Model: "fixture-model", DisplayName: "offline", Efforts: []string{}, Variants: []string{"high"}, Available: false, UnavailableReason: stringPtr("Provider is not configured")},
	}
	if err := ValidateRoute(codexRoute(), models); err != nil {
		t.Fatalf("valid codex route: %v", err)
	}
	unsupported := config.NewRoute("gpt-6-astra", "max")
	if err := ValidateRoute(unsupported, models); err == nil || !strings.Contains(err.Error(), "Unsupported reasoning route") {
		t.Fatalf("unsupported effort must fail, got %v", err)
	}
	withProvider := codexRoute()
	withProvider.Provider = stringPtr("provider")
	if err := ValidateRoute(withProvider, models); err == nil {
		t.Fatal("a codex route carrying a provider must fail")
	}
	if err := ValidateRoute(route(), models); err != nil {
		t.Fatalf("valid opencode route: %v", err)
	}
	selected := route()
	selected.Provider = stringPtr("invented")
	if err := ValidateRoute(selected, models); err == nil || !strings.Contains(err.Error(), "unavailable in this runtime") {
		t.Fatalf("unknown provider must fail, got %v", err)
	}
	selected = route()
	selected.Variant = stringPtr("invented")
	if err := ValidateRoute(selected, models); err == nil || !strings.Contains(err.Error(), "Unsupported model variant") {
		t.Fatalf("unknown variant must fail, got %v", err)
	}
	selected.Variant = nil
	if err := ValidateRoute(selected, models); err != nil {
		t.Fatalf("absent variant is always acceptable: %v", err)
	}
	selected = route()
	selected.Provider = stringPtr("offline")
	err := ValidateRoute(selected, models)
	if err == nil || !strings.Contains(err.Error(), "unavailable: Provider is not configured") {
		t.Fatalf("unavailable provider must fail with its reason, got %v", err)
	}
	selected = route()
	selected.Model = "invented-model"
	if err := ValidateRoute(selected, models); err == nil {
		t.Fatal("unknown model must fail")
	}
}

// The wire record always carries the nullable fields as explicit null, the
// shape the dashboard types require.
func TestModelWireShape(t *testing.T) {
	data, err := json.Marshal(Model{
		Backend:     config.BackendCodex,
		Model:       "gpt-6-astra",
		DisplayName: "astra",
		Efforts:     []string{"medium"},
		Variants:    []string{},
		Available:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"provider", "provider_name", "unavailable_reason"} {
		value, present := decoded[key]
		if !present || value != nil {
			t.Fatalf("%s must serialize as explicit null: %s", key, data)
		}
	}
	if decoded["backend"] != "codex" || decoded["model"] != "gpt-6-astra" ||
		decoded["display_name"] != "astra" || decoded["available"] != true {
		t.Fatalf("core fields drifted: %s", data)
	}
}

func TestVersionWarnings(t *testing.T) {
	if warning := CodexVersionWarning("codex-cli 0.153.4"); warning != nil {
		t.Fatalf("exact match must not warn: %q", *warning)
	}
	if warning := CodexVersionWarning("codex-cli 0.153.4\n"); warning != nil {
		t.Fatalf("trailing whitespace must not warn: %q", *warning)
	}
	for _, version := range []string{"codex-cli 0.153.40", "codex-cli 0.153.4-dev", "unknown"} {
		warning := CodexVersionWarning(version)
		if warning == nil || !strings.Contains(*warning, "mismatch") {
			t.Fatalf("%q must warn about a mismatch", version)
		}
	}
	if warning := OpenCodeVersionWarning(OpenCodeProtocolVersion); warning != nil {
		t.Fatalf("exact protocol match must not warn: %q", *warning)
	}
	if warning := OpenCodeVersionWarning("1.18.31"); warning == nil || !strings.Contains(*warning, "mismatch") {
		t.Fatalf("protocol mismatch must warn: %v", warning)
	}
}

// OpenCode-only routing must never start the Codex client, and audits skip
// execution routes.
func TestRunnersLazyBackendsAndAuditFiltering(t *testing.T) {
	f := opencodeFixture(t)
	cfg := f.cfg.Clone()
	for _, role := range []string{"orchestrator", "discovery", "proposal_reviewer"} {
		cfg.Roles[role] = route()
	}
	ctx := context.Background()
	clients := New(ctx, cfg, f.state, "fixture", nil)
	defer clients.Close()
	if err := clients.ValidateRoutes(cfg, f.workspace, true); err != nil {
		t.Fatalf("audit validation: %v", err)
	}
	if err := clients.ValidateRoutes(cfg, f.workspace, false); err == nil {
		t.Fatal("execution validation must fail while routes are unset")
	}
	cfg.Roles["code_reviewer"] = route()
	for tier := range cfg.Tiers {
		cfg.Tiers[tier] = route()
	}
	cfg.RepairRoute = route()
	if err := clients.ValidateRoutes(cfg, f.workspace, false); err != nil {
		t.Fatalf("execution validation: %v", err)
	}
	if _, err := clients.Client(config.BackendCodex, f.workspace); err == nil {
		t.Fatal("the nonexistent Codex binary must fail when first requested")
	}
}

// Mixed-backend catalogs keep their own identities; no route falls through to
// the other backend's catalog.
func TestRunnersMixedBackendCatalogs(t *testing.T) {
	f := newFixture(t, "opencode", func(shim string) config.Config {
		cfg := config.Default()
		cfg.OpencodeBinary = shim
		cfg.CodexBinary = wrapper(t, t.TempDir(), "codex", "codex.py")
		cfg.SessionTimeoutSeconds = 10
		cfg.CommandTimeoutSeconds = 2
		return cfg
	})
	// The Codex shim shares the fixture root so its mode files apply here.
	clients := New(context.Background(), f.cfg, f.state, "fixture", nil)
	defer clients.Close()
	if err := clients.CheckRoute(codexRoute(), f.workspace); err != nil {
		t.Fatalf("codex route: %v", err)
	}
	if err := clients.CheckRoute(route(), f.workspace); err != nil {
		t.Fatalf("opencode route: %v", err)
	}
	codexCatalog := clients.catalogs[config.BackendCodex]
	openCatalog := clients.catalogs[config.BackendOpencode]
	if len(codexCatalog) == 0 || len(openCatalog) == 0 {
		t.Fatalf("both catalogs must be loaded: %d %d", len(codexCatalog), len(openCatalog))
	}
	for _, m := range codexCatalog {
		if m.Backend != config.BackendCodex || m.Provider != nil {
			t.Fatalf("codex catalog identity drifted: %+v", m)
		}
	}
	for _, m := range openCatalog {
		if m.Backend != config.BackendOpencode || m.Provider == nil {
			t.Fatalf("opencode catalog identity drifted: %+v", m)
		}
	}
}

// Close owns every started client: the owned server exits and Close stays
// idempotent.
func TestRunnersCloseOwnsClients(t *testing.T) {
	f := opencodeFixture(t)
	clients := New(context.Background(), f.cfg, f.state, "fixture", nil)
	defer clients.Close()
	if _, err := clients.Client(config.BackendOpencode, f.workspace); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !waitUntil(5*time.Second, func() bool { return f.exists("opencode-pids.jsonl") }) {
		t.Fatal("the fixture never recorded its pid")
	}
	data, err := os.ReadFile(f.path("opencode-pids.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Pid int `json:"pid"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(string(data), "\n", 2)[0]), &record); err != nil {
		t.Fatal(err)
	}
	if err := clients.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !waitUntil(3*time.Second, func() bool { return !alive(record.Pid) }) {
		t.Fatal("Close left the owned server alive")
	}
	if err := clients.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
}

// Start and Turn failures keep BlockedReasonRunnerUnavailable in the chain
// with the concrete cause.
func TestRunnersErrorsKeepBlockedReason(t *testing.T) {
	f := opencodeFixture(t)
	clients := New(context.Background(), f.cfg, f.state, "fixture", nil)
	defer clients.Close()
	_, err := clients.Start(route(), f.workspace, stringPtr("ses_missing"))
	if err == nil {
		t.Fatal("resuming a missing session must fail")
	}
	if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonRunnerUnavailable {
		t.Fatalf("blocked reason lost: %v (%v)", reason, err)
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("concrete cause lost: %v", err)
	}
	if _, err := clients.Turn("ses_missing", route(), f.workspace, "prompt", nil); err == nil {
		t.Fatal("turn on a missing session must fail")
	} else if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonRunnerUnavailable {
		t.Fatalf("turn blocked reason lost: %v (%v)", reason, err)
	}
}
