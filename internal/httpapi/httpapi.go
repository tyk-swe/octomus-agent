// Package httpapi serves the authenticated operator API exactly as the
// current contract: route-layer authentication with bounded failure backoff,
// content-type enforcement on mutations, JSON redaction on every matched
// response, a 256 KiB request-body bound, security headers, /healthz, and the
// dashboard asset fallback outside the API prefix.
package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/evidence"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

// TokenEnv names the operator access token variable; its value is a secret.
const TokenEnv = "OCTOMUS_TOKEN"

const bodyLimit = 256 * 1024

type api struct {
	app       *engine.App
	tokenHash [32]byte
	failures  *authFailures
	assets    http.Handler
}

// authFailures implements bounded exponential delay: it starts
// at 100 ms, doubles to a 1 s ceiling and resets after a quiet minute.
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

// handlerFunc answers one matched route: (status, body). A nil error writes
// body as JSON; errors use {"error": message} at their classified status.
// Extraction rejections keep their own response form.
type handlerFunc func(w http.ResponseWriter, r *http.Request, params map[string]string) (int, any, error)

type apiRoute struct {
	method string
	segs   []string
	handle handlerFunc
}

// Router assembles the service handler: /healthz and the asset fallback
// unauthenticated, everything under /api behind the token middleware.
func Router(app *engine.App, token, assetsOverride, version string) http.Handler {
	s := &api{
		app:       app,
		tokenHash: sha256.Sum256([]byte(token)),
		failures:  &authFailures{},
		assets:    assetHandler(assetsOverride),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setHeaders(w)
		if r.URL.Path == "/healthz" {
			writeRawJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
			return
		}
		if path, ok := strings.CutPrefix(r.URL.Path, "/api"); ok && (path == "" || strings.HasPrefix(path, "/")) {
			s.serveAPI(w, r, path)
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

func (a *api) routes() []apiRoute {
	return []apiRoute{
		{"GET", segs("/state"), a.stateView},
		{"GET", segs("/tasks"), a.taskHistory},
		{"GET", segs("/cycles"), a.cycleHistory},
		{"GET", segs("/cycles/{id}"), a.cycleDetail},
		{"GET", segs("/cycles/{id}/evidence"), a.cycleEvidence},
		{"POST", segs("/cycles/{id}/{action}"), a.cycleAction},
		{"GET", segs("/proposals"), a.proposalHistory},
		{"GET", segs("/proposals/{cycle}/{id}"), a.proposalDetail},
		{"GET", segs("/prs"), a.prHistory},
		{"GET", segs("/tasks/{id}"), a.taskDetail},
		{"POST", segs("/tasks/{id}/{action}"), a.taskAction},
		{"GET", segs("/config"), a.getConfig},
		{"PUT", segs("/config"), a.saveConfig},
		{"POST", segs("/baseline-checks"), a.baselineStart},
		{"GET", segs("/baseline-checks/latest"), a.baselineLatest},
		{"GET", segs("/baseline-checks/{id}"), a.baselineDetail},
		{"POST", segs("/baseline-checks/{id}/cancel"), a.baselineCancel},
		{"POST", segs("/control/{action}"), a.controlAction},
		{"POST", segs("/doctor"), a.doctor},
		{"POST", segs("/model-catalog"), a.modelCatalog},
		{"GET", segs("/events"), a.events},
	}
}

func segs(pattern string) []string { return strings.Split(strings.TrimPrefix(pattern, "/"), "/") }

// serveAPI applies route-layer semantics: path matching picks the
// route (and its middleware) independent of method, so authentication and the
// content-type rule run before the 405 dispatch, which names the path's
// methods in Allow. Unmatched paths get the same 404 body without either check.
func (a *api) serveAPI(w http.ResponseWriter, r *http.Request, path string) {
	parts := segs(path)
	// allowed collects the methods of every route matching the path; the loop
	// only completes without a method match, which is exactly the 405 case.
	var allowed []string
	var matched *apiRoute
	params := map[string]string{}
	for _, route := range a.routes() {
		if len(route.segs) != len(parts) {
			continue
		}
		candidate := map[string]string{}
		ok := true
		for i, seg := range route.segs {
			if strings.HasPrefix(seg, "{") {
				name := seg[1 : len(seg)-1]
				candidate[name] = parts[i]
			} else if seg != parts[i] {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if !slices.Contains(allowed, route.method) {
			allowed = append(allowed, route.method)
		}
		if route.method == r.Method {
			route := route
			matched = &route
			params = candidate
			break
		}
	}
	if len(allowed) == 0 {
		writeAPIError(w, http.StatusNotFound, "Unknown API route")
		return
	}
	if !a.authenticate(w, r) {
		return
	}
	// Browser mutations require a non-simple content type. No CORS policy is enabled.
	if r.Method != http.MethodGet && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeAPIError(w, http.StatusUnsupportedMediaType, "Use application/json")
		return
	}
	if matched == nil {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	status, body, err := matched.handle(w, r, params)
	if err != nil {
		var be *bodyError
		if errors.As(err, &be) {
			writeBodyError(w, be)
		} else {
			writeAPIError(w, apiStatus(err), store.ErrorMessage(err))
		}
		return
	}
	writeJSON(w, status, body)
}

// authenticate verifies the bearer token by hash and answers failures after a
// bounded exponential delay.
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

// apiStatus mirrors ApiError::from: storage/codec failures are internal,
// typed conflicts map to 409, everything else is a bad request. Explicit
// sentinel errors carry their own status, including not-found cases.
func apiStatus(err error) int {
	var jc *wirejson.Error
	var sq *sqlite.Error
	var bc *engine.BaselineConflict
	switch {
	case errors.As(err, &sq) || errors.As(err, &jc):
		return http.StatusInternalServerError
	case errors.Is(err, engine.ErrTaskNotFound),
		errors.Is(err, engine.ErrCycleNotFound),
		errors.Is(err, engine.ErrUnknownControl),
		errors.Is(err, engine.ErrUnknownCycleAction),
		errors.Is(err, engine.ErrUnknownTaskAction),
		errors.Is(err, engine.ErrBaselineNotFound),
		errors.Is(err, engine.ErrProposalNotFound):
		return http.StatusNotFound
	case errors.As(err, &bc) || engine.IsActionConflict(err):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// writeAPIError emits the one error body shape the dashboard reads. Matched-
// route errors pass through the same redaction as success bodies.
func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// writeJSON encodes the response, redacts operator data and preserves
// server-generated settings transform metadata, then writes compact JSON.
func writeJSON(w http.ResponseWriter, status int, value any) {
	_, settingsView := value.(*engine.SettingsView)
	var generic any
	err := genericJSON(value, &generic)
	var out []byte
	if err == nil {
		if object, ok := generic.(map[string]any); ok && settingsView {
			// transformed_fields is server-generated structural metadata: running
			// secret scrubbing over its field names and paths can make the dashboard
			// lose the association between a redacted preview and its config field.
			transforms, hasTransforms := object["transformed_fields"]
			delete(object, "transformed_fields")
			store.RedactJSON(object)
			if hasTransforms {
				object["transformed_fields"] = transforms
			}
		} else {
			generic = store.RedactJSON(generic)
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

// genericJSON re-encodes value into dst as generic JSON (maps, slices and
// json.Number), so numbers keep their exact encoded form.
func genericJSON(value, dst any) error {
	data, err := wirejson.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(dst)
}

// writeRawJSON answers without redaction: healthz and assets never carry
// operator data and bypass response redaction.
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

// decodeBody reads one JSON request body under the 256 KiB bound and classifies
// failures the way axum's Json extractor does: syntax at 400, data at 422,
// overflow at 413 — all text/plain, not the JSON error shape.
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

// bodyError is an extraction-layer rejection: a plain-text status that never
// takes the JSON error shape, matching axum's Json and body rejections.
type bodyError struct {
	status  int
	message string
}

func (e *bodyError) Error() string { return e.message }

func writeBodyError(w http.ResponseWriter, err *bodyError) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(err.status)
	_, _ = w.Write([]byte(err.message))
}

func (a *api) stateView(_ http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
	view, err := a.app.StateView()
	return http.StatusOK, view, err
}

// historyQuery parses the dashboard's paged history filter; malformed numbers
// produce a 400 response.
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
		// Saturate instead of wrapping: an unsigned value above MaxInt would
		// otherwise turn negative and page one item instead of the store's cap.
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

func (a *api) taskHistory(_ http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
	query, err := historyQuery(r)
	if err != nil {
		return 0, nil, err
	}
	page, err := a.app.Store.HistoryPage("task", query)
	return http.StatusOK, page, err
}

func (a *api) cycleHistory(_ http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
	query, err := historyQuery(r)
	if err != nil {
		return 0, nil, err
	}
	page, err := a.app.Store.HistoryPage("cycle", query)
	return http.StatusOK, page, err
}

func (a *api) prHistory(_ http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
	query, err := historyQuery(r)
	if err != nil {
		return 0, nil, err
	}
	page, err := a.app.Store.HistoryPage("pr", query)
	return http.StatusOK, page, err
}

func (a *api) proposalHistory(_ http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
	query, err := historyQuery(r)
	if err != nil {
		return 0, nil, err
	}
	page, err := a.app.Store.ProposalPage(query)
	return http.StatusOK, page, err
}

func (a *api) proposalDetail(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	detail, err := a.app.Store.ProposalDetail(params["cycle"], params["id"])
	if err != nil {
		return 0, nil, err
	}
	if detail == nil {
		return 0, nil, engine.ErrProposalNotFound
	}
	return http.StatusOK, detail, nil
}

func (a *api) cycleDetail(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	cycle, err := store.Get[model.Cycle](a.app.Store, "cycle", params["id"])
	if err != nil {
		return 0, nil, err
	}
	if cycle == nil {
		return 0, nil, engine.ErrCycleNotFound
	}
	return http.StatusOK, *cycle, nil
}

func (a *api) cycleEvidence(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	value, err := evidence.RunEvidence(a.app.Store, params["id"])
	if err != nil {
		return 0, nil, err
	}
	if value == nil {
		return 0, nil, engine.ErrCycleNotFound
	}
	return http.StatusOK, value, nil
}

func (a *api) cycleAction(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	if err := a.app.CycleAction(params["id"], params["action"]); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ok": true}, nil
}

func (a *api) taskDetail(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	task, err := store.Get[model.Task](a.app.Store, "task", params["id"])
	if err != nil {
		return 0, nil, err
	}
	if task == nil {
		return 0, nil, engine.ErrTaskNotFound
	}
	var value map[string]any
	if err := genericJSON(*task, &value); err != nil {
		return 0, nil, err
	}
	value["allowed_actions"] = task.AllowedActions()
	value["effective_attempt_policy"] = model.AttemptPolicyFromConfig(task.ExecutionConfig())
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

func (a *api) taskAction(_ http.ResponseWriter, r *http.Request, params map[string]string) (int, any, error) {
	if err := a.app.TaskAction(r.Context(), params["id"], params["action"]); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ok": true}, nil
}

func (a *api) getConfig(_ http.ResponseWriter, _ *http.Request, _ map[string]string) (int, any, error) {
	view, err := a.app.Settings()
	return http.StatusOK, view, err
}

// configUpdateBody is the revision-gated settings write: the canonical
// revision the operator loaded plus only the top-level fields being replaced.
// Omitted fields keep their canonical saved values; whole-configuration bodies
// without the revision are unknown fields here and are rejected.
type configUpdateBody struct {
	ExpectedRevision string                     `json:"expected_revision"`
	Config           map[string]json.RawMessage `json:"config"`
}

func (v *configUpdateBody) UnmarshalJSON(data []byte) error {
	type plain configUpdateBody
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, true, false); err != nil {
		return err
	}
	*v = configUpdateBody(decoded)
	return nil
}

func (a *api) saveConfig(w http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
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

// baselineStartBody rejects unknown fields; the request names the saved
// canonical configuration revision rather than echoing displayed values.
type baselineStartBody struct {
	ExpectedRevision string `json:"expected_revision"`
}

func (v *baselineStartBody) UnmarshalJSON(data []byte) error {
	type plain baselineStartBody
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, true, false); err != nil {
		return err
	}
	*v = baselineStartBody(decoded)
	return nil
}

func (a *api) baselineStart(w http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
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

func (a *api) baselineLatest(_ http.ResponseWriter, _ *http.Request, _ map[string]string) (int, any, error) {
	view, err := a.app.BaselineView(nil)
	return http.StatusOK, view, err
}

func (a *api) baselineDetail(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	id := params["id"]
	check, err := store.Get[model.BaselineCheck](a.app.Store, "baseline", id)
	if err != nil {
		return 0, nil, err
	}
	if check == nil {
		return 0, nil, engine.ErrBaselineNotFound
	}
	view, err := a.app.BaselineView(&id)
	return http.StatusOK, view, err
}

func (a *api) baselineCancel(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	if err := a.app.CancelBaseline(params["id"]); err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"ok": true}, nil
}

func (a *api) controlAction(_ http.ResponseWriter, _ *http.Request, params map[string]string) (int, any, error) {
	body, err := a.app.ControlAction(params["action"])
	return http.StatusOK, body, err
}

func (a *api) doctor(_ http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
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
	result, err := a.app.DoctorFor(cfg, mode)
	status := http.StatusOK
	var body map[string]any
	if err != nil {
		status = apiStatus(err)
		body = map[string]any{"error": store.ErrorMessage(err)}
	} else {
		body = result
	}
	var checked any
	if err := genericJSON(cfg, &checked); err != nil {
		return 0, nil, err
	}
	body["checked_config"] = checked
	revision, err := cfg.Fingerprint()
	if err != nil {
		return 0, nil, err
	}
	// The checked canonical revision is the authoritative identity the result
	// applies to; the redacted display copy alone cannot carry it.
	body["checked_revision"] = revision
	return status, body, nil
}

// catalogRequest rejects unknown fields.
type catalogRequest struct {
	Backend config.Backend `json:"backend"`
	Binary  string         `json:"binary"`
}

func (v *catalogRequest) UnmarshalJSON(data []byte) error {
	type plain catalogRequest
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, true, false); err != nil {
		return err
	}
	*v = catalogRequest(decoded)
	return nil
}

func (a *api) modelCatalog(w http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
	var request catalogRequest
	if err := decodeBody(w, r, &request); err != nil {
		return 0, nil, err
	}
	catalog, err := a.app.ModelCatalog(request.Backend, request.Binary)
	return http.StatusOK, catalog, err
}

func (a *api) events(_ http.ResponseWriter, r *http.Request, _ map[string]string) (int, any, error) {
	events, err := a.app.Store.Events(first(r.URL.Query(), "entity"))
	return http.StatusOK, events, err
}
