package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// NotFound names a record or action the operator asked for that does not exist.
type NotFound string

func (e NotFound) Error() string { return string(e) }

type actionConflict struct{ msg string }

func (e *actionConflict) Error() string { return e.msg }

func conflictError(message string) error { return &actionConflict{message} }

func IsActionConflict(err error) bool {
	var c *actionConflict
	return errors.As(err, &c) || model.BlockedReasonFromError(err) != model.BlockedUnknown
}

var (
	errNotPaused       = conflictError("Octomus must be paused for this operation")
	errBusy            = conflictError("Octomus has active work")
	errRecoveryBlocked = conflictError("Wait for saved-state recovery to finish before starting new work.")
)

type capacityError struct {
	capacity model.PlanningCapacity
}

func (e *capacityError) Error() string { return e.capacity.Message() }
func (e *capacityError) Unwrap() error { return model.BlockedBudgetExhausted }

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
		a.withoutGate(func() { _, err = a.startAudit(a.ctx) })
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
		return nil, NotFound("Unknown control")
	}
	// Activity is best-effort after the operator change has committed.
	_ = a.Store.Event("system", "operator", action)
	body, err := wirejson.GenericMap(control)
	if err != nil {
		return nil, err
	}
	if action == "resume" {
		capacity, err := a.Store.PlanningCapacity(time.Now())
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

func (a *App) enterContinuous(control *model.Control) error {
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	if err := cfg.Validate(true); err != nil {
		return err
	}
	control.SetMode(model.OperatingModeContinuous)
	control.Error = nil
	control.NextCycleAt = 0
	if err := a.Store.SaveControl(*control); err != nil {
		return err
	}
	a.notify()
	return nil
}

func (a *App) startRunOnceBatch(control *model.Control) error {
	capacity, started, err := a.Store.StartBatch(control, time.Now())
	if err != nil {
		return err
	}
	if started {
		return nil
	}
	if err := capacity.EnsureAvailable(); err != nil {
		return err
	}
	return conflictError("Control state changed; try again")
}

func (a *App) startAudit(ctx context.Context) (string, error) {
	cfg, control, err := a.admitAuditPreflight()
	if err != nil {
		return "", err
	}
	defer a.wg.Done()
	preflightCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()

	if err := a.preflight(preflightCtx, cfg, true); err != nil {
		a.endPreflight()
		return "", err
	}

	a.gate.Lock()
	defer a.gate.Unlock()
	if err := preflightCtx.Err(); err != nil {
		a.endPreflight()
		return "", err
	}
	id, err := a.beginCycle(cfg, control, model.CycleModeAudit)
	if err != nil {
		a.endPreflight()
		return "", err
	}
	_ = a.Store.Event(id, "operator", "Audit started")
	return id, nil
}

func (a *App) admitAuditPreflight() (config.Config, model.Control, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	if err := a.ctx.Err(); err != nil {
		return config.Config{}, model.Control{}, err
	}
	if err := a.recoveryConflict(); err != nil {
		return config.Config{}, model.Control{}, err
	}
	a.runtimeMu.Lock()
	idle := a.runtime.idle()
	a.runtimeMu.Unlock()
	if !idle {
		return config.Config{}, model.Control{}, errBusy
	}
	cfg, err := a.Config()
	if err != nil {
		return config.Config{}, model.Control{}, err
	}
	if err := cfg.ValidateAudit(); err != nil {
		return config.Config{}, model.Control{}, err
	}
	control, err := a.Control()
	if err != nil {
		return config.Config{}, model.Control{}, err
	}
	if control.Mode != model.OperatingModePaused {
		return config.Config{}, model.Control{}, errNotPaused
	}
	capacity, err := a.Store.PlanningCapacity(time.Now())
	if err != nil {
		return config.Config{}, model.Control{}, err
	}
	if err := capacity.EnsureAvailable(); err != nil {
		return config.Config{}, model.Control{}, err
	}
	a.runtimeMu.Lock()
	a.runtime.startPreflight(model.CycleModeAudit)
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	return cfg, control, nil
}

// preflight checks the repository remote and every planning or execution route before a cycle starts.
func (a *App) preflight(ctx context.Context, cfg config.Config, audit bool) error {
	if err := gitops.ValidateRemote(ctx, cfg); err != nil {
		return fmt.Errorf("Repository remote preflight failed: %w", err)
	}
	clients := a.runners(ctx, cfg, "doctor")
	if err := a.validateRoutes(clients, cfg, audit); err != nil {
		_ = clients.Close()
		return fmt.Errorf("Runner route preflight failed: %w", err)
	}
	if err := clients.Close(); err != nil {
		return fmt.Errorf("Runner route preflight cleanup failed: %w", err)
	}
	return nil
}

func (a *App) beginCycle(cfg config.Config, expected model.Control, mode model.CycleMode) (string, error) {
	if err := a.ctx.Err(); err != nil {
		return "", err
	}
	if err := a.recoveryConflict(); err != nil {
		return "", err
	}
	a.runtimeMu.Lock()
	runtimeBusy := a.runtime.cycle != nil || len(a.runtime.tasks) > 0
	a.runtimeMu.Unlock()
	if runtimeBusy {
		return "", conflictError("Work started during planning preflight")
	}
	live, err := a.Control()
	if err != nil {
		return "", err
	}
	if !sameOperatorControl(live, expected) {
		return "", conflictError("Control state changed during planning preflight")
	}
	if mode == model.CycleModeExecution {
		if live.Mode == model.OperatingModeRunOnce {
			if live.Batch == nil || live.Batch.Phase != model.BatchPhaseDraining {
				return "", errors.New("Run once is not ready to plan")
			}
			pending, unresolved, countErr := a.Store.BatchCounts(live.Batch.ID)
			if countErr != nil {
				return "", countErr
			}
			if pending != 0 || unresolved != 0 {
				return "", errors.New("Run-once work changed during planning preflight")
			}
		} else {
			tasks, listErr := a.Store.SchedulingTasks(nil)
			if listErr != nil {
				return "", listErr
			}
			for _, task := range tasks {
				if task.Status == model.StatusQueued || task.Status.Active() {
					return "", errors.New("Queued work appeared during planning preflight")
				}
			}
		}
	}
	fingerprint, err := cfg.Fingerprint()
	if err != nil {
		return "", err
	}
	id := model.ID()
	next := live.Clone()
	next.CycleNumber++
	next.Error = nil
	if mode == model.CycleModeExecution && next.Mode == model.OperatingModeRunOnce {
		next.Batch.Phase = model.BatchPhasePlanning
		next.Batch.CycleID = &id
	}
	var runID *string
	if next.Mode == model.OperatingModeRunOnce && next.Batch != nil {
		runID = new(next.Batch.ID)
	}
	cycle := model.Cycle{
		Mode: mode, ID: id, Number: next.CycleNumber, Status: model.CycleRunning,
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: cfg.GitHubRepo, DecisionMemory: []model.DecisionRecord{}, RunID: runID,
	}
	capacity, started, err := a.Store.BeginCycle(cycle, next, live, fingerprint, time.Now())
	if err != nil {
		return "", err
	}
	if !started {
		if !capacity.Available() {
			return "", &capacityError{capacity: capacity}
		}
		return "", conflictError("Configuration or control state changed during planning preflight")
	}
	cycleCtx, cancel := context.WithCancel(a.ctx)
	a.runtimeMu.Lock()
	a.runtime.preflight = nil
	a.runtime.cycle = &cycleJob{id: id, mode: mode, cancel: cancel}
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		a.planCycle(cycleCtx, cfg, cycle)
	}()
	return id, nil
}

func sameOperatorControl(a, b model.Control) bool {
	a.ContextFingerprint, b.ContextFingerprint = "", ""
	a.IdleStreak, b.IdleStreak = 0, 0
	a.NextCycleAt, b.NextCycleAt = 0, 0
	return reflect.DeepEqual(a, b)
}

type SettingsView struct {
	Config            map[string]any            `json:"config"`
	Revision          string                    `json:"revision"`
	TransformedFields []redact.DisplayTransform `json:"transformed_fields"`
}

func settingsView(c config.Config) (*SettingsView, error) {
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
	return settingsView(c)
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
	return settingsView(c)
}
