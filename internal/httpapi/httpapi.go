package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/export"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

const bodyLimit = 256 * 1024

type api struct {
	app       *engine.App
	tokenHash [32]byte
	failures  *authFailures
	assets    http.Handler
	mux       *http.ServeMux
	methods   map[string][]string
}

type authFailures struct {
	mu    sync.Mutex
	count uint
	last  time.Time
}

func (f *authFailures) delay(now time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last.IsZero() || now.Sub(f.last) >= 60*time.Second {
		f.count = 0
	}
	shift := f.count
	if shift > 4 {
		shift = 4
	}
	delay := time.Duration(100<<shift) * time.Millisecond
	if delay > time.Second {
		delay = time.Second
	}
	f.count++
	f.last = now
	return delay
}

type handlerFunc func(w http.ResponseWriter, r *http.Request) (int, any, error)

func Router(app *engine.App, token, assetsOverride, version string) http.Handler {
	s := &api{
		app:       app,
		tokenHash: sha256.Sum256([]byte(token)),
		failures:  &authFailures{},
		assets:    assetHandler(assetsOverride),
		mux:       http.NewServeMux(),
		methods:   map[string][]string{},
	}
	s.route("GET", "/api/state", s.stateView)
	s.route("GET", "/api/tasks", s.history("task"))
	s.route("GET", "/api/cycles", s.history("cycle"))
	s.route("GET", "/api/cycles/{id}", s.cycleDetail)
	s.route("GET", "/api/cycles/{id}/evidence", s.cycleEvidence)
	s.route("POST", "/api/cycles/{id}/{action}", s.cycleAction)
	s.route("GET", "/api/proposals", s.proposalHistory)
	s.route("GET", "/api/proposals/{cycle}/{id}", s.proposalDetail)
	s.route("GET", "/api/prs", s.history("pr"))
	s.route("GET", "/api/tasks/{id}", s.taskDetail)
	s.route("POST", "/api/tasks/{id}/{action}", s.taskAction)
	s.route("GET", "/api/config", s.getConfig)
	s.route("PUT", "/api/config", s.saveConfig)
	s.route("POST", "/api/baseline-checks", s.baselineStart)
	s.route("GET", "/api/baseline-checks/latest", s.baselineLatest)
	s.route("GET", "/api/baseline-checks/{id}", s.baselineDetail)
	s.route("POST", "/api/baseline-checks/{id}/cancel", s.baselineCancel)
	s.route("POST", "/api/control/{action}", s.controlAction)
	s.route("POST", "/api/doctor", s.doctor)
	s.route("POST", "/api/sandbox/self-test", s.sandboxSelfTest)
	s.route("POST", "/api/model-catalog", s.modelCatalog)
	s.route("GET", "/api/events", s.events)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		if r.URL.Path == "/healthz" {
			writeRawJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
			return
		}
		if path, ok := strings.CutPrefix(r.URL.Path, "/api"); ok && (path == "" || strings.HasPrefix(path, "/")) {
			s.serveAPI(w, r)
			return
		}
		s.assets.ServeHTTP(w, r)
	})
}

func setHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("x-content-type-options", "nosniff")
	h.Set("x-frame-options", "DENY")
	h.Set("referrer-policy", "no-referrer")
	h.Set("cache-control", "no-store")
	h.Set("content-security-policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
}

func (a *api) route(method, pattern string, handle handlerFunc) {
	a.methods[pattern] = append(a.methods[pattern], method)
	a.mux.HandleFunc(method+" "+pattern, func(w http.ResponseWriter, r *http.Request) {
		status, body, err := handle(w, r)
		if err != nil {
			var be *bodyError
			if errors.As(err, &be) {
				writeBodyError(w, be)
			} else {
				writeAPIError(w, apiStatus(err), redact.Error(err))
			}
			return
		}
		writeJSON(w, status, body)
	})
}

