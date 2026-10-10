package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/export"
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
	return http.StatusOK, map[string]any{"ok": true}, nil
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
	return acknowledged(a.app.CycleAction(r.PathValue("id"), r.PathValue("action")))
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
	return acknowledged(a.app.TaskAction(r.PathValue("id"), r.PathValue("action")))
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
	return acknowledged(a.app.CancelBaseline(r.PathValue("id")))
}

func (a *api) controlAction(_ http.ResponseWriter, r *http.Request) (int, any, error) {
	body, err := a.app.ControlAction(r.PathValue("action"))
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
	events, err := a.app.Store.Events(first(r.URL.Query(), "entity"))
	return http.StatusOK, events, err
}
