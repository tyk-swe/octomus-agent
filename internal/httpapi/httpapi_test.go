package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const token = "operator-fixture-token-with-at-least-32-characters"

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	state, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func testApp(t *testing.T, options ...engine.Option) (*engine.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	state := openStore(t, dir)
	return engine.New(state, dir, options...), state
}

func request(t *testing.T, handler http.Handler, method, path string, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func call(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, handler, method, path, body, true)
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("%s: %v", recorder.Body.String(), err)
	}
	return value
}

func TestPrivateAPIEnforcesAuthContentTypeAndConfigurationRules(t *testing.T) {
	app, _ := testApp(t)
	router := Router(app, token, t.TempDir(), "test")
	if response := request(t, router, "GET", "/api/state", "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", response.Code)
	}
	req := httptest.NewRequest("POST", "/api/control/resume", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type: %d", recorder.Code)
	}
	if response := call(t, router, "POST", "/api/control/resume", "{}"); response.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured resume: %d", response.Code)
	}
	if control, err := app.Control(); err != nil || !control.Paused {
		t.Fatal("failed resume must not change control")
	}
	response := call(t, router, "GET", "/api/state", "")
	if response.Code != http.StatusOK {
		t.Fatalf("state: %d", response.Code)
	}
	body := decode(t, response)
	if body["status"] != "paused" || body["configured"] != false {
		t.Fatalf("state: %v", body)
	}
}

