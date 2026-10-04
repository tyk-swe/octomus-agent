package engine

import (
	"fmt"
	"path/filepath"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func (a *App) Recover() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	a.runtimeMu.Lock()
	a.runtime.cancelScanDone = false
	a.runtimeMu.Unlock()

	// Scratch roots only ever hold a check that died with the previous process.
	if err := workspace.RemoveOwnedDir(a.dataDir, filepath.Join(a.dataDir, scratchDir)); err != nil {
		return err
	}
	if err := a.recoverBaselines(); err != nil {
		return err
	}
	candidates, err := a.Store.ReservableTasks()
	if err != nil {
		return err
	}
	for _, task := range candidates {
		initializedQueued := task.Status == model.StatusQueued && workspace.Initialized(task)
		needsReservation := task.Status.Active() || initializedQueued || (task.Status != model.StatusPublished && task.OutputCommit != nil)
		if task.Proposal.Target == task.Config.DefaultBranch && task.Status != model.StatusCancelled && needsReservation {
			if err := a.Store.SeedPRReservation(task); err != nil {
				return err
			}
		}
	}

	active := make([]string, 0, len(model.ActiveStatuses()))
	for _, status := range model.ActiveStatuses() {
		active = append(active, status.String())
	}
	tasks, err := a.Store.TasksWithStatus(active)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		markedCancelled, err := a.Store.Marked("cancel", task.ID)
		if err != nil {
			return err
		}
		model.InterruptRunning(task.Sessions)
		switch {
		case markedCancelled && task.OutputCommit == nil:
			task.Status = model.StatusCancelled
			task.Error = new("Operator cancellation preserved across restart")
		case workspace.Initialized(task) && task.Attempts < task.ExecutionConfig().MaxRetries:
			task.Status = model.StatusQueued
			task.Attempts++
			task.Error = new("Recovering an interrupted task: inspecting the recorded workspace and reconciling remote state before continuing.")
		default:
			task.Status = model.StatusBlocked
			reason := model.BlockedWorkspaceInvalid
			message := "Service interrupted before workspace initialization completed, or retry budget exhausted. Inspect the preserved task before retrying."
			if workspace.Initialized(task) {
				reason = model.BlockedRetryLimit
				if task.OutputCommit != nil {
					reason = model.BlockedPublicationUncertain
					message = "Service interrupted during publication after the retry budget was exhausted. Reconcile publication to finish delivery without another model turn."
				}
			}
			task.BlockedReason = &reason
			task.Error = new(message)
		}
		task.UpdatedAt = model.Now()
		if err := a.Store.Put("task", task.ID, task); err != nil {
			return err
		}
		if err := a.Store.Event(task.ID, "recovery", *task.Error); err != nil {
			return err
		}
	}

	if err := a.settleCancelledSessions(); err != nil {
		return err
	}
	return a.interruptOrphanedCycles()
}

// Caller holds the gate so worker ownership cannot change during recovery.
func (a *App) interruptOrphanedCycles() error {
	a.runtimeMu.Lock()
	activeID := ""
	if a.runtime.cycle != nil {
		activeID = a.runtime.cycle.id
	}
	a.runtimeMu.Unlock()
	cycles, err := a.Store.RunningCyclesExcept(activeID)
	if err != nil {
		return err
	}
	for _, cycle := range cycles {
		model.InterruptRunning(cycle.Sessions)
		cycle.Status = model.CycleInterrupted
		cycle.CompletedAt = new(model.Now())
		cycle.Error = new(interruptedMsg)
		if err := a.Store.Put("cycle", cycle.ID, cycle); err != nil {
			return err
		}
	}
	if activeID != "" {
		return nil
	}
	// Retry control settlement even if an earlier pass interrupted the cycle
	// successfully but could not pause its now-workerless planning batch.
	control, err := a.Control()
	if err != nil {
		return err
	}
	if control.Mode == model.OperatingModeRunOnce && control.Batch != nil && control.Batch.Phase == model.BatchPhasePlanning {
		message := "Run once was interrupted before its planning transaction committed"
		return a.pauseLocked(&control, &message)
	}
	return nil
}

// Caller holds the gate so a publication cannot acquire or release its worker
// claim while recovery checks ownership and preserves its checkpoint.
func (a *App) blockOrphanedPublications() error {
	a.runtimeMu.Lock()
	excluded := a.ownedTaskIDs()
	a.runtimeMu.Unlock()
	tasks, err := a.Store.PublishingTasksExcept(excluded)
	if err != nil {
		return err
	}
	for i := range tasks {
		if a.ctx.Err() != nil {
			return nil
		}
		err := fmt.Errorf("Publication has no active worker; reconcile publication to check delivery: %w", model.BlockedPublicationUncertain)
		if err := a.setTaskError(&tasks[i], err); err != nil {
			return err
		}
	}
	return nil
}

// Cancellation commits its terminal status before the worker joins and saves its
// final session evidence. A refused final write must not leave a running session
// permanently attached to a cancelled task, including after archive or restart.
// Caller holds the gate while checking ownership and updating the saved evidence.
func (a *App) settleCancelledSessions() error {
	a.runtimeMu.Lock()
	if a.runtime.cancelScanDone {
		a.runtimeMu.Unlock()
		return nil
	}
	excluded := a.ownedTaskIDs()
	a.runtimeMu.Unlock()
	tasks, err := a.Store.CancelledWithLiveSessions(excluded)
	if err != nil {
		return err
	}
	for i := range tasks {
		model.InterruptRunning(tasks[i].Sessions)
		if err := a.saveTask(&tasks[i]); err != nil {
			return err
		}
	}
	// A new service scans durable history once. Worker exit and cleanup release
	// invalidate this check after an excluded owner finishes; failed writes and
	// full pages stay retryable without scanning finalized history every tick.
	if len(tasks) < 500 {
		a.runtimeMu.Lock()
		a.runtime.cancelScanDone = true
		a.runtimeMu.Unlock()
	}
	return nil
}

func (a *App) recoverBaselines() error {
	checks, err := a.Store.RunningBaselines()
	if err != nil {
		return err
	}
	for i := range checks {
		if err := a.abandonBaseline(&checks[i],
			"The operator cancelled this check before the service stopped",
			"The service stopped while the baseline check was running"); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) abandonBaseline(check *model.BaselineCheck, cancelled, interrupted string) error {
	marked, err := a.Store.Marked("baseline_cancel", check.ID)
	if err != nil {
		return err
	}
	check.Status = model.BaselineStatusInterrupted
	message := interrupted
	if marked {
		check.Status = model.BaselineStatusCancelled
		message = cancelled
	}
	check.CompletedAt = new(model.Now())
	check.Error = &message
	return a.Store.Put("baseline", check.ID, *check)
}
