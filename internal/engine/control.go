package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

var (
	ErrNotPaused = conflictError("Octomus must be paused for this operation")
	ErrBusy      = conflictError("Octomus has active work")
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
	capacity, started, err := a.Store.StartBatchIfAffordable(control, time.Now())
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

func (a *App) StartAudit(ctx context.Context) (string, error) {
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

func (a *App) admitAuditPreflight() (config.Config, model.Control, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	if err := a.ctx.Err(); err != nil {
		return config.Config{}, model.Control{}, err
	}
	if !a.runtimeIdle() {
		return config.Config{}, model.Control{}, ErrBusy
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
		return config.Config{}, model.Control{}, ErrNotPaused
	}
	capacity, err := a.Store.PlanningCapacity()
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

func (a *App) doctor(ctx context.Context, cfg config.Config, audit bool) error {
	if err := gitops.ValidateRemote(ctx, cfg); err != nil {
		return fmt.Errorf("Repository remote preflight failed: %w", err)
	}
	clients := a.runners(ctx, cfg, "doctor")
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
	a.runtime.preflight = nil
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
		value := next.Batch.ID
		runID = &value
	}
	cycle := model.Cycle{
		Mode: mode, ID: id, Number: next.CycleNumber, Status: model.CycleRunning,
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: cfg.GitHubRepo, DecisionMemory: []any{}, RunID: runID,
	}
	capacity, started, err := a.Store.BeginCycleIfAffordable(cycle, next, live, fingerprint, time.Now())
	if err != nil {
		return "", err
	}
	if !started {
		if !capacity.Available() {
			return "", &planningCapacityError{capacity: capacity}
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
