package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
)

var (
	ErrNotPaused = errors.New("Octomus must be paused for this operation")
	ErrBusy      = errors.New("Octomus has active work")
)

type planningCapacityError struct {
	capacity model.PlanningCapacity
}

func (e *planningCapacityError) Error() string { return e.capacity.Message() }
func (e *planningCapacityError) Unwrap() error { return model.BlockedReasonBudgetExhausted }

func (a *App) runtimeIdle() bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	return a.runtime.idle()
}

// Pause durably prevents new work and invalidates process-local remote
// observations. Already-running task workers retain their durable evidence.
func (a *App) Pause() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	control, err := a.Control()
	if err != nil {
		return err
	}
	control.SetMode(model.OperatingModePaused)
	if err := a.Store.SaveControl(control); err != nil {
		return err
	}
	a.invalidatePrObservation()
	return a.Store.Event("system", "operator", "Paused")
}

// Resume enters durable Continuous mode after validating all execution and
// planning policy. An audit remains isolated from queue execution.
func (a *App) Resume() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	a.runtimeMu.Lock()
	busyAudit := a.runtime.cycle != nil && a.runtime.cycle.mode == model.CycleModeAudit || a.runtime.preflight && a.runtime.preflightMode == model.CycleModeAudit
	a.runtimeMu.Unlock()
	if busyAudit {
		return errors.New("Cannot resume while an audit is running")
	}
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	if err := cfg.Validate(true); err != nil {
		return err
	}
	control, err := a.Control()
	if err != nil {
		return err
	}
	control.SetMode(model.OperatingModeContinuous)
	control.Error = nil
	control.NextCycleAt = 0
	if err := a.Store.SaveControl(control); err != nil {
		return err
	}
	a.notify()
	return a.Store.Event("system", "operator", "Continuous mode started")
}

// RunOnce creates a durable membership snapshot only if a complete planning
// pass is affordable in the same database transaction.
func (a *App) RunOnce() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	if !a.runtimeIdle() {
		return ErrBusy
	}
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	if err := cfg.Validate(true); err != nil {
		return err
	}
	control, err := a.Control()
	if err != nil {
		return err
	}
	if control.Mode != model.OperatingModePaused {
		return ErrNotPaused
	}
	capacity, started, err := a.Store.StartBatchIfAffordable(&control, time.Now())
	if err != nil {
		return err
	}
	if !started {
		return capacity.EnsureAvailable()
	}
	a.notify()
	return a.Store.Event("system", "operator", "Run once started")
}