func patternPath(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

var probeMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

func (a *api) pathMethods(r *http.Request) (path string, allowed []string) {
	seen := map[string]struct{}{}
	for _, method := range probeMethods {
		probe := *r
		probe.Method = method
		_, pattern := a.mux.Handler(&probe)
		if pattern == "" {
			continue
		}
		candidate := patternPath(pattern)
		if path == "" {
			path = candidate
		}
		if candidate != path {
			continue
		}
		for _, registered := range a.methods[candidate] {
			if _, dup := seen[registered]; dup {
				continue
			}
			seen[registered] = struct{}{}
			allowed = append(allowed, registered)
		}
	}
	return path, allowed
}

func (a *api) serveAPI(w http.ResponseWriter, r *http.Request) {
	path, allowed := a.pathMethods(r)
	if path == "" {
		writeAPIError(w, http.StatusNotFound, "Unknown API route")
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		contentTypes := r.Header.Values("Content-Type")
		if len(contentTypes) != 1 {
			writeAPIError(w, http.StatusUnsupportedMediaType, "Use application/json")
			return
		}
		mediaType, _, err := mime.ParseMediaType(contentTypes[0])
		if err != nil || mediaType != "application/json" {
			writeAPIError(w, http.StatusUnsupportedMediaType, "Use application/json")
			return
		}
	}
	matched := false
	for _, method := range allowed {
		if method == r.Method {
			matched = true
			break
		}
	}
	if !matched {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	a.mux.ServeHTTP(w, r)
}

func (a *api) authenticate(w http.ResponseWriter, r *http.Request) bool {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	digest := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(digest[:], a.tokenHash[:]) != 1 {
		time.Sleep(a.failures.delay(time.Now()))
		writeAPIError(w, http.StatusUnauthorized, "Enter the operator access token to connect.")
		return false
	}
	return true
}

func apiStatus(err error) int {
	var jc *wirejson.Error
	var sq *sqlite.Error
	switch {
	case errors.As(err, &sq) || errors.As(err, &jc):
		return http.StatusInternalServerError
	case errors.As(err, new(engine.NotFound)):
		return http.StatusNotFound
	case engine.IsActionConflict(err):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	_, settingsView := value.(*engine.SettingsView)
	generic, err := wirejson.Generic(value)
	var out []byte
	if err == nil {
		if object, ok := generic.(map[string]any); ok && settingsView {
			transforms, hasTransforms := object["transformed_fields"]
			delete(object, "transformed_fields")
			generic = redact.JSON(object)
			if redacted, ok := generic.(map[string]any); ok && hasTransforms {
				redacted["transformed_fields"] = transforms
			}
		} else {
			generic = redact.JSON(generic)
		}
		out, err = wirejson.Marshal(generic)
	}
	if err != nil {
		writeRawJSON(w, http.StatusInternalServerError, map[string]any{"error": "The response could not be encoded"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func writeRawJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return &bodyError{http.StatusRequestEntityTooLarge, "Failed to buffer the request body: length limit exceeded"}
		}
		return &bodyError{http.StatusBadRequest, fmt.Sprintf("Failed to read the request body: %v", err)}
	}
	if err := json.Unmarshal(data, dst); err != nil {
		var jc *wirejson.Error
		var ute *json.UnmarshalTypeError
		if errors.As(err, &jc) || errors.As(err, &ute) {
			return &bodyError{http.StatusUnprocessableEntity, fmt.Sprintf("Failed to deserialize the JSON body into the target type: %v", err)}
		}
		return &bodyError{http.StatusBadRequest, fmt.Sprintf("Failed to parse the request body as JSON: %v", err)}
	}
	return nil
}

type bodyError struct {
	status  int
	message string
}

func (e *bodyError) Error() string { return e.message }

func writeBodyError(w http.ResponseWriter, err *bodyError) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(err.status)
	_, _ = w.Write([]byte(redact.Text(err.message)))
}

func (a *api) stateView(_ http.ResponseWriter, _ *http.Request) (int, any, error) {
	view, err := a.app.StateView()
	return http.StatusOK, view, err
}

func historyQuery(r *http.Request) (store.HistoryQuery, error) {
	var query store.HistoryQuery
	values := r.URL.Query()
	if raw := first(values, "before"); raw != nil {
		before, err := strconv.ParseInt(*raw, 10, 64)
		if err != nil {
			return query, &bodyError{http.StatusBadRequest, fmt.Sprintf("Invalid query string: %v", err)}
		}
		query.Before = &before
	}
	if raw := first(values, "limit"); raw != nil {
		limit, err := strconv.ParseUint(*raw, 10, 64)
		if err != nil {
			return query, &bodyError{http.StatusBadRequest, fmt.Sprintf("Invalid query string: %v", err)}
		}
		v := int(min(limit, math.MaxInt))
		query.Limit = &v
	}
	query.Status = first(values, "status")
	query.Q = first(values, "q")
	query.Cycle = first(values, "cycle")
	return query, nil
}

func first(values map[string][]string, key string) *string {
	list, ok := values[key]
	if !ok || len(list) == 0 {
		return nil
	}
	return &list[0]
}

func (a *api) history(kind string) handlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) (int, any, error) {
		query, err := historyQuery(r)
		if err != nil {
			return 0, nil, err
		}
		page, err := a.app.Store.HistoryPage(kind, query)
		return http.StatusOK, page, err
	}
}

func (a *api) proposalHistory(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	query, err := historyQuery(r)
	if err != nil {
		return 0, nil, err
	}
	page, err := a.app.Store.ProposalPage(query)
	return http.StatusOK, page, err
}

func (a *api) proposalDetail(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	detail, err := a.app.Store.ProposalDetail(r.PathValue("cycle"), r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if detail == nil {
		return 0, nil, engine.NotFound("Proposal not found")
	}
	return http.StatusOK, detail, nil
}

func (a *api) cycleDetail(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	cycle, err := store.Get[model.Cycle](a.app.Store, "cycle", r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if cycle == nil {
		return 0, nil, engine.NotFound("Cycle not found")
	}
	return http.StatusOK, *cycle, nil
}

func (a *api) cycleEvidence(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	value, err := export.RunEvidence(a.app.Store, r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if value == nil {
		return 0, nil, engine.NotFound("Cycle not found")
	}
	return http.StatusOK, value, nil
}

func (a *api) cycleAction(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := a.app.CycleAction(r.PathValue("id"), r.PathValue("action")); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ok": true}, nil
}

func (a *api) taskDetail(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	task, err := store.Get[model.Task](a.app.Store, "task", r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	if task == nil {
		return 0, nil, engine.NotFound("Task not found")
	}
	value, err := wirejson.GenericMap(*task)
	if err != nil {
		return 0, nil, err
	}
	value["allowed_actions"] = task.AllowedActions()
	value["effective_attempt_policy"] = model.PolicyOf(task.ExecutionConfig())
	live, err := a.app.Config()
	if err != nil {
		return 0, nil, err
	}
	value["operating_policy"] = map[string]any{
		"max_sessions_per_day": live.MaxSessionsPerDay,
		"max_workspace_bytes":  live.MaxWorkspaceBytes,
	}
	return http.StatusOK, value, nil
}

func (a *api) taskAction(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := a.app.TaskAction(r.PathValue("id"), r.PathValue("action")); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ok": true}, nil
}

func (a *api) getConfig(_ http.ResponseWriter, _ *http.Request) (int, any, error) {
	view, err := a.app.Settings()
	return http.StatusOK, view, err
}

type configUpdateBody struct {
	ExpectedRevision string                     `json:"expected_revision"`
	Config           map[string]json.RawMessage `json:"config"`
}

func (v *configUpdateBody) UnmarshalJSON(data []byte) error { return wirejson.DecodeStrict(data, v) }

func (a *api) saveConfig(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var body configUpdateBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	view, err := a.app.SaveConfig(body.ExpectedRevision, body.Config)
	if err != nil {
		var patch *engine.ConfigPatchError
		if errors.As(err, &patch) {
			return 0, nil, &bodyError{http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: " + patch.Error()}
		}
		return 0, nil, err
	}
	return http.StatusOK, view, nil
}

type baselineStartBody struct {
	ExpectedRevision string `json:"expected_revision"`
}

func (v *baselineStartBody) UnmarshalJSON(data []byte) error { return wirejson.DecodeStrict(data, v) }

func (a *api) baselineStart(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var body baselineStartBody
	if err := decodeBody(w, r, &body); err != nil {
		return 0, nil, err
	}
	check, err := a.app.StartBaseline(body.ExpectedRevision)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusAccepted, *check, nil
}

func (a *api) baselineLatest(_ http.ResponseWriter, _ *http.Request) (int, any, error) {
	view, err := a.app.BaselineView(nil)
	return http.StatusOK, view, err
}

func (a *api) baselineDetail(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	check, err := store.Get[model.BaselineCheck](a.app.Store, "baseline", id)
	if err != nil {
		return 0, nil, err
	}
	if check == nil {
		return 0, nil, engine.NotFound("Baseline check not found")
	}
	view, err := a.app.BaselineView(&id)
	return http.StatusOK, view, err
}

func (a *api) baselineCancel(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	if err := a.app.CancelBaseline(r.PathValue("id")); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ok": true}, nil
}

func (a *api) controlAction(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	body, err := a.app.ControlAction(r.PathValue("action"))
	return http.StatusOK, body, err
}

func (a *api) doctor(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	mode := model.CycleModeExecution
	if raw := first(r.URL.Query(), "mode"); raw != nil {
		switch *raw {
		case "execution":
		case "audit":
			mode = model.CycleModeAudit
		default:
			return 0, nil, &bodyError{http.StatusBadRequest, "Invalid query string: invalid value for `mode`"}
		}
	}
	cfg, err := a.app.Config()
	if err != nil {
		return 0, nil, err
	}
	result, _, err := a.app.Doctor(cfg, mode)
	status := http.StatusOK
	var body map[string]any
	if err != nil {
		status = apiStatus(err)
		body = map[string]any{"error": redact.Error(err)}
	} else {
		body = result
	}
	checked, err := wirejson.Generic(cfg)
	if err != nil {
		return 0, nil, err
	}
	body["checked_config"] = checked
	revision, err := cfg.Fingerprint()
	if err != nil {
		return 0, nil, err
	}
	body["checked_revision"] = revision
	return status, body, nil
}

// sandboxSelfTest proves the sandbox from inside a real one; with the sandbox off there is nothing to prove.
func (a *api) sandboxSelfTest(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	result, err := a.app.SelfTest(r.Context())
	return http.StatusOK, result, err
}

type catalogRequest struct {
	Backend config.Backend `json:"backend"`
	Binary  string         `json:"binary"`
}

func (v *catalogRequest) UnmarshalJSON(data []byte) error { return wirejson.DecodeStrict(data, v) }

func (a *api) modelCatalog(w http.ResponseWriter, r *http.Request) (int, any, error) {
	var request catalogRequest
	if err := decodeBody(w, r, &request); err != nil {
		return 0, nil, err
	}
	catalog, err := a.app.ModelCatalog(request.Backend, request.Binary)
	return http.StatusOK, catalog, err
}

func (a *api) events(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	events, err := a.app.Store.Events(first(r.URL.Query(), "entity"))
	return http.StatusOK, events, err
}
