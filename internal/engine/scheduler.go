package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func idleDelay(base uint64, streak uint32) uint64 {
	ceiling := uint64(86400)
	if base > ceiling {
		ceiling = base
	}
	if streak <= 1 {
		return base
	}
	shift := streak - 1
	if shift > 16 {
		shift = 16
	}
	if base > math.MaxUint64>>shift {
		return ceiling
	}
	delay := base << shift
	if delay > ceiling {
		return ceiling
	}
	return delay
}

func (a *App) tick() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	if a.ctx.Err() != nil {
		return nil
	}
	// A failed terminal write can outlive its planning worker, including while paused.
	if err := a.interruptOrphanedCycles(); err != nil {
		return a.blockRecovery(err)
	}
	if err := a.blockOrphanedPublications(); err != nil {
		return a.blockRecovery(err)
	}
	if err := a.settleCancelledSessions(); err != nil {
		return a.blockRecovery(err)
	}
	// Reset only after every maintenance recovery step succeeds.
	a.recoveryLog = recoveryActivity{}
	a.runtimeMu.Lock()
	a.runtime.activeRecoveryError = nil
	a.runtimeMu.Unlock()
	control, err := a.Control()
	if err != nil {
		return err
	}
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	if a.ctx.Err() != nil {
		return nil
	}
	a.startHousekeeping(cfg)
	a.runtimeMu.Lock()
	reconciling := a.runtime.reconciling
	baseline := a.runtime.baseline != nil
	a.runtimeMu.Unlock()
	if reconciling {
		return nil
	}
	// A running baseline check owns the whole service: no cycle, dispatch or batch completion may proceed.
	if baseline {
		return nil
	}
	if control.Mode == model.OperatingModePaused {
		return nil
	}
	if err := a.sandboxReady(); err != nil {
		return err
	}
	if a.ctx.Err() != nil {
		return nil
	}
	if err := cfg.Validate(true); err != nil {
		return err
	}
	a.runtimeMu.Lock()
	planning := a.runtime.planning()
	a.runtimeMu.Unlock()
	if planning {
		return nil
	}

	var runID *string
	if control.Mode == model.OperatingModeRunOnce {
		if control.Batch == nil {
			return errors.New("Run once is missing its durable batch")
		}
		id := control.Batch.ID
		runID = &id
		pending, unresolved, err := a.Store.BatchCounts(id)
		if err != nil {
			return err
		}
		if pending == 0 && (control.Batch.Phase == model.BatchPhaseExecuting || unresolved > 0) {
			return a.finishRunOnce(control, unresolved)
		}
	}

	tasks, err := a.Store.SchedulingTasks(runID)
	if err != nil {
		return err
	}
	if a.ctx.Err() != nil {
		return nil
	}
	changed, err := a.validateQueuedCycles(tasks)
	if err != nil {
		return err
	}
	if changed {
		// Removing queued rows can expose a new window that has not been
		// prepared. Release the gate before preparing that window on the next
		// pass; do not dispatch it or start planning in between.
		a.notify()
		return nil
	}
	started, waiting, err := a.dispatch(cfg, control, tasks)
	if err != nil {
		return err
	}
	if a.ctx.Err() != nil || started || waiting {
		return nil
	}

	if control.Mode == model.OperatingModeRunOnce {
		pending, unresolved, err := a.Store.BatchCounts(control.Batch.ID)
		if err != nil {
			return err
		}
		if pending == 0 && unresolved > 0 {
			return a.finishRunOnce(control, unresolved)
		}
		if control.Batch.Phase != model.BatchPhaseDraining || pending != 0 {
			return nil
		}
		return a.startPlanning(cfg, control)
	}
	if control.Mode == model.OperatingModeContinuous && time.Now().Unix() >= control.NextCycleAt {
		return a.startPlanning(cfg, control)
	}
	return nil
}

func (a *App) pauseLocked(control *model.Control, message *string) error {
	control.SetMode(model.OperatingModePaused)
	if message != nil {
		control.Error = message
	}
	if err := a.Store.SaveControl(*control); err != nil {
		return err
	}
	a.invalidatePRs()
	return nil
}

