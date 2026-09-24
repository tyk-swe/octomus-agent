// api.go holds the engine-side operations the HTTP layer invokes. Each method
// follows the operator control contract: gate ordering, conflict
// classification, durable writes and operator events all match.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// Not-found and unknown-action errors the HTTP layer maps to 404, matching
// not-found responses.
var (
	ErrCycleNotFound      = errors.New("Cycle not found")
	ErrUnknownControl     = errors.New("Unknown control")
	ErrUnknownCycleAction = errors.New("Unknown cycle action")
	ErrUnknownTaskAction  = errors.New("Unknown task action")
	ErrBaselineNotFound   = errors.New("Baseline check not found")
	ErrProposalNotFound   = errors.New("Proposal not found")
)

// genericMap re-encodes a typed record as generic JSON with exact numbers so
// response assembly preserves the store's saved spelling.
func genericMap(value any) (map[string]any, error) {
	data, err := wirejson.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

// ControlAction runs the conflict check under the scheduler gate, then the durable mode transition and its
// operator event. The response is the serialized control record (plus
// planning_capacity for resume), exactly as the dashboard reads it.
func (a *App) ControlAction(action string) (map[string]any, error) {
	a.gate.Lock()
	control, err := a.Control()
	if err != nil {
		a.gate.Unlock()
		return nil, err
	}
	a.runtimeMu.Lock()
	baselineActive := a.runtime.baseline != nil
	idle := a.runtime.idle()
	auditActive := a.runtime.cycle != nil && a.runtime.cycle.mode == model.CycleModeAudit ||
		a.runtime.preflight && a.runtime.preflightMode == model.CycleModeAudit
	a.runtimeMu.Unlock()
	if ((action == "audit" || action == "cycle") && (!control.Paused || !idle)) ||
		((action == "resume" || action == "cycle") && auditActive) ||
		(action == "resume" && baselineActive) {
		a.gate.Unlock()
		var message string
		if baselineActive {
			message = "Wait for the baseline check to finish."
		} else {
			switch action {
			case "audit":
				message = "Audits require paused operation with no active work. Pause the service and wait for active work to finish."
			case "cycle":
				message = "Run once requires paused operation with no active work. Pause the service and wait for active work to finish."
			default:
				message = "Wait for the audit to finish before starting continuous operation."
			}
		}
		return nil, conflictError(message)
	}
	if action == "audit" {
		// Audit includes a remote preflight, so the gate drops for remote work
		// and the launch itself revalidates paused and idle state.
		a.gate.Unlock()
		_, err := a.StartAudit(a.ctx)
		a.gate.Lock()
		if err != nil {
			a.gate.Unlock()
			return nil, err
		}
		if err := a.Store.Event("system", "operator", "audit"); err != nil {
			a.gate.Unlock()
			return nil, err
		}
		control, err = a.Control()
		if err != nil {
			a.gate.Unlock()
			return nil, err
		}
		body, err := genericMap(control)
		a.gate.Unlock()
		return body, err
	}
	body := map[string]any{}
	switch action {
	case "pause":
		control.SetMode(model.OperatingModePaused)
		a.invalidatePrObservation()
	case "resume":
		if err := a.enterContinuous(&control); err != nil {
			a.gate.Unlock()
			return nil, err
		}
	case "cycle":
		cfg, err := a.Config()
		if err != nil {
			a.gate.Unlock()
			return nil, err
		}
		if err := cfg.Validate(true); err != nil {
			a.gate.Unlock()
			return nil, err
		}
		capacity, err := a.Store.PlanningCapacity()
		if err != nil {
			a.gate.Unlock()
			return nil, err
		}
		if err := capacity.EnsureAvailable(); err != nil {
			a.gate.Unlock()
			return nil, err
		}
		if err := a.Store.StartBatch(&control); err != nil {
			a.gate.Unlock()
			return nil, err
		}
	default:
		a.gate.Unlock()
		return nil, ErrUnknownControl
	}
	if action != "resume" {
		if err := a.Store.SaveControl(control); err != nil {
			a.gate.Unlock()
			return nil, err
		}
	}
	if err := a.Store.Event("system", "operator", action); err != nil {
		a.gate.Unlock()
		return nil, err
	}
	body, err = genericMap(control)
	if err != nil {
		a.gate.Unlock()
		return nil, err
	}
	if action == "resume" {
		capacity, err := a.Store.PlanningCapacity()
		if err != nil {
			a.gate.Unlock()
			return nil, err
		}
		body["planning_capacity"] = capacity
	}
	a.gate.Unlock()
	return body, nil
}

// CycleAction handles running cycles
// conflict, archive stamps the lifecycle and discard requires the archive.
func (a *App) CycleAction(id, action string) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	cycle, err := store.Get[model.Cycle](a.Store, "cycle", id)
	if err != nil {
		return err
	}
	if cycle == nil {
		return ErrCycleNotFound
	}
	if cycle.Status == model.CycleRunning {
		return conflictError("Wait for planning to finish")
	}
	switch action {
	case "archive":
		now := model.Now()
		cycle.Lifecycle.ArchivedAt = &now
		if err := a.Store.Put("cycle", id, *cycle); err != nil {
			return err
		}
	case "discard":
		if cycle.Lifecycle.ArchivedAt == nil {
			return conflictError("Archive the cycle before discarding its workspace")
		}
		if err := a.DiscardCycle(cycle); err != nil {
			return err
		}
	default:
		return ErrUnknownCycleAction
	}
	return a.Store.Event(id, "operator", action)
}

