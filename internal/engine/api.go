package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

var (
	ErrCycleNotFound      = errors.New("Cycle not found")
	ErrUnknownControl     = errors.New("Unknown control")
	ErrUnknownCycleAction = errors.New("Unknown cycle action")
	ErrUnknownTaskAction  = errors.New("Unknown task action")
	ErrBaselineNotFound   = errors.New("Baseline check not found")
	ErrProposalNotFound   = errors.New("Proposal not found")
)

func (a *App) ControlAction(action string) (map[string]any, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	control, err := a.Control()
	if err != nil {
		return nil, err
	}
	if err := a.controlConflict(action, control); err != nil {
		return nil, err
	}
	switch action {
	case "audit":
		a.withoutGate(func() { _, err = a.StartAudit(a.ctx) })
		if err != nil {
			return nil, err
		}
		control, err = a.Control()
		if err != nil {
			return nil, err
		}
		return wirejson.GenericMap(control)
	case "pause":
		if err := a.pauseLocked(&control, nil); err != nil {
			return nil, err
		}
	case "resume":
		if err := a.enterContinuous(&control); err != nil {
			return nil, err
		}
	case "cycle":
		cfg, err := a.Config()
		if err != nil {
			return nil, err
		}
		if err := cfg.Validate(true); err != nil {
			return nil, err
		}
		if err := a.startRunOnceBatch(&control); err != nil {
			return nil, err
		}
		a.notify()
	default:
		return nil, ErrUnknownControl
	}
	// Activity is best-effort after the operator change has committed.
	_ = a.Store.Event("system", "operator", action)
	body, err := wirejson.GenericMap(control)
	if err != nil {
		return nil, err
	}
	if action == "resume" {
		capacity, err := a.Store.PlanningCapacity()
		if err != nil {
			return nil, err
		}
		body["planning_capacity"] = capacity
	}
	return body, nil
}

func (a *App) controlConflict(action string, control model.Control) error {
	if action == "audit" || action == "cycle" || action == "resume" {
		if err := a.recoveryConflict(); err != nil {
			return err
		}
	}
	a.runtimeMu.Lock()
	baselineActive := a.runtime.baseline != nil
	idle := a.runtime.idle()
	auditActive := a.runtime.auditActive()
	a.runtimeMu.Unlock()
	refused := (action == "audit" || action == "cycle") && (!control.Paused || !idle) ||
		(action == "resume" || action == "cycle") && auditActive ||
		action == "resume" && baselineActive
	switch {
	case !refused:
		return nil
	case baselineActive:
		return conflictError("Wait for the baseline check to finish.")
	case action == "audit":
		return conflictError("Audits require paused operation with no active work. Pause the service and wait for active work to finish.")
	case action == "cycle":
		return conflictError("Run once requires paused operation with no active work. Pause the service and wait for active work to finish.")
	default:
		return conflictError("Wait for the audit to finish before starting continuous operation.")
	}
}

func (a *App) CycleAction(id, action string) error {
	if action != "archive" && action != "discard" {
		return ErrUnknownCycleAction
	}
	a.gate.Lock()
	if err := a.ctx.Err(); err != nil {
		a.gate.Unlock()
		return err
	}
	a.wg.Add(1)
	defer a.wg.Done()
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
	if a.cleanupClaimed(cleanupCycle, id) {
		return conflictError("Workspace cleanup is in progress for this cycle; wait for it to finish")
	}
	switch action {
	case "archive":
		if cycle.Lifecycle.ArchivedAt != nil {
			return conflictError("The cycle is already archived")
		}
		now := model.Now()
		cycle.Lifecycle.ArchivedAt = &now
		if err := a.Store.Put("cycle", id, *cycle); err != nil {
			return err
		}
	case "discard":
		if cycle.Lifecycle.ArchivedAt == nil {
			return conflictError("Archive the cycle before discarding its workspace")
		}
		if err := a.discardCycle(cycle); err != nil {
			return err
		}
	}
	_ = a.Store.Event(id, "operator", action)
	return nil
}

type SettingsView struct {
	Config            map[string]any            `json:"config"`
	Revision          string                    `json:"revision"`
	TransformedFields []redact.DisplayTransform `json:"transformed_fields"`
}

