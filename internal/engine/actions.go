package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func (a *App) CycleAction(id, action string) error {
	if action != "archive" && action != "discard" {
		return NotFound("Unknown cycle action")
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
		return NotFound("Cycle not found")
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
		cycle.Lifecycle.ArchivedAt = new(model.Now())
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

func (a *App) TaskAction(id, action string) error {
	switch action {
	case "cancel", "retry", "supersede", "archive", "discard", "reconcile":
	default:
		return NotFound("Unknown task action")
	}
	a.gate.Lock()
	if err := a.ctx.Err(); err != nil {
		a.gate.Unlock()
		return err
	}
	a.wg.Add(1)
	defer a.wg.Done()
	defer a.gate.Unlock()
	task, err := a.eligibleTask(id, action)
	if err != nil {
		return err
	}
	if action == "reconcile" {
		return a.reconcileLocked(id, task)
	}
	var actionErr error
	switch action {
	case "cancel":
		actionErr = a.cancelTask(id)
	case "retry":
		actionErr = a.retryTask(task)
	case "supersede":
		task.Status = model.StatusCancelled
		task.RediscoveryRequested = true
		task.RediscoveryResult = nil
		if actionErr = a.Store.ClearCancel(id); actionErr == nil {
			actionErr = a.saveTask(task)
		}
	case "archive":
		if task.Status.Retryable() {
			task.Status = model.StatusCancelled
		}
		task.Lifecycle.ArchivedAt = new(model.Now())
		actionErr = a.saveTask(task)
	case "discard":
		actionErr = a.discardTask(task)
	}
	if actionErr == nil {
		if action == "cancel" || task.Status == model.StatusCancelled {
			a.runtimeMu.Lock()
			a.runtime.cancelScanDone = false
			a.runtimeMu.Unlock()
		}
		_ = a.Store.Event(id, "operator", action)
	}
	a.notify()
	return actionErr
}

func (a *App) cancelTask(id string) error {
	a.runtimeMu.Lock()
	job, running := a.runtime.tasks[id]
	a.runtimeMu.Unlock()
	if running {
		job.cancel()
	}
	if err := a.Store.MarkCancel(id); err != nil {
		return err
	}
	cancelled, err := a.Store.CancelTask(id)
	if err != nil {
		return err
	}
	if !cancelled {
		return conflictError("Publication has started; inspect the task and reconcile unfinished publication.")
	}
	return nil
}

func (a *App) retryTask(task *model.Task) error {
	if !task.Status.Retryable() {
		return conflictError("Only failed or blocked tasks can be retried.")
	}
	cfg, err := a.Config()
	if err != nil {
		return err
	}
	if task.Attempts >= cfg.MaxRetries {
		return conflictError("Retry limit reached. Adjust the operating limit after inspecting the failure.")
	}
	original := task.Clone()
	policy := model.PolicyOf(cfg)
	task.AttemptPolicy = &policy
	if task.OutputCommit == nil {
		var preflightErr error
		a.withoutGate(func() { preflightErr = a.retryPreflight(a.ctx, task) })
		if err := a.revalidateTaskAction(&original, "retry"); err != nil {
			return err
		}
		live, err := a.Config()
		if err != nil {
			return err
		}
		livePolicy := model.PolicyOf(live)
		if task.AttemptPolicy == nil || *task.AttemptPolicy != livePolicy {
			return conflictError("Retry policy changed during remote checks; try again with the current limits")
		}
		if preflightErr != nil {
			recordTaskError(task, preflightErr)
			_ = a.saveTask(task)
			return preflightErr
		}
	}
	task.Attempts++
	task.ReviewBaseline = uint64(len(task.Reviews))
	task.RepairRounds = new(uint64(0))
	task.RepairProgress = nil
	task.Error = nil
	task.BlockedReason = nil
	task.Status = model.StatusQueued
	task.RunID = nil
	policyJSON, err := wirejson.Marshal(task.AttemptPolicy)
	if err != nil {
		return err
	}
	if err := a.Store.Event(task.ID, "attempt_policy", string(policyJSON)); err != nil {
		return err
	}
	if err := a.Store.ClearCancel(task.ID); err != nil {
		return err
	}
	return a.saveTask(task)
}

func (a *App) eligibleTask(id, action string) (*model.Task, error) {
	task, err := store.Get[model.Task](a.Store, "task", id)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, NotFound("Task not found")
	}
	if !slices.Contains(task.AllowedActions(), action) {
		return nil, conflictError("This action is not eligible for the task's recorded failure and workspace state")
	}
	if a.cleanupClaimed(cleanupTask, id) {
		return nil, conflictError("Workspace cleanup is in progress for this task; wait for it to finish")
	}
	if action != "cancel" {
		a.runtimeMu.Lock()
		_, running := a.runtime.tasks[id]
		a.runtimeMu.Unlock()
		if running {
			return nil, conflictError("Wait for active task work to finish")
		}
	}
	return task, nil
}

func (a *App) revalidateTaskAction(original *model.Task, action string) error {
	current, err := a.eligibleTask(original.ID, action)
	if err != nil {
		return err
	}
	if !wirejson.Equal(current, original) {
		return conflictError("Task changed during remote checks; inspect its current state before trying again")
	}
	return nil
}

func (a *App) reconcileLocked(id string, task *model.Task) error {
	a.runtimeMu.Lock()
	baseline := a.runtime.baseline != nil
	busy := len(a.runtime.tasks) > 0
	a.runtimeMu.Unlock()
	if baseline {
		return conflictError("Wait for the baseline check to finish")
	}
	if busy {
		return conflictError("Wait for active tasks before publication reconciliation")
	}
	if task.OutputCommit == nil {
		var preflightErr error
		a.withoutGate(func() { preflightErr = a.retryPreflight(a.ctx, task) })
		if err := a.revalidateTaskAction(task, "reconcile"); err != nil {
			return err
		}
		if preflightErr == nil {
			task.BlockedReason = new(model.BlockedUnknown)
			task.Error = new("Remote prerequisites are restored; task can be retried")
		} else {
			recordTaskError(task, preflightErr)
		}
		if err := a.Store.ClearCancel(id); err != nil {
			return err
		}
		if err := a.saveTask(task); err != nil {
			return err
		}
		_ = a.Store.Event(id, "operator", "reconcile")
		a.notify()
		return nil
	}
	if err := a.Store.ClearCancel(id); err != nil {
		return err
	}
	previousStatus := task.Status
	if err := a.transition(task, model.StatusPublishing); err != nil {
		return err
	}
	workCtx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	a.runtimeMu.Lock()
	a.runtime.tasks[id] = taskJob{branch: task.Branch, cancel: cancel}
	a.runtime.reconciling = true
	a.runtimeMu.Unlock()
	defer func() {
		a.runtimeMu.Lock()
		delete(a.runtime.tasks, id)
		a.runtime.reconciling = false
		a.runtimeMu.Unlock()
		a.notify()
	}()

	var published model.PullRequest
	var publishErr error
	a.withoutGate(func() {
		limit := task.ExecutionConfig().TaskTimeout()
		result, err := runJoined(workCtx, cancel, limit, "Publication reconciliation panicked", func() error {
			p, err := gitops.Publish(workCtx, *task)
			if err == nil {
				published = p
			}
			return err
		})
		publishErr = err
		if publishErr != nil && result.Expired {
			if result.AlreadyCancelled {
				publishErr = fmt.Errorf("Publication reconciliation was interrupted; reconcile again: %w", model.BlockedPublicationUncertain)
			} else {
				publishErr = model.BlockedTimeout
			}
		}
	})

	var actionErr error
	if publishErr == nil {
		actionErr = a.published(task, published)
	} else {
		recordTaskError(task, publishErr)
		actionErr = a.transition(task, previousStatus)
	}
	_ = a.Store.Event(id, "operator", "reconcile")
	current, loadErr := store.Get[model.Task](a.Store, "task", id)
	if loadErr == nil && current != nil && current.Status.Active() {
		a.settleExitedTask(current, errors.New("Task worker exited unexpectedly; inspect the preserved workspace"))
	} else if loadErr != nil {
		a.setRecoveryError(loadErr)
	}
	return actionErr
}

// removeOwnedRoot removes root, an owned root under parent, under its cleanup claim: the claim is taken with the
// gate the caller holds, so no operator action can act on the record meanwhile, and released once the removal is
// done; the removal itself runs without the gate. A root another cleanup already claims is a conflict.
func (a *App) removeOwnedRoot(kind cleanupKind, id, parent, root string) error {
	if !a.claimCleanup(kind, id) {
		return conflictError("Workspace cleanup is already in progress for this " + string(kind))
	}
	defer a.releaseCleanup(kind, id)
	var err error
	a.withoutGate(func() { err = workspace.RemoveOwnedDir(parent, root) })
	return err
}

func (a *App) discardTask(task *model.Task) error {
	if task.Status.Active() || task.Status == model.StatusQueued {
		return errors.New("Active or queued workspaces cannot be discarded")
	}
	if task.Lifecycle.DiscardedAt != nil {
		return conflictError("The task workspace was already discarded")
	}
	if task.Workspace != "" {
		parent := filepath.Join(a.dataDir, "tasks")
		owner := filepath.Dir(filepath.Clean(task.Workspace))
		if owner != filepath.Join(parent, task.ID) {
			return errors.New("Cleanup path does not belong to this task")
		}
		if err := a.removeOwnedRoot(cleanupTask, task.ID, parent, owner); err != nil {
			return err
		}
	}
	current, err := store.Get[model.Task](a.Store, "task", task.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.Status.Active() || current.Status == model.StatusQueued {
		return conflictError("Task resumed work during workspace cleanup; inspect it before discarding")
	}
	current.Lifecycle.DiscardedAt = new(model.Now())
	if err := a.Store.Put("task", current.ID, *current); err != nil {
		return err
	}
	task.Lifecycle.DiscardedAt = current.Lifecycle.DiscardedAt
	a.clearCleanupReport(cleanupTask, task.ID)
	return nil
}

func (a *App) discardCycle(cycle *model.Cycle) error {
	if cycle.Status == model.CycleRunning {
		return errors.New("Running planning work cannot be discarded")
	}
	if cycle.Lifecycle.DiscardedAt != nil {
		return conflictError("The cycle workspaces were already discarded")
	}
	if _, err := uuid.Parse(cycle.ID); err != nil {
		return errors.New("Invalid cycle workspace identity")
	}
	root := filepath.Join(a.dataDir, "cycles")
	if err := a.removeOwnedRoot(cleanupCycle, cycle.ID, root, filepath.Join(root, cycle.ID)); err != nil {
		return err
	}
	current, err := store.Get[model.Cycle](a.Store, "cycle", cycle.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.Status == model.CycleRunning {
		return conflictError("Planning work restarted during workspace cleanup; inspect it before discarding")
	}
	current.Lifecycle.DiscardedAt = new(model.Now())
	if err := a.Store.Put("cycle", current.ID, *current); err != nil {
		return err
	}
	cycle.Lifecycle.DiscardedAt = current.Lifecycle.DiscardedAt
	a.clearCleanupReport(cleanupCycle, cycle.ID)
	return nil
}
