package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func testApp(t *testing.T, options ...engine.Option) (*engine.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return engine.New(state, dir, options...), state
}

// baselineFixture uses a real local git
// repository as the configured checkout plus a baseline-valid configuration.
func baselineFixture(t *testing.T) (*engine.App, *store.Store, config.Config) {
	t.Helper()
	app, state := testApp(t)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.name", "Fixture")
	run("config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	cfg := config.Default()
	cfg.Repository = repo
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"true"}
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	return app, state, cfg
}

// githubFixture mirrors tests/fixtures: the repository's git and gh shims sit
// first on PATH so remote reads hit a real local bare remote while identity
// answers as github.com/fixture/project. Verification commands then run for
// real in an owned clone, which keeps a live check deterministic.
func githubFixture(t *testing.T, commands []string) (*engine.App, *store.Store, config.Config) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fixtures := filepath.Join("..", "..", "tests", "fixtures")
	for _, name := range []string{"git", "gh"} {
		data, err := os.ReadFile(filepath.Join(fixtures, name+".py"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(cwd string, args ...string) {
		cmd := exec.Command("/usr/bin/git", args...)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	remote := filepath.Join(root, "remote.git")
	checkout := filepath.Join(root, "checkout")
	run(root, "init", "--bare", remote)
	run(root, "init", "-b", "main", checkout)
	run(checkout, "config", "user.name", "Fixture")
	run(checkout, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(checkout, "add", ".")
	run(checkout, "commit", "-m", "initial")
	run(checkout, "remote", "add", "origin", remote)
	run(checkout, "push", "-u", "origin", "main")
	run(remote, "symbolic-ref", "HEAD", "refs/heads/main")
	t.Setenv("OCTOMUS_FIXTURE", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	data := t.TempDir()
	state, err := store.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	cfg := config.Default()
	cfg.Repository = checkout
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = commands
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	return engine.New(state, data), state, cfg
}

// request performs one call against the router, optionally authenticated.
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

func TestAuthenticationBackoffIsBoundedAndExpires(t *testing.T) {
	failures := &authFailures{}
	now := time.Now()
	for _, expected := range []int64{100, 200, 400, 800, 1000, 1000} {
		if delay := failures.delay(now); delay != time.Duration(expected)*time.Millisecond {
			t.Fatalf("delay %v, want %dms", delay, expected)
		}
	}
	if delay := failures.delay(now.Add(60 * time.Second)); delay != 100*time.Millisecond {
		t.Fatalf("expired backoff %v", delay)
	}
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
	// Both asset sources share one boundary: the method is checked before the
	// path, and an unsafe path is refused before any file is looked up.
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
	// Index pages in an override are HTML under their resolved name, not the
	// extensionless request path: nosniff would otherwise make browsers
	// download them.
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

func TestValidAuthenticationBypassesPendingFailureDelay(t *testing.T) {
	app, _ := testApp(t)
	router := Router(app, token, "", "test")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- request(t, router, "GET", "/api/state", "", false)
	}()
	time.Sleep(20 * time.Millisecond)
	if response := call(t, router, "GET", "/api/state", ""); response.Code != http.StatusOK {
		t.Fatalf("valid token: %d", response.Code)
	}
	select {
	case bad := <-done:
		if bad.Code != http.StatusUnauthorized {
			t.Fatalf("bad token: %d", bad.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unauthenticated request never answered")
	}
}

// TestControlActionsThroughHTTP covers the control surface at the
// wire layer: pause and run-once batch work under the default paused control
// while a malformed action and a content-type miss keep their statuses.
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

// An unrecognized cycle action reports 404, not the 409 that belongs to
// discarding a cycle nobody archived yet.
func TestUnknownCycleActionsAreNotReportedAsArchiveConflicts(t *testing.T) {
	app, state := testApp(t)
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: "cycle-1", Number: 1, Status: "completed",
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{},
		Sessions: []model.Session{}, Repository: "fixture/project",
	}
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	router := Router(app, token, "", "test")
	response := call(t, router, "POST", "/api/cycles/cycle-1/bogus", "{}")
	if response.Code != http.StatusNotFound || decode(t, response)["error"] != "Unknown cycle action" {
		t.Fatalf("bogus: %d %s", response.Code, response.Body.String())
	}
	response = call(t, router, "POST", "/api/cycles/cycle-1/discard", "{}")
	if response.Code != http.StatusConflict || decode(t, response)["error"] != "Archive the cycle before discarding its workspace" {
		t.Fatalf("discard: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/cycles/cycle-1/archive", "{}"); response.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", response.Code, response.Body.String())
	}
}

func TestBaselineAPIAuthenticationRoutesAndMissingRecords(t *testing.T) {
	app, _, _ := baselineFixture(t)
	router := Router(app, token, "", "test")
	if response := request(t, router, "GET", "/api/baseline-checks/latest", "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", response.Code)
	}
	response := call(t, router, "GET", "/api/baseline-checks/latest", "")
	if response.Code != http.StatusOK {
		t.Fatalf("latest: %d", response.Code)
	}
	view := decode(t, response)
	if check, ok := view["check"]; check != nil && ok {
		t.Fatalf("check: %v", check)
	}
	if view["eligible"] != true {
		t.Fatalf("eligible: %v", view)
	}
	if response := call(t, router, "GET", "/api/baseline-checks/no-such-check", ""); response.Code != http.StatusNotFound {
		t.Fatalf("missing detail: %d", response.Code)
	}
	if response := call(t, router, "POST", "/api/baseline-checks/no-such-check/cancel", "{}"); response.Code != http.StatusConflict {
		t.Fatalf("missing cancel: %d", response.Code)
	}
	if response := call(t, router, "POST", "/api/baseline-checks/latest", "{}"); response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET" {
		t.Fatalf("wrong method: %d allow %q", response.Code, response.Header().Get("Allow"))
	}
}

// A 405 names every method the matched path answers, once each and in route
// order, after authentication and the content-type rule have run.
func TestMethodNotAllowedNamesThePathMethods(t *testing.T) {
	app, _ := testApp(t)
	router := Router(app, token, "", "test")
	for _, check := range []struct{ method, path, allow string }{
		{"DELETE", "/api/state", "GET"},
		{"DELETE", "/api/config", "GET, PUT"},
		{"PATCH", "/api/cycles/cycle-1/evidence", "GET, POST"},
		{"GET", "/api/cycles/cycle-1/archive", "POST"},
		{"DELETE", "/api/baseline-checks/latest", "GET"},
		{"GET", "/api/doctor", "POST"},
	} {
		response := call(t, router, check.method, check.path, "{}")
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != check.allow || response.Body.Len() != 0 {
			t.Fatalf("%s %s: %d allow %q body %q", check.method, check.path, response.Code, response.Header().Get("Allow"), response.Body.String())
		}
	}
	// Unauthenticated requests learn nothing about the path's methods.
	response := request(t, router, "DELETE", "/api/config", "{}", false)
	if response.Code != http.StatusUnauthorized || response.Header().Get("Allow") != "" {
		t.Fatalf("unauthenticated: %d allow %q", response.Code, response.Header().Get("Allow"))
	}
}

func TestBaselineStartConflictsAndGateBlocksCoverTheLiveSlot(t *testing.T) {
	// The sleeping command keeps the worker alive through every gate check.
	app, state, cfg := githubFixture(t, []string{"sleep 60"})
	router := Router(app, token, "", "test")
	startBody := func(c config.Config) string {
		revision, err := c.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(map[string]any{"expected_revision": revision})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// The legacy whole-config payload is an unknown field now.
	if data, err := json.Marshal(map[string]any{"expected_config": cfg}); err != nil {
		t.Fatal(err)
	} else if response := call(t, router, "POST", "/api/baseline-checks", string(data)); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("legacy expected_config: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/baseline-checks", "{}"); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "expected_revision") {
		t.Fatalf("missing revision: %d %s", response.Code, response.Body.String())
	}
	invalid := cfg.Clone()
	invalid.Repository = "relative"
	if err := state.Put("settings", "config", invalid); err != nil {
		t.Fatal(err)
	}
	if response := call(t, router, "POST", "/api/baseline-checks", startBody(invalid)); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid config: %d %s", response.Code, response.Body.String())
	}
	response := call(t, router, "GET", "/api/baseline-checks/latest", "")
	view := decode(t, response)
	if view["eligible"] != false || view["reason"] == nil {
		t.Fatalf("ineligible view: %v", view)
	}
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	stale := cfg.Clone()
	stale.VerificationCommands = []string{"false"}
	if response := call(t, router, "POST", "/api/baseline-checks", startBody(stale)); response.Code != http.StatusConflict {
		t.Fatalf("stale expected: %d", response.Code)
	}
	if latest, err := state.LatestBaseline(); err != nil || latest != nil {
		t.Fatal("rejected start persisted a check")
	}
	response = call(t, router, "POST", "/api/baseline-checks", startBody(cfg))
	if response.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", response.Code, response.Body.String())
	}
	check := decode(t, response)
	if check["status"] != "running" {
		t.Fatalf("check: %v", check)
	}
	revision, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if check["config_fingerprint"] != revision {
		t.Fatalf("check config fingerprint: %v", check["config_fingerprint"])
	}
	id := check["id"].(string)
	if response := call(t, router, "POST", "/api/baseline-checks", startBody(cfg)); response.Code != http.StatusConflict {
		t.Fatalf("duplicate start: %d", response.Code)
	}
	for _, path := range []string{"/api/control/cycle", "/api/control/resume", "/api/control/audit"} {
		if response := call(t, router, "POST", path, "{}"); response.Code != http.StatusConflict {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}
	configBody, err := json.Marshal(map[string]any{"expected_revision": revision, "config": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if response := call(t, router, "PUT", "/api/config", string(configBody)); response.Code != http.StatusConflict {
		t.Fatalf("config save during baseline: %d", response.Code)
	}
	task := queuedTask(cfg)
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	if response := call(t, router, "POST", "/api/tasks/"+task.ID+"/reconcile", "{}"); response.Code != http.StatusConflict {
		t.Fatalf("reconcile during baseline: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/control/pause", "{}"); response.Code != http.StatusOK {
		t.Fatalf("pause during baseline: %d", response.Code)
	}
	response = call(t, router, "GET", "/api/state", "")
	stateBody := decode(t, response)
	if stateBody["baseline_active"] != true {
		t.Fatalf("baseline_active: %v", stateBody)
	}
	baseline, _ := stateBody["baseline"].(map[string]any)
	if baseline["id"] != id || baseline["config_matches"] != true || baseline["config_revision"] != revision {
		t.Fatalf("baseline summary: %v", baseline)
	}
	if _, ok := baseline["commands"]; ok {
		t.Fatal("state summary must not include commands")
	}
	// Cancelling interrupts the sleeping command, persists the terminal record
	// and removes the owned clone.
	if response := call(t, router, "POST", "/api/baseline-checks/"+id+"/cancel", "{}"); response.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", response.Code, response.Body.String())
	}
	finished := waitBaseline(t, state, id)
	if finished.Status != model.BaselineStatusCancelled {
		t.Fatalf("status %s", finished.Status)
	}
	if finished.CompletedAt == nil || finished.Error == nil || !finished.WorkspaceRemoved {
		t.Fatalf("finished: %+v", finished)
	}
	// Cancellation may land during remote setup (no command evidence yet) or
	// inside the sleeping command (one failed record); both orderings are
	// correct. The mid-command case is covered deterministically by e2e.
	if len(finished.Commands) > 1 || (len(finished.Commands) == 1 && finished.Commands[0].Success) {
		t.Fatalf("cancelled command evidence: %+v", finished.Commands)
	}
	response = call(t, router, "GET", "/api/baseline-checks/latest", "")
	view = decode(t, response)
	if got, _ := view["check"].(map[string]any); got["id"] != id {
		t.Fatalf("latest view: %v", view["check"])
	}
	if view["eligible"] != true {
		t.Fatalf("eligible after finish: %v", view)
	}
	if sessions, err := state.SessionsToday(); err != nil || sessions != 0 {
		t.Fatalf("sessions: %d %v", sessions, err)
	}
	if running, err := state.RunningCycles(); err != nil || len(running) != 0 {
		t.Fatalf("cycles: %d", len(running))
	}
	page, err := state.HistoryPage("task", store.HistoryQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("tasks: %d", len(page.Items))
	}
	if response := call(t, router, "POST", "/api/baseline-checks/"+id+"/cancel", "{}"); response.Code != http.StatusConflict {
		t.Fatalf("cancel finished: %d", response.Code)
	}
}

func waitBaseline(t *testing.T, state *store.Store, id string) *model.BaselineCheck {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		check, err := store.Get[model.BaselineCheck](state, "baseline", id)
		// The terminal record lands before owned-workspace cleanup completes;
		// wait for both so the returned check is the fully settled record.
		if err == nil && check != nil && check.Status != model.BaselineStatusRunning &&
			(check.WorkspaceRemoved || check.CleanupError != nil) {
			return check
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("baseline check did not finish")
	return nil
}

// queuedTask seeds a blocked task: publication
// uncertain, so reconcile is a live action.
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

// Every JSON body route answers extraction failures once, as text: an
// oversized body at 413, malformed JSON at 400 and a body of the wrong shape
// at 422, before any handler work runs.
func TestBodyRejectionsKeepTheirPlainTextForm(t *testing.T) {
	app, state := testApp(t)
	router := Router(app, token, "", "test")
	oversized := `{"expected_revision":"` + strings.Repeat("a", bodyLimit) + `"}`
	for _, route := range []struct{ method, path string }{
		{"PUT", "/api/config"},
		{"POST", "/api/baseline-checks"},
		{"POST", "/api/model-catalog"},
	} {
		for _, check := range []struct {
			body, prefix string
			status       int
		}{
			{oversized, "Failed to buffer the request body: length limit exceeded", http.StatusRequestEntityTooLarge},
			{"{bad", "Failed to parse the request body as JSON: ", http.StatusBadRequest},
			{`{"bogus":1}`, `Failed to deserialize the JSON body into the target type: unknown field "bogus"`, http.StatusUnprocessableEntity},
		} {
			response := call(t, router, route.method, route.path, check.body)
			text := response.Body.String()
			if response.Code != check.status || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
				!strings.HasPrefix(text, check.prefix) || strings.Count(text, "Failed to") != 1 {
				t.Fatalf("%s %s %d: %d %q %q", route.method, route.path, check.status, response.Code, response.Header().Get("Content-Type"), text)
			}
		}
	}
	if raw, found, err := state.GetRaw("settings", "config"); err != nil || found {
		t.Fatalf("rejected saves wrote a configuration: %s %v", raw, err)
	}
	if latest, err := state.LatestBaseline(); err != nil || latest != nil {
		t.Fatalf("rejected starts persisted a check: %v %v", latest, err)
	}
}

// Responses keep integers exact through redaction, and a value that cannot be
// encoded becomes a JSON 500 rather than a partial or empty body.
func TestWriteJSONKeepsExactNumbersAndReportsEncodeFailures(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeJSON(recorder, http.StatusCreated, map[string]any{"count": uint64(1<<63 + 1), "note": "ok"})
	if recorder.Code != http.StatusCreated || recorder.Header().Get("Content-Type") != "application/json" ||
		recorder.Body.String() != `{"count":9223372036854775809,"note":"ok"}` {
		t.Fatalf("encoded: %d %q %s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	writeJSON(recorder, http.StatusOK, map[string]any{"ratio": math.Inf(1)})
	if recorder.Code != http.StatusInternalServerError || recorder.Header().Get("Content-Type") != "application/json" ||
		recorder.Body.String() != `{"error":"The response could not be encoded"}` {
		t.Fatalf("unencodable: %d %q %s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
}

// The request boundary holds on every path: an oversized body is refused before
// it can replace a saved configuration, malformed query values are plain-text
// 400s, and every response, including rejections, carries the security headers.
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
