package engine

import (
	"fmt"
	"path/filepath"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func (a *App) Recover() error {
	a.gate.Lock()
	defer a.gate.Unlock()
	a.runtimeMu.Lock()
	a.runtime.cancelScanDone = false
	a.runtimeMu.Unlock()

	// Store transactions check the saved configuration, so it must carry the identity this deployment pins even when
	// OCTOMUS_GITHUB_REPO changed after the last save.
	saved, err := store.Get(a.store, store.ConfigKey)
	if err != nil {
		return err
	}
	if saved != nil {
		if pinned := a.deployment.pin(*saved); !wirejson.Equal(*saved, pinned) {
			if err := store.Put(a.store, store.ConfigKey, pinned); err != nil {
				return err
			}
			_ = a.store.Event("system", model.EventConfiguration, "Saved configuration now names the repository this deployment pins")
		}
	}

	// Scratch roots only ever hold a check that died with the previous process.
	if err := workspace.RemoveOwnedDir(a.dataDir, filepath.Join(a.dataDir, scratchDir)); err != nil {
		return err
	}
	if err := a.recoverBaselines(); err != nil {
		return err
	}
	candidates, err := a.store.ReservableTasks()
	if err != nil {
		return err
	}
	// Active tasks and unpublished output checkpoints keep their new-PR slot; a queued task
	// whose blocked attempt released its reservation waits for admission again.
	for _, task := range candidates {
		if task.Proposal.Target == task.Config.DefaultBranch && task.Status != model.StatusCancelled {
			if err := a.store.SeedPRReservation(task); err != nil {
				return err
			}
		}
	}

	tasks, err := a.store.ActiveTasks()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		markedCancelled, err := a.store.CancelMarked(task.ID)
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
		if err := a.checkpointTask(&task); err != nil {
			return err
		}
		_ = a.store.Event(task.ID, model.EventRecovery, *task.Error)
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
	cycles, err := a.store.RunningCycles(activeID)
	if err != nil {
		return err
	}
	for _, cycle := range cycles {
		model.InterruptRunning(cycle.Sessions)
		cycle.Status = model.CycleInterrupted
		cycle.CompletedAt = new(model.Now())
		cycle.Error = new(interruptedMsg)
		if err := store.Put(a.store, store.Cycles.Key(cycle.ID), cycle); err != nil {
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

// Caller holds the gate so a task cannot acquire or release its worker claim
// while recovery settles an active checkpoint left behind by a failed write.
func (a *App) blockOrphanedTasks() error {
	a.runtimeMu.Lock()
	excluded := a.ownedTaskIDs()
	a.runtimeMu.Unlock()
	for {
		tasks, err := a.store.ActiveTasksExcept(excluded)
		if err != nil {
			return err
		}
		for i := range tasks {
			if a.ctx.Err() != nil {
				return nil
			}
			cause := fmt.Errorf("Task worker exited unexpectedly; inspect the preserved workspace")
			if tasks[i].Status == model.StatusPublishing && tasks[i].OutputCommit != nil {
				cause = fmt.Errorf("Publication has no active worker; reconcile publication to check delivery: %w", model.BlockedPublicationUncertain)
			}
			model.FailRunning(tasks[i].Sessions, cause.Error())
			if err := a.setTaskError(&tasks[i], cause); err != nil {
				return err
			}
		}
		if len(tasks) < store.PageLimit {
			return nil
		}
	}
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
	tasks, err := a.store.CancelledWithLiveSessions(excluded)
	if err != nil {
		return err
	}
	for i := range tasks {
		model.InterruptRunning(tasks[i].Sessions)
		if err := a.checkpointTask(&tasks[i]); err != nil {
			return err
		}
	}
	// A new service scans durable history once. Worker exit and cleanup release
	// invalidate this check after an excluded owner finishes; failed writes and
	// full pages stay retryable without scanning finalized history every tick.
	if len(tasks) < store.PageLimit {
		a.runtimeMu.Lock()
		a.runtime.cancelScanDone = true
		a.runtimeMu.Unlock()
	}
	return nil
}

func (a *App) recoverBaselines() error {
	a.runtimeMu.Lock()
	active := a.runtime.baseline
	a.runtimeMu.Unlock()
	checks, err := a.store.RunningBaselines()
	if err != nil {
		return err
	}
	for i := range checks {
		if active != nil && active.id == checks[i].ID {
			continue
		}
		if err := a.abandonBaseline(&checks[i],
			"The operator cancelled this check before the service stopped",
			"The service stopped while the baseline check was running"); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) abandonBaseline(check *model.BaselineCheck, cancelled, interrupted string) error {
	marked, err := a.store.BaselineCancelMarked(check.ID)
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
	return store.Put(a.store, store.Baselines.Key(check.ID), *check)
}
