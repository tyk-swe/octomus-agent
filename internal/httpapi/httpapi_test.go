// The authenticated control API and embedded dashboard: auth, HTTP boundaries, security headers and redacted request errors.

package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/store"
	dashboard "github.com/tyk-swe/octomus-agent/web"
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

// savedConfig returns the saved configuration record's exact bytes, so a test can prove a rejected save left it untouched.
func savedConfig(t *testing.T, state *store.Store) string {
	t.Helper()
	raw, err := store.Get(state, store.ConfigKey.Raw())
	if err != nil || raw == nil {
		t.Fatalf("saved config: %v, %v", raw, err)
	}
	return string(*raw)
}

func testFleet(t *testing.T, connect runner.Connector) (*engine.Fleet, *engine.App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	state := openStore(t, dir)
	fleet, err := engine.NewFleet(state, dir, engine.FleetOptions{Connector: connect})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	return fleet, fleet.Primary(), state
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

func requestHeaders(t *testing.T, handler http.Handler, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatalf("%s: %v", recorder.Body.String(), err)
	}
	return value
}

func TestPrivateAPIGuards(t *testing.T) {
	fleet, app, _ := testFleet(t, nil)
	router := Router(fleet, token, t.TempDir(), "test")
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

func TestDashboardAssets(t *testing.T) {
	fleet, _, _ := testFleet(t, nil)
	router := Router(fleet, token, "", "test")
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
	response := request(t, Router(fleet, token, override, "test"), "GET", "/", "", false)
	if response.Body.String() != "override dashboard" {
		t.Fatalf("override: %q", response.Body.String())
	}
	for name, assets := range map[string]http.Handler{"embedded": router, "override": Router(fleet, token, override, "test")} {
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
	indexedRouter := Router(fleet, token, indexed, "test")
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

func TestDashboardAssetDelivery(t *testing.T) {
	fleet, _, _ := testFleet(t, nil)
	router := Router(fleet, token, "", "test")
	// Pick an asset big enough that gzip is always the cheaper encoding; tiny
	// chunks are correctly served uncompressed.
	var asset string
	var assetSize int64
	err := fs.WalkDir(dashboard.Files(), "_app/immutable", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > assetSize {
			asset, assetSize = path, info.Size()
		}
		return nil
	})
	if err != nil || asset == "" {
		t.Fatalf("no immutable .js asset in the embedded dashboard: %v", err)
	}
	raw, err := fs.ReadFile(dashboard.Files(), asset)
	if err != nil {
		t.Fatal(err)
	}
	gunzip := func(t *testing.T, body []byte) []byte {
		t.Helper()
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		out, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("gzip body: %v", err)
		}
		return out
	}
	acceptGzip := map[string]string{"Accept-Encoding": "gzip, deflate"}

	compressed := requestHeaders(t, router, "GET", "/"+asset, acceptGzip)
	if compressed.Code != http.StatusOK {
		t.Fatalf("asset gzip GET: %d", compressed.Code)
	}
	if h := compressed.Header(); h.Get("Content-Encoding") != "gzip" ||
		h.Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		!strings.Contains(h.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("asset gzip GET headers: %v", h)
	}
	if !bytes.Equal(gunzip(t, compressed.Body.Bytes()), raw) {
		t.Fatal("asset gzip GET body does not gunzip to the embedded bytes")
	}
	compressedLength := compressed.Header().Get("Content-Length")
	if compressedLength != strconv.Itoa(compressed.Body.Len()) {
		t.Fatalf("asset gzip GET Content-Length %q for %d body bytes", compressedLength, compressed.Body.Len())
	}

	identity := request(t, router, "GET", "/"+asset, "", false)
	if identity.Code != http.StatusOK || identity.Header().Get("Content-Encoding") != "" ||
		identity.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		!strings.Contains(identity.Header().Get("Vary"), "Accept-Encoding") ||
		!bytes.Equal(identity.Body.Bytes(), raw) {
		t.Fatalf("asset identity GET: %d headers %v", identity.Code, identity.Header())
	}
	if contentType := compressed.Header().Get("Content-Type"); contentType != identity.Header().Get("Content-Type") ||
		(!strings.HasPrefix(contentType, "text/javascript") && !strings.HasPrefix(contentType, "application/javascript")) {
		t.Fatalf("asset content type %q, identity %q", contentType, identity.Header().Get("Content-Type"))
	}

	for _, header := range []string{"gzip;q=0", "gzip;q=0.0"} {
		refused := requestHeaders(t, router, "GET", "/"+asset, map[string]string{"Accept-Encoding": header})
		if refused.Code != http.StatusOK || refused.Header().Get("Content-Encoding") != "" ||
			!bytes.Equal(refused.Body.Bytes(), raw) {
			t.Fatalf("Accept-Encoding %q: %d headers %v", header, refused.Code, refused.Header())
		}
	}

	index, err := fs.ReadFile(dashboard.Files(), indexName)
	if err != nil {
		t.Fatal(err)
	}
	root := requestHeaders(t, router, "GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	if root.Code != http.StatusOK || root.Header().Get("Content-Encoding") != "gzip" ||
		root.Header().Get("Cache-Control") != "no-store" ||
		!bytes.Equal(gunzip(t, root.Body.Bytes()), index) {
		t.Fatalf("/ gzip GET: %d headers %v", root.Code, root.Header())
	}

	missing := requestHeaders(t, router, "GET", "/_app/missing.js", map[string]string{"Accept-Encoding": "gzip"})
	if missing.Code != http.StatusNotFound || missing.Header().Get("Cache-Control") != "no-store" ||
		missing.Header().Get("Content-Encoding") != "" {
		t.Fatalf("missing asset: %d headers %v", missing.Code, missing.Header())
	}

	head := requestHeaders(t, router, "HEAD", "/"+asset, map[string]string{"Accept-Encoding": "gzip"})
	if head.Code != http.StatusOK || head.Body.Len() != 0 ||
		head.Header().Get("Content-Encoding") != "gzip" ||
		head.Header().Get("Content-Length") != compressedLength {
		t.Fatalf("asset HEAD: %d headers %v body %d", head.Code, head.Header(), head.Body.Len())
	}

	override := t.TempDir()
	if err := os.MkdirAll(filepath.Join(override, "_app", "immutable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(override, "_app", "immutable", "app.js"), []byte("override js"), 0o644); err != nil {
		t.Fatal(err)
	}
	overridden := requestHeaders(t, Router(fleet, token, override, "test"), "GET", "/_app/immutable/app.js",
		map[string]string{"Accept-Encoding": "gzip"})
	if overridden.Code != http.StatusOK || overridden.Header().Get("Content-Encoding") != "" ||
		overridden.Header().Get("Cache-Control") != "no-store" ||
		overridden.Body.String() != "override js" {
		t.Fatalf("override asset: %d headers %v body %q", overridden.Code, overridden.Header(), overridden.Body.String())
	}
}

func TestControlActionsThroughHTTP(t *testing.T) {
	fleet, app, state := testFleet(t, nil)
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
	if err := store.Put(state, store.ConfigKey, cfg); err != nil {
		t.Fatal(err)
	}
	router := Router(fleet, token, "", "test")
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
	control.Error = new("earlier planning failure")
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
	reason := model.BlockedPublicationUncertain
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
		OutputCommit: new("o"), Attempts: 0,
		CreatedAt: model.Now(), UpdatedAt: model.Now(),
	}
}

func TestHTTPBoundaries(t *testing.T) {
	fleet, _, state := testFleet(t, nil)
	router := Router(fleet, token, "", "test")
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	if err := store.Put(state, store.ConfigKey, cfg); err != nil {
		t.Fatal(err)
	}
	revision, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	saved := savedConfig(t, state)
	oversized := `{"expected_revision":"` + revision + `","config":{"github_repo":"` + strings.Repeat("x", 300*1024) + `"}}`
	response := call(t, router, "PUT", "/api/config", oversized)
	if response.Code != http.StatusRequestEntityTooLarge || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		!strings.Contains(response.Body.String(), "length limit exceeded") {
		t.Fatalf("oversized save: %d %q %.200q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	if savedConfig(t, state) != saved {
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
	for _, path := range []string{"/api/insights?days=x", "/api/insights?days=14"} {
		rejections = append(rejections, rejection{"GET", path, "", "Invalid query string: days must be 7, 30 or 90"})
	}
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

func TestRequestErrorRedaction(t *testing.T) {
	fleet, _, state := testFleet(t, nil)
	cfg := config.Default()
	if err := store.Put(state, store.ConfigKey, cfg); err != nil {
		t.Fatal(err)
	}
	revision, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	before := savedConfig(t, state)
	router := Router(fleet, token, "", "test")
	const secret = "ghp_requestSecret0123456789"
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"unknown body field", "POST", "/api/baseline-checks", `{"` + secret + `":true}`, http.StatusUnprocessableEntity},
		{"enum value", "POST", "/api/model-catalog", `{"backend":"` + secret + `","binary":"codex"}`, http.StatusUnprocessableEntity},
		{"unknown patch field", "PUT", "/api/config", `{"expected_revision":"` + revision + `","config":{"` + secret + `":true}}`, http.StatusUnprocessableEntity},
		{"history cursor", "GET", "/api/tasks?before=" + secret, "", http.StatusBadRequest},
		{"history limit", "GET", "/api/cycles?limit=" + secret, "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := call(t, router, tc.method, tc.path, tc.body)
			body := response.Body.String()
			if response.Code != tc.status || response.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("response = %d %q %q", response.Code, response.Header().Get("Content-Type"), body)
			}
			if strings.Contains(body, secret) || !strings.Contains(body, "[redacted]") {
				t.Fatalf("request error was not redacted: %q", body)
			}
		})
	}
	if savedConfig(t, state) != before {
		t.Fatalf("rejected requests changed saved settings: %v", err)
	}
	if latest, err := state.LatestBaseline(); err != nil || latest != nil {
		t.Fatalf("rejected requests started a baseline check: %v, %v", latest, err)
	}
}

// TestProposalListRedactsBeforeBound places credentials across the proposal list's 2000-character bound on problem and
// reason, and expects the list to scrub them as completely as the detail does.
func TestProposalListRedactsBeforeBound(t *testing.T) {
	fleet, _, state := testFleet(t, nil)
	router := Router(fleet, token, "", "test")
	const password, tokenHead = "Hunter2Secret99", "Q7rTk2Lm9"
	proposal := queuedTask(config.Default()).Proposal
	// The bound falls just before the URL's "@" and after nine characters of the token.
	proposal.Problem = strings.Repeat("x", 2000-len(" postgres://admin:"+password)) + " postgres://admin:" + password + "@db.internal/app is hard-coded in settings.py"
	proposal.Reason = strings.Repeat("y", 2000-len(" ghp_"+tokenHead)) + " ghp_" + tokenHead + "Zp4Wn8Bv3Cx6Dy1Fe5Gh0Jk2Lm7Np9Qr is committed in ci.yml"
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: "cycle-secret", Number: 1, Status: model.CycleCompleted,
		StartedAt: model.Now(), CompletedAt: new(model.Now()), Proposals: []model.Proposal{proposal},
		Assessments: []any{}, Sessions: []model.Session{},
	}
	if err := store.Put(state, store.Cycles.Key(cycle.ID), cycle); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/proposals/cycle-secret/p", "/api/proposals"} {
		response := call(t, router, "GET", path, "")
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, "xxxxx") || !strings.Contains(body, "yyyyy") {
			t.Fatalf("%s: %d %.200q", path, response.Code, body)
		}
		// The list cuts the scrubbed problem before its last words; the detail keeps them.
		if strings.Contains(body, "settings.py") != (path != "/api/proposals") {
			t.Errorf("%s did not bound the problem as expected", path)
		}
		for _, secret := range []string{password, tokenHead} {
			if at := strings.Index(body, secret); at >= 0 {
				t.Errorf("%s returned credential text: ...%s...", path, body[max(0, at-40):min(len(body), at+len(secret)+20)])
			}
		}
	}
}