func TestEmbeddedDashboardAndOverridesPreserveHTTPBoundaries(t *testing.T) {
	app, _ := testApp(t)
	router := Router(app, token, "", "test")
	for _, check := range []struct {
		uri    string
		status int
		mime   string
	}{
		{"/", http.StatusOK, "text/html"},
		{"/proposals", http.StatusOK, "text/html"},
		{"/favicon.svg", http.StatusOK, "image/svg+xml"},
		{"/_app/missing.js", http.StatusNotFound, ""},
		{"/%2e%2e/go.mod", http.StatusBadRequest, ""},
		{"/api/missing", http.StatusNotFound, "application/json"},
	} {
		response := request(t, router, "GET", check.uri, "", false)
		if response.Code != check.status {
			t.Fatalf("%s: %d want %d", check.uri, response.Code, check.status)
		}
		if response.Header().Get("x-content-type-options") != "nosniff" {
			t.Fatalf("%s missing nosniff", check.uri)
		}
		if check.mime != "" && !strings.HasPrefix(response.Header().Get("Content-Type"), check.mime) {
			t.Fatalf("%s content type %q", check.uri, response.Header().Get("Content-Type"))
		}
	}
	head := request(t, router, "HEAD", "/", "", false)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", head.Code, head.Body.Len())
	}
	override := t.TempDir()
	if err := os.WriteFile(filepath.Join(override, "200.html"), []byte("override dashboard"), 0o644); err != nil {
		t.Fatal(err)
	}
	response := request(t, Router(app, token, override, "test"), "GET", "/", "", false)
	if response.Body.String() != "override dashboard" {
		t.Fatalf("override: %q", response.Body.String())
	}
	for name, assets := range map[string]http.Handler{"embedded": router, "override": Router(app, token, override, "test")} {
		for _, uri := range []string{"/", "/%2e%2e/go.mod"} {
			response := request(t, assets, "POST", uri, "", false)
			if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" || response.Body.Len() != 0 {
				t.Fatalf("%s POST %s: %d allow %q body %q", name, uri, response.Code, response.Header().Get("Allow"), response.Body.String())
			}
		}
		for _, method := range []string{"GET", "HEAD"} {
			response := request(t, assets, method, "/%2e%2e/go.mod", "", false)
			if response.Code != http.StatusBadRequest || response.Header().Get("Allow") != "" || response.Body.Len() != 0 {
				t.Fatalf("%s %s traversal: %d allow %q body %q", name, method, response.Code, response.Header().Get("Allow"), response.Body.String())
			}
		}
	}
	indexed := t.TempDir()
	for name, body := range map[string]string{"200.html": "fallback", "index.html": "root index", "sub/index.html": "sub index"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(indexed, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(indexed, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	indexedRouter := Router(app, token, indexed, "test")
	for _, check := range []struct{ uri, body string }{
		{"/", "root index"},
		{"/sub", "sub index"},
		{"/sub/", "sub index"},
		{"/missing", "fallback"},
	} {
		response := request(t, indexedRouter, "GET", check.uri, "", false)
		if response.Code != http.StatusOK || response.Body.String() != check.body ||
			response.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s: %d %q %q", check.uri, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}
}

func TestControlActionsThroughHTTP(t *testing.T) {
	app, state := testApp(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Repository = repo
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"true"}
	for _, role := range config.Roles() {
		cfg.Roles[role] = config.NewRoute("gpt-6-astra", "medium")
	}
	for _, tier := range config.Tiers() {
		cfg.Tiers[tier] = config.NewRoute("gpt-6-astra", "medium")
	}
	cfg.RepairRoute = config.NewRoute("gpt-6-astra", "medium")
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	router := Router(app, token, "", "test")
	response := call(t, router, "POST", "/api/control/cycle", "{}")
	if response.Code != http.StatusOK {
		t.Fatalf("cycle: %d %s", response.Code, response.Body.String())
	}
	if body := decode(t, response); body["mode"] != "run_once" {
		t.Fatalf("cycle body: %v", body)
	}
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModePaused)
	control.NextCycleAt = time.Now().Add(time.Hour).Unix()
	message := "earlier planning failure"
	control.Error = &message
	if err := state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	response = call(t, router, "POST", "/api/control/resume", "{}")
	if response.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	if body["mode"] != "continuous" || body["next_cycle_at"] != float64(0) || body["error"] != nil || body["planning_capacity"] == nil {
		t.Fatalf("resume body: %v", body)
	}
	resumed, err := app.Control()
	if err != nil || resumed.Mode != model.OperatingModeContinuous || resumed.NextCycleAt != 0 || resumed.Error != nil {
		t.Fatalf("saved resume control: %+v, %v", resumed, err)
	}
	response = call(t, router, "POST", "/api/control/pause", "{}")
	if response.Code != http.StatusOK || decode(t, response)["mode"] != "paused" {
		t.Fatalf("pause: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/control/bogus", "{}"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown control: %d", response.Code)
	}
}

func queuedTask(cfg config.Config) model.Task {
	reason := model.BlockedReasonPublicationUncertain
	return model.Task{
		ID: "task-seed", CycleID: "cycle-seed",
		Proposal: model.Proposal{
			ID: "p", Title: "T", Problem: "P", Benefit: "B", Scope: "S",
			Evidence: []string{}, Category: "features", Target: "main", Tier: "M",
			Dependencies: []string{}, Prompt: "Do it", Decision: model.DecisionAccepted, Reason: "R",
		},
		Status: model.StatusBlocked, BlockedReason: &reason,
		Route:  config.Route{Backend: config.BackendCodex, Model: "m", Effort: "low"},
		Config: cfg.Clone(), SourceRevision: "s", ComparisonBase: "s",
		DefaultRevision: "s", Branch: "octomus/seed", Workspace: "",
		Sessions: []model.Session{}, Reviews: []model.ReviewRound{}, Verification: []model.Verification{},
		OutputCommit: stringPointer("o"), Attempts: 0,
		CreatedAt: model.Now(), UpdatedAt: model.Now(),
	}
}

func stringPointer(s string) *string { return &s }

func TestHTTPBoundaryRejectionsAndSecurityHeaders(t *testing.T) {
	app, state := testApp(t)
	router := Router(app, token, "", "test")
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	revision, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := state.GetRaw("settings", "config")
	if err != nil {
		t.Fatal(err)
	}
	oversized := `{"expected_revision":"` + revision + `","config":{"github_repo":"` + strings.Repeat("x", 300*1024) + `"}}`
	response := call(t, router, "PUT", "/api/config", oversized)
	if response.Code != http.StatusRequestEntityTooLarge || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		!strings.Contains(response.Body.String(), "length limit exceeded") {
		t.Fatalf("oversized save: %d %q %.200q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	if after, _, err := state.GetRaw("settings", "config"); err != nil || string(after) != string(saved) {
		t.Fatalf("oversized save changed the configuration: %v", err)
	}
	if view := decode(t, call(t, router, "GET", "/api/config", "")); view["revision"] != revision {
		t.Fatalf("revision after oversized save: %v", view["revision"])
	}

	type rejection struct{ method, path, body, want string }
	var rejections []rejection
	for _, history := range []string{"/api/tasks", "/api/cycles", "/api/prs", "/api/proposals"} {
		rejections = append(rejections,
			rejection{"GET", history + "?before=x", "", "Invalid query string: "},
			rejection{"GET", history + "?limit=-1", "", "Invalid query string: "})
	}
	rejections = append(rejections, rejection{"POST", "/api/doctor?mode=bogus", "{}", "Invalid query string: invalid value for `mode`"})
	for _, check := range rejections {
		response := call(t, router, check.method, check.path, check.body)
		if response.Code != http.StatusBadRequest || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
			!strings.HasPrefix(response.Body.String(), check.want) {
			t.Fatalf("%s %s: %d %q %q", check.method, check.path, response.Code, response.Header().Get("Content-Type"), response.Body.String())
		}
	}

	for _, check := range []struct {
		name     string
		response *httptest.ResponseRecorder
		status   int
	}{
		{"dashboard", request(t, router, "GET", "/", "", false), http.StatusOK},
		{"asset miss", request(t, router, "GET", "/_app/missing.js", "", false), http.StatusNotFound},
		{"health", request(t, router, "GET", "/healthz", "", false), http.StatusOK},
		{"state", call(t, router, "GET", "/api/state", ""), http.StatusOK},
		{"unauthenticated", request(t, router, "GET", "/api/state", "", false), http.StatusUnauthorized},
		{"body rejection", call(t, router, "PUT", "/api/config", "{bad"), http.StatusBadRequest},
		{"unknown route", call(t, router, "GET", "/api/missing", ""), http.StatusNotFound},
	} {
		h := check.response.Header()
		csp := h.Get("content-security-policy")
		if check.response.Code != check.status ||
			h.Get("x-content-type-options") != "nosniff" ||
			h.Get("x-frame-options") != "DENY" ||
			h.Get("referrer-policy") != "no-referrer" ||
			h.Get("cache-control") != "no-store" ||
			!strings.HasPrefix(csp, "default-src 'self';") ||
			!strings.Contains(csp, "frame-ancestors 'none'") ||
			!strings.Contains(csp, "base-uri 'self'") ||
			!strings.Contains(csp, "form-action 'self'") {
			t.Fatalf("%s: %d headers %v", check.name, check.response.Code, h)
		}
	}
}