func (a *App) finishRunOnce(control model.Control, unresolved uint64) error {
	if a.ctx.Err() != nil {
		return nil
	}
	message := "Run once completed; new work paused"
	if unresolved > 0 {
		message = "Run once finished with unresolved work"
	}
	if err := a.pauseLocked(&control, nil); err != nil {
		return err
	}
	return a.Store.Event("system", "run_complete", message)
}

func (a *App) startPlanning(cfg config.Config, control model.Control) error {
	if a.ctx.Err() != nil {
		return nil
	}
	a.runtimeMu.Lock()
	if !a.runtime.idle() {
		a.runtimeMu.Unlock()
		return nil
	}
	a.runtimeMu.Unlock()
	capacity, err := a.Store.PlanningCapacity()
	if err != nil {
		return err
	}
	if a.ctx.Err() != nil {
		return nil
	}
	if !capacity.Available() {
		return a.waitForCapacity(control, capacity)
	}
	a.runtimeMu.Lock()
	if a.ctx.Err() != nil {
		a.runtimeMu.Unlock()
		return nil
	}
	a.runtime.startPreflight(model.CycleModeExecution)
	a.runtimeMu.Unlock()
	snapshot := cfg.Clone()
	expected := control.Clone()
	a.wg.Go(func() {
		err := a.preflight(a.ctx, snapshot, false)
		a.gate.Lock()
		defer a.gate.Unlock()
		defer a.notify()
		if err == nil {
			_, err = a.beginCycle(snapshot, expected, model.CycleModeExecution)
		}
		if err == nil {
			return
		}
		a.endPreflight()
		if a.ctx.Err() != nil || errors.Is(err, errRecoveryBlocked) {
			return
		}
		live, loadErr := a.Control()
		if loadErr != nil || !sameOperatorControl(live, expected) {
			return
		}
		var capacityErr *capacityError
		switch {
		case errors.As(err, &capacityErr):
			_ = a.waitForCapacity(live, capacityErr.capacity)
		case live.Mode == model.OperatingModeRunOnce:
			message := redact.Error(err)
			_ = a.pauseLocked(&live, &message)
			_ = a.Store.Event("system", "planning_error", message)
		case live.Mode == model.OperatingModeContinuous:
			message := redact.Error(err)
			live.Error = &message
			live.NextCycleAt = time.Now().Unix() + int64(cfg.CycleIntervalSeconds)
			_ = a.Store.SaveControl(live)
			_ = a.Store.Event("system", "planning_error", message)
		}
	})
	return nil
}

func (a *App) waitForCapacity(control model.Control, capacity model.PlanningCapacity) error {
	if a.ctx.Err() != nil {
		return nil
	}
	if control.Mode == model.OperatingModeContinuous {
		control.Error = nil
		control.NextCycleAt = capacity.NextResetAt
		return a.Store.SaveControl(control)
	}
	message := capacity.Message()
	if err := a.pauseLocked(&control, &message); err != nil {
		return err
	}
	return a.Store.Event("system", "planning_capacity", message)
}