// SaveConfig mirrors save_config: the service must be paused and drained, the
// new policy validates in its incomplete form, and identity changes require a
// clean task ledger.
func (a *App) SaveConfig(c config.Config) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	control, err := a.Control()
	if err != nil {
		return err
	}
	a.runtimeMu.Lock()
	idle := a.runtime.idle()
	a.runtimeMu.Unlock()
	if !control.Paused || !idle {
		return conflictError("Pause and wait for active work to finish before changing configuration.")
	}
	if err := c.Validate(false); err != nil {
		return err
	}
	old, err := a.Config()
	if err != nil {
		return err
	}
	if !old.SameRemoteIdentity(c) || old.BranchPrefix != c.BranchPrefix {
		unresolved, err := a.Store.HasUnresolvedTasks()
		if err != nil {
			return err
		}
		if unresolved {
			return conflictError("Resolve or cancel existing tasks before changing repository identity or branch policy.")
		}
	}
	if err := a.Store.Put("settings", "config", c); err != nil {
		return err
	}
	a.invalidatePrObservation()
	return a.Store.Event("system", "configuration", "Operator saved configuration")
}

// DoctorFor mirrors doctor_for: validate for the requested mode, check the
// remote, then connect each required backend and validate every route against
// the discovered catalog. The result names which Codex version was observed
// when the codex backend answered.
func (a *App) DoctorFor(cfg config.Config, mode model.CycleMode) (map[string]any, error) {
	if mode == model.CycleModeAudit {
		if err := cfg.ValidateAudit(); err != nil {
			return nil, err
		}
	} else {
		if err := cfg.Validate(true); err != nil {
			return nil, err
		}
	}
	if err := gitops.ValidateRemote(a.ctx, cfg); err != nil {
		return nil, err
	}
	routes := cfg.RoutesFor(mode == model.CycleModeAudit)
	seen := map[config.Backend]bool{}
	backends := []config.Backend{}
	for _, named := range routes {
		if !seen[named.Route.Backend] {
			seen[named.Route.Backend] = true
			backends = append(backends, named.Route.Backend)
		}
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i] < backends[j] })
	diagnostics := []map[string]any{}
	models := []runner.Model{}
	warnings := []string{}
	errs := []string{}
	for _, backend := range backends {
		checkErr := func() error {
			client, err := a.connectRunner(a.ctx, backend, cfg, a.DataDir, "system")
			if err != nil {
				return err
			}
			defer client.Close()
			diagnostic, err := client.Diagnostics(a.DataDir)
			if err != nil {
				return err
			}
			catalog, err := client.Models(a.DataDir)
			if err != nil {
				return err
			}
			for _, named := range routes {
				if named.Route.Backend != backend {
					continue
				}
				if err := runner.ValidateRoute(named.Route, catalog); err != nil {
					errs = append(errs, named.Name+": "+err.Error())
				}
			}
			if warning, ok := diagnostic["warning"].(string); ok && warning != "" {
				fmt.Fprintf(os.Stderr, "WARN %s\n", warning)
				warnings = append(warnings, warning)
			}
			models = append(models, catalog...)
			diagnostics = append(diagnostics, diagnostic)
			return nil
		}()
		if checkErr != nil {
			errs = append(errs, backend.Display()+": "+checkErr.Error())
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	modeName := "all"
	if mode == model.CycleModeAudit {
		modeName = "planning"
	}
	message := "Repository, GitHub authentication, and " + modeName + " model routes are available."
	if len(warnings) > 0 {
		message += " Warning: " + strings.Join(warnings, " ")
	}
	result := map[string]any{
		"ok": true, "mode": mode, "models": models, "backends": diagnostics,
		"warnings": warnings, "message": message,
	}
	for _, diagnostic := range diagnostics {
		if diagnostic["backend"] == "codex" {
			result["codex_version"] = diagnostic["version"]
			result["tested_codex_version"] = runner.CodexTestedVersion
			break
		}
	}
	return result, nil
}

// ModelCatalog mirrors model_catalog: validate the binary override, patch the
// matching backend's binary on a config copy and list the discovered models.
func (a *App) ModelCatalog(backend config.Backend, binary string) ([]runner.Model, error) {
	if err := config.ValidateBinary(binary); err != nil {
		return nil, err
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	switch backend {
	case config.BackendCodex:
		cfg.CodexBinary = binary
	case config.BackendOpencode:
		cfg.OpencodeBinary = binary
	default:
		return nil, errors.New("Invalid backend")
	}
	client, err := a.connectRunner(a.ctx, backend, cfg, a.DataDir, "system")
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.Models(a.DataDir)
}

// StateView assembles the live dashboard document: the stored snapshot
// flattened with runtime state, capacity, notification health and the latest
// baseline summary in the state view.
func (a *App) StateView() (map[string]any, error) {
	control, err := a.Control()
	if err != nil {
		return nil, err
	}
	snapshot, err := a.Store.Dashboard()
	if err != nil {
		return nil, err
	}
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	planningCapacity, err := a.Store.PlanningCapacity()
	if err != nil {
		return nil, err
	}
	prCapacity, err := a.PrCapacity()
	if err != nil {
		return nil, err
	}
	latest, err := a.Store.LatestBaseline()
	if err != nil {
		return nil, err
	}
	notifications, err := a.Store.NotificationHealth()
	if err != nil {
		return nil, err
	}
	a.runtimeMu.Lock()
	var cycleMode *model.CycleMode
	if a.runtime.cycle != nil {
		mode := a.runtime.cycle.mode
		cycleMode = &mode
	} else if a.runtime.preflight {
		mode := a.runtime.preflightMode
		cycleMode = &mode
	}
	activeTasks := len(a.runtime.tasks)
	cycleActive := a.runtime.cycle != nil
	baselineActive := a.runtime.baseline != nil
	a.runtimeMu.Unlock()
	// A committed running cycle is durable before its runtime slot is assigned,
	// so the stored record must count toward activity as well: reporting
	// inactive while a running cycle is visible contradicts the document.
	if !cycleActive || cycleMode == nil {
		running, err := a.Store.RunningCycles()
		if err != nil {
			return nil, err
		}
		if len(running) > 0 {
			cycleActive = true
			if cycleMode == nil {
				mode := running[0].Mode
				cycleMode = &mode
			}
		}
	}
	status := "idle"
	switch {
	case cycleMode != nil && *cycleMode == model.CycleModeAudit:
		status = "auditing"
	case control.Paused:
		status = "paused"
	case control.Error != nil:
		status = "unhealthy"
	case cycleActive || activeTasks > 0:
		status = "running"
	}
	view, err := genericMap(snapshot)
	if err != nil {
		return nil, err
	}
	controlJSON, err := genericMap(control)
	if err != nil {
		return nil, err
	}
	var baselineView any
	if latest != nil {
		baselineView = map[string]any{
			"id":              latest.ID,
			"status":          latest.Status.String(),
			"started_at":      latest.StartedAt,
			"completed_at":    latest.CompletedAt,
			"error":           latest.Error,
			"config_matches":  a.BaselineConfigMatches(latest, cfg),
			"revision_status": a.BaselineRevisionStatus(latest, cfg),
		}
	}
	storage, found, err := a.Store.GetValue("settings", "storage")
	if err != nil {
		return nil, err
	}
	if !found {
		storage = nil
	}
	for key, value := range map[string]any{
		"status":            status,
		"control":           controlJSON,
		"repository":        cfg.GitHubRepo,
		"configured":        cfg.Validate(true) == nil,
		"audit_configured":  cfg.ValidateAudit() == nil,
		"active_cycle_mode": cycleMode,
		"active_tasks":      activeTasks,
		"cycle_active":      cycleActive,
		"baseline_active":   baselineActive,
		"baseline":          baselineView,
		"notifications":     notifications,
		"session_limit":     cfg.MaxSessionsPerDay,
		"storage_limit":     cfg.MaxWorkspaceBytes,
		"storage":           storage,
		"planning_capacity": planningCapacity,
		"pr_capacity":       prCapacity,
	} {
		view[key] = value
	}
	return view, nil
}
