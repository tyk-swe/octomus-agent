// Shared runner fixtures, exact route validation and the wire shapes of models and diagnostics.

package runner

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
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// The environment carries secrets the redaction of runner output must scrub.
const (
	cutPhraseEnv = "RUNNER_TEST_PASSWORD"
	cutPhrase    = "correct horse battery staple"
	cutLinesEnv  = "RUNNER_TEST_SECRET"
	cutLines     = "first-line-of-key\nsecond-line-of-key\nthird-line"
)

func TestMain(m *testing.M) {
	for name, value := range map[string]string{cutPhraseEnv: cutPhrase, cutLinesEnv: cutLines} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

func pyString(s string) string { return strconv.Quote(s) }

// wrapper writes the one Python shim that starts a tests/fixtures runner peer as the named binary.
func wrapper(t *testing.T, root, name, fixture string) string {
	t.Helper()
	path := filepath.Join(root, name)
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os, runpy, sys\nos.environ['OCTOMUS_FIXTURE'] = %s\nsys.path.insert(0, %s)\nrunpy.run_path(%s, run_name='__main__')\n",
		pyString(root), pyString(filepath.Dir(testutil.FixturePath(fixture))), pyString(testutil.FixturePath(fixture)))
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
	progress  testutil.SyncBuffer
}

func newFixture(t *testing.T, name string, configure func(shim string) config.Config) *fixture {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := wrapper(t, root, name, name+".py")
	return &fixture{t: t, root: root, workspace: workspace, cfg: configure(shim)}
}

// record keeps every progress line the adapters report, for assertions on what they contain.
func (f *fixture) record(message string) error {
	_, err := f.progress.Write([]byte(message + "\n"))
	return err
}

func (f *fixture) connector() Connector {
	return func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (Adapter, error) {
		return Connect(ctx, backend, cfg, cwd, "fixture", f.record, sandbox.Host{})
	}
}

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
	return connectOpenCode(ctx, f.cfg, f.workspace, "fixture", f.record, sandbox.Host{})
}

func (f *fixture) connectCodex(ctx context.Context) (*Codex, error) {
	return connectCodex(ctx, f.cfg, f.workspace, f.record, sandbox.Host{})
}

func published(path string) (string, bool) {
	data, err := os.ReadFile(path)
	value := strings.TrimSpace(string(data))
	return value, err == nil && value != ""
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
	t.Parallel()
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

func TestModelWireShape(t *testing.T) {
	t.Parallel()
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

func TestDiagnosticsWireShape(t *testing.T) {
	t.Parallel()
	warning := "OpenCode version mismatch"
	for _, tc := range []struct {
		diagnostics Diagnostics
		want        string
	}{
		{Diagnostics{Backend: config.BackendCodex, ProtocolVersion: "0.1.0", Version: "codex-cli 0.1.0"},
			`{"backend":"codex","protocol_version":"0.1.0","version":"codex-cli 0.1.0","warning":null}`},
		{Diagnostics{Backend: config.BackendOpencode, ProtocolVersion: "1.2.0", Version: "1.3.0", Warning: &warning},
			`{"backend":"opencode","protocol_version":"1.2.0","version":"1.3.0","warning":"OpenCode version mismatch"}`},
	} {
		data, err := wirejson.Marshal(tc.diagnostics)
		if err != nil || string(data) != tc.want {
			t.Fatalf("diagnostics = %s, %v; want %s", data, err, tc.want)
		}
	}
}

func TestRunnersMixedBackendCatalogs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "opencode", func(shim string) config.Config {
		cfg := config.Default()
		cfg.OpencodeBinary = shim
		cfg.CodexBinary = wrapper(t, t.TempDir(), "codex", "codex.py")
		cfg.SessionTimeoutSeconds = 10
		cfg.CommandTimeoutSeconds = 2
		return cfg
	})
	clients := New(context.Background(), f.cfg, f.connector())
	defer clients.Close()
	if err := clients.checkRoute(codexRoute(), f.workspace); err != nil {
		t.Fatalf("codex route: %v", err)
	}
	if err := clients.checkRoute(route(), f.workspace); err != nil {
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