func TestInsightsEndpoint(t *testing.T) {
	fleet, _, _ := testFleet(t, nil)
	router := Router(fleet, token, "", "test")
	wantDays := map[string]int{"/api/insights": 30, "/api/insights?days=7": 7, "/api/insights?days=90": 90}
	for path, want := range wantDays {
		response := call(t, router, "GET", path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
		view := decode(t, response)
		if days, _ := view["days"].(float64); int(days) != want {
			t.Fatalf("%s days: %v", path, view["days"])
		}
		if daily, _ := view["daily"].([]any); len(daily) != want {
			t.Fatalf("%s daily rows: %d", path, len(daily))
		}
	}
}

func TestOperatorRequestEndpoints(t *testing.T) {
	fleet, _, state := testFleet(t, nil)
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	if err := store.Put(state, store.ConfigKey, cfg); err != nil {
		t.Fatal(err)
	}
	router := Router(fleet, token, "", "test")
	if response := request(t, router, "GET", "/api/requests", "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", response.Code)
	}
	response := call(t, router, "POST", "/api/requests", `{"title":"Add the feature","description":"Create feature.txt","target":"main"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	created := decode(t, response)
	id, _ := created["id"].(string)
	if id == "" || created["status"] != "pending" || created["target"] != "main" {
		t.Fatalf("created record: %v", created)
	}
	response = call(t, router, "POST", "/api/requests", `{"title":"x","description":"y","target":"main","bogus":true}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field: %d %s", response.Code, response.Body.String())
	}
	response = call(t, router, "GET", "/api/requests", "")
	if response.Code != http.StatusOK {
		t.Fatalf("view: %d", response.Code)
	}
	view := decode(t, response)
	pending, _ := view["pending"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["id"] != id || view["seed_limit"] != float64(10) {
		t.Fatalf("view: %v", view)
	}
	response = call(t, router, "POST", "/api/requests/"+id+"/withdraw", "{}")
	if response.Code != http.StatusOK || decode(t, response)["ok"] != true {
		t.Fatalf("withdraw: %d %s", response.Code, response.Body.String())
	}
	if response := call(t, router, "POST", "/api/requests/"+id+"/bogus", "{}"); response.Code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", response.Code)
	}
	if response := call(t, router, "POST", "/api/requests/"+id+"/withdraw", "{}"); response.Code != http.StatusConflict {
		t.Fatalf("second withdraw: %d %s", response.Code, response.Body.String())
	}
}

