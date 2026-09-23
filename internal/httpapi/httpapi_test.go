package httpapi

import (
	"encoding/json"
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

func testApp(t *testing.T) (*engine.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return engine.New(state, dir), state
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

// TestControlPauseResumeCycleThroughHTTP covers the control surface at the
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
	response = call(t, router, "POST", "/api/control/resume", "{}")
	if response.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", response.Code, response.Body.String())
	}
	body := decode(t, response)
	if body["mode"] != "continuous" || body["planning_capacity"] == nil {
		t.Fatalf("resume body: %v", body)
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
	if response := call(t, router, "POST", "/api/baseline-checks/latest", "{}"); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method: %d", response.Code)
	}
}

func TestBaselineStartConflictsAndGateBlocksCoverTheLiveSlot(t *testing.T) {
	// The sleeping command keeps the worker alive through every gate check.
	app, state, cfg := githubFixture(t, []string{"sleep 60"})
	router := Router(app, token, "", "test")
	startBody := func(c config.Config) string {
		data, err := json.Marshal(map[string]any{"expected_config": c})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
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
	id := check["id"].(string)
	if response := call(t, router, "POST", "/api/baseline-checks", startBody(cfg)); response.Code != http.StatusConflict {
		t.Fatalf("duplicate start: %d", response.Code)
	}
	for _, path := range []string{"/api/control/cycle", "/api/control/resume", "/api/control/audit"} {
		if response := call(t, router, "POST", path, "{}"); response.Code != http.StatusConflict {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}
	configBody, err := json.Marshal(cfg)
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
	if baseline["id"] != id || baseline["config_matches"] != true {
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