func (a *App) validateQueuedCycles(tasks []model.Task) (bool, error) {
	visibleCycles := map[string]struct{}{}
	for _, task := range tasks {
		visibleCycles[task.CycleID] = struct{}{}
	}
	a.runtimeMu.Lock()
	for cycleID := range a.runtime.checkedCycles {
		if _, visible := visibleCycles[cycleID]; !visible {
			delete(a.runtime.checkedCycles, cycleID)
		}
	}
	a.runtimeMu.Unlock()
	queuedCycles := map[string][]model.Task{}
	for _, task := range tasks {
		if task.Status == model.StatusQueued {
			queuedCycles[task.CycleID] = append(queuedCycles[task.CycleID], task)
		}
	}
	changed := false
	for cycleID, cycleTasks := range queuedCycles {
		if a.ctx.Err() != nil {
			return changed, nil
		}
		a.runtimeMu.Lock()
		_, checked := a.runtime.checkedCycles[cycleID]
		a.runtimeMu.Unlock()
		if !checked {
			var err error
			cycleTasks, err = a.Store.TasksForCycle(cycleID)
			if err != nil {
				return changed, err
			}
		}
		// Cancellation must precede invalid-plan writes, including for queued
		// members outside the scheduling window. Checked cycles still need this
		// pass when an operator cancellation was only partially persisted.
		cancelled, err := a.cancelQueuedTasks(cycleTasks)
		changed = changed || cancelled
		if err != nil {
			return changed, err
		}
		if checked {
			continue
		}
		deferred := false
		if err := validateTaskPlan(cycleTasks); err != nil {
			for i := range cycleTasks {
				if a.ctx.Err() != nil {
					return changed, nil
				}
				if cycleTasks[i].Status == model.StatusQueued {
					if a.cleanupClaimed(cleanupTask, cycleTasks[i].ID) {
						deferred = true
						continue
					}
					changed = true
					if blockErr := a.setTaskError(&cycleTasks[i], invalidPlan(err.Error())); blockErr != nil {
						return changed, blockErr
					}
				}
			}
		}
		if !deferred {
			a.runtimeMu.Lock()
			a.runtime.checkedCycles[cycleID] = struct{}{}
			a.runtimeMu.Unlock()
		}
	}
	return changed, nil
}

func invalidPlan(message string) error {
	return fmt.Errorf("%s: %w", message, model.BlockedInvalidPlan)
}

func (a *App) cancelQueuedTasks(tasks []model.Task) (bool, error) {
	changed := false
	for i := range tasks {
		if a.ctx.Err() != nil {
			return changed, nil
		}
		task := &tasks[i]
		if task.Status != model.StatusQueued || task.OutputCommit != nil || a.cleanupClaimed(cleanupTask, task.ID) {
			continue
		}
		marked, err := a.Store.Marked("cancel", task.ID)
		if err != nil {
			return changed, err
		}
		if a.ctx.Err() != nil {
			return changed, nil
		}
		if !marked {
			continue
		}
		cancelled, err := a.Store.CancelTask(task.ID)
		if err != nil {
			return changed, err
		}
		if cancelled {
			changed = true
			task.Status = model.StatusCancelled
			if err := a.Store.Event(task.ID, "operator", "cancel"); err != nil {
				return changed, err
			}
		}
	}
	return changed, nil
}

func (a *App) dispatch(cfg config.Config, control model.Control, tasks []model.Task) (bool, bool, error) {
	activeByBranch := map[string]struct{}{}
	activeTasks := map[string]struct{}{}
	activeCount := uint64(0)
	for _, task := range tasks {
		if task.Status.Active() {
			activeTasks[task.ID] = struct{}{}
			activeCount++
			activeByBranch[task.Branch] = struct{}{}
		}
	}
	a.runtimeMu.Lock()
	for id, task := range a.runtime.tasks {
		if _, active := activeTasks[id]; !active {
			activeCount++
		}
		activeByBranch[task.branch] = struct{}{}
	}
	a.runtimeMu.Unlock()
	available := uint64(0)
	if cfg.ExecutionConcurrency > activeCount {
		available = cfg.ExecutionConcurrency - activeCount
	}
	started := false
	waiting := false
	var inventory *model.OpenPRInventory
	inventoryChecked := false
	refreshRequested := false
	requestRefresh := func() {
		if !refreshRequested && a.ctx.Err() == nil {
			refreshRequested = true
			a.startPRRefresh(cfg)
		}
	}
	for i := range tasks {
		if a.ctx.Err() != nil {
			return started, waiting, nil
		}
		task := &tasks[i]
		if task.Status != model.StatusQueued {
			continue
		}
		if a.cleanupClaimed(cleanupTask, task.ID) {
			waiting = true
			continue
		}
		if available == 0 {
			waiting = true
			break
		}
		ready, blocked, err := a.dependenciesReady(*task, control)
		if err != nil {
			return started, waiting, err
		}
		if a.ctx.Err() != nil {
			return started, waiting, nil
		}
		if blocked != nil {
			if err := a.setTaskError(task, blocked); err != nil {
				return started, waiting, err
			}
			continue
		}
		if !ready {
			waiting = true
			continue
		}
		if _, busy := activeByBranch[task.Branch]; busy {
			waiting = true
			continue
		}
		reservation, err := a.Store.HasPRReservation(task.ID)
		if err != nil {
			return started, waiting, err
		}
		if a.ctx.Err() != nil {
			return started, waiting, nil
		}
		admitted := false
		if task.Proposal.Target == task.Config.DefaultBranch && !reservation {
			if !inventoryChecked {
				inventory, _ = a.claimInventory(cfg, time.Now())
				inventoryChecked = true
			}
			if inventory == nil {
				requestRefresh()
				waiting = true
				continue
			}
			if a.ctx.Err() != nil {
				return started, waiting, nil
			}
			admitted, err = a.Store.AdmitNewPRTask(task, *inventory)
			if err != nil {
				return started, waiting, err
			}
			a.runtimeMu.Lock()
			a.runtime.prAdmissionRefused = !admitted
			a.runtimeMu.Unlock()
			if !admitted {
				requestRefresh()
				waiting = true
				continue
			}
		} else if err := a.transition(task, model.StatusExecuting); err != nil {
			return started, waiting, err
		}
		activeByBranch[task.Branch] = struct{}{}
		available--
		started = true
		// A durable admission that already began still needs its joined worker.
		a.runTask(*task)
	}
	return started, waiting, nil
}

