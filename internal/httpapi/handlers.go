package httpapi

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// acknowledged answers an action that returns nothing but its outcome.
func acknowledged(err error) (int, any, error) {
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, Acknowledgement{OK: true}, nil
}

// pathEnum parses a closed-vocabulary path word; an unknown word is a missing route.
func pathEnum[T any, P interface {
	*T
	encoding.TextUnmarshaler
}](r *http.Request, name, unknown string) (T, error) {
	var value T
	if err := P(&value).UnmarshalText([]byte(r.PathValue(name))); err != nil {
		return value, engine.NotFound(unknown)
	}
	return value, nil
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
			return query, invalidQuery(err)
		}
		query.Before = &before
	}
	if raw := first(values, "limit"); raw != nil {
		limit, err := strconv.ParseUint(*raw, 10, 64)
		if err != nil {
			return query, invalidQuery(err)
		}
		v := int(min(limit, math.MaxInt))
		query.Limit = &v
	}
	query.Status = first(values, "status")
	query.Q = first(values, "q")
	query.Cycle = first(values, "cycle")
	return query, nil
}

func invalidQuery(err error) *bodyError {
	return &bodyError{http.StatusBadRequest, fmt.Sprintf("Invalid query string: %v", err)}
}

func first(values map[string][]string, key string) *string {
	list, ok := values[key]
	if !ok || len(list) == 0 {
		return nil
	}
	return &list[0]
}

func history[T any](read func(store.HistoryQuery) (T, error)) handlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) (int, any, error) {
		query, err := historyQuery(r)
		if err != nil {
			return 0, nil, err
		}
		page, err := read(query)
		return http.StatusOK, page, err
	}
}

func (a *api) proposalDetail(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	detail, err := a.app.ProposalDetail(r.PathValue("cycle"), r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, detail, nil
}

func (a *api) cycleDetail(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	cycle, err := a.app.CycleDetail(r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, *cycle, nil
}

func (a *api) cycleEvidence(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	value, err := a.app.RunEvidence(r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, value, nil
}

func (a *api) cycleAction(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	action, err := pathEnum[model.CycleAction](r, "action", "Unknown cycle action")
	if err != nil {
		return 0, nil, err
	}
	return acknowledged(a.app.CycleAction(r.PathValue("id"), action))
}

func (a *api) taskDetail(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	detail, err := a.app.TaskDetail(r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, *detail, nil
}

func (a *api) taskAction(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	action, err := pathEnum[model.TaskAction](r, "action", "Unknown task action")
	if err != nil {
		return 0, nil, err
	}
	return acknowledged(a.app.TaskAction(r.PathValue("id"), action))
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
			return 0, nil, deserializeBodyError(patch)
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
	view, err := a.app.BaselineView(&id)
	return http.StatusOK, view, err
}

func (a *api) baselineCancel(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	return acknowledged(a.app.CancelBaseline(r.PathValue("id")))
}

func (a *api) controlAction(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	action, err := pathEnum[model.ControlAction](r, "action", "Unknown control")
	if err != nil {
		return 0, nil, err
	}
	body, err := a.app.ControlAction(action)
	return http.StatusOK, body, err
}

func (a *api) doctor(w http.ResponseWriter, r *http.Request) (int, any, error) {
	// Consume the ignored body so net/http can detect a disconnected HTTP/1 caller
	// while the diagnostic is running, rather than waiting for this handler to end.
	if _, err := readBody(w, r); err != nil {
		return 0, nil, err
	}
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
	result, _, err := a.app.Doctor(r.Context(), cfg, mode)
	revision, fingerprintErr := cfg.Fingerprint()
	if fingerprintErr != nil {
		return 0, nil, fingerprintErr
	}
	if err != nil {
		return apiStatus(err), DoctorFailure{
			ErrorResponse: ErrorResponse{Error: redact.Error(err), Code: engine.ActionCode(err)},
			CheckedConfig: cfg, CheckedRevision: revision,
		}, nil
	}
	return http.StatusOK, DoctorResponse{DoctorResult: *result, CheckedConfig: cfg, CheckedRevision: revision}, nil
}

type DoctorResponse struct {
	engine.DoctorResult
	CheckedConfig   config.Config `json:"checked_config"`
	CheckedRevision string        `json:"checked_revision"`
}

type DoctorFailure struct {
	ErrorResponse
	CheckedConfig   config.Config `json:"checked_config"`
	CheckedRevision string        `json:"checked_revision"`
}

// sandboxSelfTest proves the sandbox from inside a real one; with the sandbox off there is nothing to prove.
func (a *api) sandboxSelfTest(w http.ResponseWriter, r *http.Request) (int, any, error) {
	if _, err := readBody(w, r); err != nil {
		return 0, nil, err
	}
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
	catalog, err := a.app.ModelCatalog(r.Context(), request.Backend, request.Binary)
	return http.StatusOK, catalog, err
}

func (a *api) events(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	events, err := a.app.Events(first(r.URL.Query(), "entity"))
	return http.StatusOK, events, err
}