func TestMissingRecordsAreJSONNotFound(t *testing.T) {
	fleet, _, _ := testFleet(t, nil)
	router := Router(fleet, token, t.TempDir(), "test")
	for _, path := range []string{
		"/api/tasks/missing",
		"/api/cycles/missing",
		"/api/cycles/missing/evidence",
		"/api/baseline-checks/missing",
		"/api/proposals/c/missing",
	} {
		t.Run(path, func(t *testing.T) {
			response := call(t, router, "GET", path, "")
			if response.Code != http.StatusNotFound {
				t.Fatalf("status: %d %s", response.Code, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("content type: %q", contentType)
			}
			if message, _ := decode(t, response)["error"].(string); message == "" {
				t.Fatalf("error body: %s", response.Body.String())
			}
		})
	}
}

func TestUnknownActionsAreJSONNotFound(t *testing.T) {
	fleet, _, _ := testFleet(t, nil)
	router := Router(fleet, token, t.TempDir(), "test")
	for path, message := range map[string]string{
		"/api/tasks/x/bogus":  "Unknown task action",
		"/api/cycles/x/bogus": "Unknown cycle action",
		"/api/control/bogus":  "Unknown control",
	} {
		t.Run(path, func(t *testing.T) {
			response := call(t, router, "POST", path, "{}")
			if response.Code != http.StatusNotFound {
				t.Fatalf("status: %d %s", response.Code, response.Body.String())
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("content type: %q", contentType)
			}
			if got := decode(t, response)["error"]; got != message {
				t.Fatalf("error: %v; want %q", got, message)
			}
		})
	}
}