func (a *App) dependenciesReady(task model.Task, control model.Control) (bool, error, error) {
	for _, id := range task.Proposal.Dependencies {
		dependency, err := store.Get[model.Task](a.Store, "task", id)
		if err != nil {
			return false, nil, err
		}
		if dependency == nil {
			return false, fmt.Errorf("Dependency %s is missing: %w", id, model.BlockedDependencyBlocked), nil
		}
		if dependency.Status == model.StatusPublished {
			continue
		}
		if control.Mode == model.OperatingModeRunOnce && (dependency.RunID == nil || task.RunID == nil || *dependency.RunID != *task.RunID) {
			return false, fmt.Errorf("Dependency %s is outside this run-once batch: %w", id, model.BlockedDependencyBlocked), nil
		}
		if dependency.Status == model.StatusBlocked || dependency.Status == model.StatusFailed || dependency.Status == model.StatusCancelled {
			return false, fmt.Errorf("Dependency %s is unresolved: %w", id, model.BlockedDependencyBlocked), nil
		}
		return false, nil, nil
	}
	return true, nil, nil
}

func (a *App) runTask(task model.Task) {
	ctx, cancel := context.WithCancel(a.ctx)
	a.runtimeMu.Lock()
	a.runtime.tasks[task.ID] = taskJob{branch: task.Branch, cancel: cancel}
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		var runErr error
		func() {
			defer func() {
				if panicked := recover(); panicked != nil {
					runErr = fmt.Errorf("Task worker panicked: %v", panicked)
				}
			}()
			runErr = a.supervise(ctx, task.Clone())
		}()
		a.gate.Lock()
		current, loadErr := store.Get[model.Task](a.Store, "task", task.ID)
		interrupted := a.ctx.Err() != nil && current != nil && workspace.Initialized(*current)
		if loadErr == nil && current != nil && current.Status.Active() && !interrupted {
			if runErr == nil || errors.Is(runErr, context.Canceled) {
				runErr = errors.New("Task worker exited unexpectedly; inspect the preserved workspace")
			}
			a.settleExitedTask(current, runErr)
		} else if loadErr != nil {
			// The final read could not establish whether a checkpoint remains.
			// A successful recovery pass can safely clear this barrier.
			a.setRecoveryError(loadErr)
		}
		a.runtimeMu.Lock()
		delete(a.runtime.tasks, task.ID)
		if loadErr != nil || (current != nil && current.Status == model.StatusCancelled) {
			a.runtime.cancelScanDone = false
		}
		a.runtimeMu.Unlock()
		a.gate.Unlock()
		a.notify()
	}()
}