func NewSettingsView(c config.Config) (*SettingsView, error) {
	revision, err := c.Fingerprint()
	if err != nil {
		return nil, err
	}
	generic, err := wirejson.GenericMap(c)
	if err != nil {
		return nil, err
	}
	display, fields := redact.DisplayJSON(generic)
	return &SettingsView{Config: display, Revision: revision, TransformedFields: fields}, nil
}

func (a *App) Settings() (*SettingsView, error) {
	c, err := a.Config()
	if err != nil {
		return nil, err
	}
	return NewSettingsView(c)
}

type ConfigPatchError struct{ inner error }

func (e *ConfigPatchError) Error() string { return e.inner.Error() }

func mergeConfigPatch(live config.Config, patch map[string]json.RawMessage) (config.Config, error) {
	generic, err := wirejson.GenericMap(live)
	if err != nil {
		return config.Config{}, err
	}
	for key, raw := range patch {
		generic[key] = raw
	}
	merged, err := json.Marshal(generic)
	if err != nil {
		return config.Config{}, err
	}
	var next config.Config
	if err := json.Unmarshal(merged, &next); err != nil {
		return config.Config{}, &ConfigPatchError{err}
	}
	return next, nil
}

func (a *App) SaveConfig(expectedRevision string, patch map[string]json.RawMessage) (*SettingsView, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	control, err := a.Control()
	if err != nil {
		return nil, err
	}
	a.runtimeMu.Lock()
	idle := a.runtime.idle()
	a.runtimeMu.Unlock()
	if !control.Paused || !idle {
		return nil, conflictError("Pause and wait for active work to finish before changing configuration.")
	}
	old, err := a.Config()
	if err != nil {
		return nil, err
	}
	revision, err := old.Fingerprint()
	if err != nil {
		return nil, err
	}
	if revision != expectedRevision {
		return nil, conflictError("The saved configuration changed; reload settings and check the current values.")
	}
	c, err := mergeConfigPatch(old, patch)
	if err != nil {
		return nil, err
	}
	if err := a.deployment.check(c); err != nil {
		return nil, err
	}
	c = a.deployment.pin(c)
	if err := c.Validate(false); err != nil {
		return nil, err
	}
	if !old.SameRemoteIdentity(c) || old.BranchPrefix != c.BranchPrefix {
		unresolved, err := a.Store.HasUnresolvedTasks()
		if err != nil {
			return nil, err
		}
		if unresolved {
			return nil, conflictError("Resolve or cancel existing tasks before changing repository identity or branch policy.")
		}
	}
	if err := a.Store.Put("settings", "config", c); err != nil {
		return nil, err
	}
	a.invalidatePRs()
	// Activity is best-effort after the configuration change has committed.
	_ = a.Store.Event("system", "configuration", "Operator saved configuration")
	return NewSettingsView(c)
}