// StartAudit validates remote and route availability outside gate, then
// atomically starts an audit only if paused state and policy are unchanged.
func (a *App) StartAudit(ctx context.Context) (string, error) {
	a.gate.Lock()
	if err := a.ctx.Err(); err != nil {
		a.gate.Unlock()
		return "", err
	}
	if !a.runtimeIdle() {
		a.gate.Unlock()
		return "", ErrBusy
	}
	cfg, err := a.Config()
	if err != nil {
		a.gate.Unlock()
		return "", err
	}
	if err := cfg.ValidateAudit(); err != nil {
		a.gate.Unlock()
		return "", err
	}
	control, err := a.Control()
	if err != nil {
		a.gate.Unlock()
		return "", err
	}
	if control.Mode != model.OperatingModePaused {
		a.gate.Unlock()
		return "", ErrNotPaused
	}
	capacity, err := a.Store.PlanningCapacity()
	if err != nil {
		a.gate.Unlock()
		return "", err
	}
	if err := capacity.EnsureAvailable(); err != nil {
		a.gate.Unlock()
		return "", err
	}
	a.runtimeMu.Lock()
	a.runtime.preflight = true
	a.runtime.preflightMode = model.CycleModeAudit
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	a.gate.Unlock()
	defer a.wg.Done()
	preflightCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	defer func() {
		stop()
		cancel()
	}()

	if err := a.doctor(preflightCtx, cfg, true); err != nil {
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

func (a *App) doctor(ctx context.Context, cfg config.Config, audit bool) error {
	if err := gitops.ValidateRemote(ctx, cfg); err != nil {
		return fmt.Errorf("Repository remote preflight failed: %w", err)
	}
	clients := runner.New(ctx, cfg, a.Store, "doctor")
	if err := clients.ValidateRoutes(cfg, cfg.Repository, audit); err != nil {
		_ = clients.Close()
		return fmt.Errorf("Runner route preflight failed: %w", err)
	}
	if err := clients.Close(); err != nil {
		return fmt.Errorf("Runner route preflight cleanup failed: %w", err)
	}
	return nil
}

func (a *App) endPreflight() {
	a.runtimeMu.Lock()
	a.runtime.preflight = false
	a.runtime.preflightMode = model.CycleModeExecution
	a.runtimeMu.Unlock()
	a.notify()
}

func (a *App) beginCycle(cfg config.Config, expected model.Control, mode model.CycleMode) (string, error) {
	if err := a.ctx.Err(); err != nil {
		return "", err
	}
	a.runtimeMu.Lock()
	runtimeBusy := a.runtime.cycle != nil || len(a.runtime.tasks) > 0
	a.runtimeMu.Unlock()
	if runtimeBusy {
		return "", errors.New("Work started during planning preflight")
	}
	live, err := a.Control()
	if err != nil {
		return "", err
	}
	if !controlsEqual(live, expected) {
		return "", errors.New("Control state changed during planning preflight")
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
	next := cloneControl(live)
	next.CycleNumber++
	next.Error = nil
	if mode == model.CycleModeExecution && next.Mode == model.OperatingModeRunOnce {
		next.Batch.Phase = model.BatchPhasePlanning
		next.Batch.CycleID = &id
	}
	var runID *string
	if next.Mode == model.OperatingModeRunOnce && next.Batch != nil {
		value := next.Batch.ID
		runID = &value
	}
	cycle := model.Cycle{
		Mode: mode, ID: id, Number: next.CycleNumber, Status: "running",
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: cfg.GitHubRepo, DecisionMemory: []any{}, RunID: runID,
	}
	capacity, started, err := a.Store.BeginCycleIfAffordable(cycle, next, expected, fingerprint, time.Now())
	if err != nil {
		return "", err
	}
	if !started {
		if !capacity.Available() {
			return "", &planningCapacityError{capacity: capacity}
		}
		return "", errors.New("Configuration or control state changed during planning preflight")
	}
	cycleCtx, cancel := context.WithCancel(a.ctx)
	a.runtimeMu.Lock()
	a.runtime.preflight = false
	a.runtime.preflightMode = model.CycleModeExecution
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

func controlsEqual(a, b model.Control) bool {
	if a.Paused != b.Paused || a.CycleNumber != b.CycleNumber || a.NextCycleAt != b.NextCycleAt || a.Mode != b.Mode || a.IdleStreak != b.IdleStreak || a.ContextFingerprint != b.ContextFingerprint {
		return false
	}
	if (a.Error == nil) != (b.Error == nil) || a.Error != nil && *a.Error != *b.Error {
		return false
	}
	if (a.Batch == nil) != (b.Batch == nil) {
		return false
	}
	if a.Batch != nil {
		if a.Batch.ID != b.Batch.ID || a.Batch.Phase != b.Batch.Phase || (a.Batch.CycleID == nil) != (b.Batch.CycleID == nil) || a.Batch.CycleID != nil && *a.Batch.CycleID != *b.Batch.CycleID {
			return false
		}
	}
	return true
}

func cloneControl(control model.Control) model.Control {
	copy := control
	if control.Error != nil {
		value := *control.Error
		copy.Error = &value
	}
	if control.Batch != nil {
		batch := control.Batch.Clone()
		copy.Batch = &batch
	}
	return copy
}