func (a *App) Doctor(cfg config.Config, mode model.CycleMode) (map[string]any, []string, error) {
	if err := a.admitDiagnostic(); err != nil {
		return nil, nil, err
	}
	defer a.wg.Done()
	// The containment self-test needs no configuration, so it runs first: a broken sandbox is reported even when the
	// configuration or repository check fails too.
	errs := []string{}
	sandboxResult := map[string]any{"mode": a.sandbox.Mode().String()}
	if a.sandbox.Mode() == sandbox.ModeDocker {
		selfTest, err := a.SelfTest(a.ctx)
		if err != nil {
			return nil, nil, err
		}
		if failure := selfTest.failure(); failure != nil {
			errs = append(errs, failure.Error())
		}
		sandboxResult["self_test"] = selfTest
	}
	withSelfTest := func(err error) error {
		if len(errs) == 0 {
			return err
		}
		return fmt.Errorf("%s; %w", strings.Join(errs, "; "), err)
	}
	if mode == model.CycleModeAudit {
		if err := cfg.ValidateAudit(); err != nil {
			return nil, nil, withSelfTest(err)
		}
	} else {
		if err := cfg.Validate(true); err != nil {
			return nil, nil, withSelfTest(err)
		}
	}
	if err := gitops.ValidateRemote(a.ctx, cfg); err != nil {
		return nil, nil, withSelfTest(err)
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
	diagnostics := []runner.Diagnostics{}
	models := []runner.Model{}
	warnings := []string{}
	for _, backend := range backends {
		checkErr := func() (err error) {
			scratch, discard, err := a.scratchWorkspace()
			if err != nil {
				return err
			}
			defer discard()
			client, err := a.connectRunner(a.ctx, backend, cfg, scratch, "system")
			if err != nil {
				return err
			}
			defer func() {
				if closeErr := client.Close(); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("Runner cleanup failed: %s", redact.Error(closeErr)))
				}
			}()
			diagnostic, err := client.Diagnose(scratch)
			if err != nil {
				return err
			}
			if diagnostic.Warning != nil && *diagnostic.Warning != "" {
				warnings = append(warnings, *diagnostic.Warning)
			}
			catalog, err := client.Models(scratch)
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
			models = append(models, catalog...)
			diagnostics = append(diagnostics, diagnostic)
			return nil
		}()
		if checkErr != nil {
			errs = append(errs, backend.Display()+": "+checkErr.Error())
		}
	}
	if len(errs) > 0 {
		return nil, warnings, errors.New(strings.Join(errs, "; "))
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
		"warnings": warnings, "message": message, "sandbox": sandboxResult,
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Backend == config.BackendCodex {
			result["codex_version"] = diagnostic.Version
			result["tested_codex_version"] = runner.CodexVersion
			break
		}
	}
	return result, warnings, nil
}

func (a *App) ModelCatalog(backend config.Backend, binary string) (_ []runner.Model, err error) {
	if err := a.admitDiagnostic(); err != nil {
		return nil, err
	}
	defer a.wg.Done()
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
	scratch, discard, err := a.scratchWorkspace()
	if err != nil {
		return nil, err
	}
	defer discard()
	client, err := a.connectRunner(a.ctx, backend, cfg, scratch, "system")
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("Runner cleanup failed: %s", redact.Error(closeErr)))
		}
	}()
	return client.Models(scratch)
}

// Diagnostics own runner children and scratch roots even though they do not start
// model turns. Join their cleanup before shutdown can close the service store.
func (a *App) admitDiagnostic() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	if err := a.ctx.Err(); err != nil {
		return err
	}
	a.wg.Add(1)
	return nil
}

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
	prCapacity, err := a.prCapacity(cfg)
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
	} else if a.runtime.preflight != nil {
		mode := *a.runtime.preflight
		cycleMode = &mode
	}
	activeTasks := len(a.runtime.tasks)
	cycleActive := a.runtime.cycle != nil
	baselineActive := a.runtime.baseline != nil
	recoveryError := a.runtime.activeRecoveryError
	a.runtimeMu.Unlock()
	if !cycleActive {
		for _, raw := range snapshot.Cycles {
			var cycle struct {
				Mode   model.CycleMode `json:"mode"`
				Status string          `json:"status"`
			}
			if err := json.Unmarshal(raw, &cycle); err != nil {
				return nil, err
			}
			if cycle.Status != model.CycleRunning {
				continue
			}
			cycleActive = true
			if cycleMode == nil {
				mode := cycle.Mode
				cycleMode = &mode
			}
			break
		}
	}
	status := "idle"
	switch {
	case recoveryError != nil:
		status = "unhealthy"
	case cycleMode != nil && *cycleMode == model.CycleModeAudit:
		status = "auditing"
	case control.Paused:
		status = "paused"
	case control.Error != nil:
		status = "unhealthy"
	case cycleActive || activeTasks > 0:
		status = "running"
	}
	view, err := wirejson.GenericMap(snapshot)
	if err != nil {
		return nil, err
	}
	controlJSON, err := wirejson.GenericMap(control)
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
			"config_revision": latest.ConfigFingerprint,
			"config_matches":  baselineConfigMatches(latest, cfg),
			"revision_status": a.baselineRevisionStatus(latest, cfg),
		}
	}
	storage, _, err := a.Store.GetValue("settings", "storage")
	if err != nil {
		return nil, err
	}
	maps.Copy(view, map[string]any{
		"status":            status,
		"recovery_error":    recoveryError,
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
		"sandbox":           a.SandboxPosture(),
	})
	return view, nil
}
