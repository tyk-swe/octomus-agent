package engine

import (
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

func (a *App) CycleAction(id string, action model.CycleAction) error {
	a.gate.Lock()
	if err := a.ctx.Err(); err != nil {
		a.gate.Unlock()
		return err
	}
	a.wg.Add(1)
	defer a.wg.Done()
	defer a.gate.Unlock()
	cycle, err := store.Get(a.store, store.Cycles.Key(id))
	if err != nil {
		return err
	}
	if cycle == nil {
		return NotFound("Cycle not found")
	}
	if !slices.Contains(model.CycleActions(cycle.Status, cycle.Lifecycle), action) {
		switch {
		case cycle.Status == model.CycleRunning:
			return conflictError("Wait for planning to finish")
		case action == model.CycleActionArchive:
			return conflictError("The cycle is already archived")
		case cycle.Lifecycle.DiscardedAt != nil:
			return conflictError("The cycle workspaces were already discarded")
		default:
			return conflictError("Archive the cycle before discarding its workspace")
		}
	}
	if a.cleanupClaimed(cleanupCycle, id) {
		return conflictError("Workspace cleanup is in progress for this cycle; wait for it to finish")
	}
	switch action {
	case model.CycleActionArchive:
		cycle.Lifecycle.ArchivedAt = new(model.Now())
		if err := store.Put(a.store, store.Cycles.Key(id), *cycle); err != nil {
			return err
		}
	case model.CycleActionDiscard:
		if err := a.discardCycle(cycle); err != nil {
			return err
		}
	default:
		return NotFound("Unknown cycle action")
	}
	_ = a.store.Event(id, "operator", action.String())
	return nil
}

func (a *App) TaskAction(id string, action model.TaskAction) error {
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
	if action == model.TaskActionReconcile {
		return a.reconcileLocked(id, task)
	}
	var actionErr error
	switch action {
	case model.TaskActionCancel:
		actionErr = a.cancelTask(id)
	case model.TaskActionRetry:
		actionErr = a.retryTask(task)
	case model.TaskActionSupersede:
		next := task.Clone()
		next.Status = model.StatusCancelled
		next.RediscoveryRequested = true
		next.RediscoveryResult = nil
		next.UpdatedAt = model.Now()
		actionErr = a.store.ResetTaskCancellation(next)
		if actionErr == nil {
			*task = next
		}
	case model.TaskActionArchive:
		next := task.Clone()
		if next.Status.Retryable() {
			next.Status = model.StatusCancelled
		}
		next.Lifecycle.ArchivedAt = new(model.Now())
		next.UpdatedAt = model.Now()
		actionErr = a.store.ArchiveTask(next)
		if actionErr == nil {
			*task = next
		}
	case model.TaskActionDiscard:
		actionErr = a.discardTask(task)
	default:
		return NotFound("Unknown task action")
	}
	if actionErr == nil {
		if action == model.TaskActionCancel || task.Status == model.StatusCancelled {
			a.runtimeMu.Lock()
			a.runtime.cancelScanDone = false
			a.runtimeMu.Unlock()
		}
		_ = a.store.Event(id, "operator", action.String())
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
	if err := a.store.MarkCancel(id); err != nil {
		return err
	}
	cancelled, err := a.store.CancelTask(id)
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
	next := task.Clone()
	policy := model.PolicyOf(cfg)
	next.AttemptPolicy = &policy
	if task.OutputCommit == nil {
		var preflightErr error
		a.withoutGate(func() { preflightErr = a.retryPreflight(a.ctx, &next) })
		if err := a.revalidateTaskAction(&original, model.TaskActionRetry); err != nil {
			return err
		}
		live, err := a.Config()
		if err != nil {
			return err
		}
		livePolicy := model.PolicyOf(live)
		if next.AttemptPolicy == nil || *next.AttemptPolicy != livePolicy {
			return conflictError("Retry policy changed during remote checks; try again with the current limits")
		}
		if preflightErr != nil {
			recordTaskError(&next, preflightErr)
			if err := a.checkpointTask(&next); err == nil {
				*task = next
			}
			return preflightErr
		}
	}
	next.Attempts++
	next.ReviewBaseline = uint64(len(next.Reviews))
	next.RepairRounds = new(uint64(0))
	next.RepairProgress = nil
	next.Error = nil
	next.BlockedReason = nil
	next.Status = model.StatusQueued
	next.RunID = nil
	next.UpdatedAt = model.Now()
	if err := a.store.RetryTask(next); err != nil {
		return err
	}
	*task = next
	return nil
}

func (a *App) eligibleTask(id string, action model.TaskAction) (*model.Task, error) {
	task, err := store.Get(a.store, store.Tasks.Key(id))
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, NotFound("Task not found")
	}
	if !slices.Contains(task.AllowedActions(), action) {
		return nil, conflictError("This action is not eligible for the task's recorded failure and workspace state")
	}
	if action != model.TaskActionCancel {
		a.runtimeMu.Lock()
		_, merging := a.runtime.merges[task.Branch]
		a.runtimeMu.Unlock()
		if merging {
			return nil, conflictError("An automatic merge check is in flight for this delivery; wait for it to finish")
		}
	}
	if a.cleanupClaimed(cleanupTask, id) {
		return nil, conflictError("Workspace cleanup is in progress for this task; wait for it to finish")
	}
	if action != model.TaskActionCancel {
		a.runtimeMu.Lock()
		_, running := a.runtime.tasks[id]
		a.runtimeMu.Unlock()
		if running {
			return nil, conflictError("Wait for active task work to finish")
		}
	}
	return task, nil
}

func (a *App) revalidateTaskAction(original *model.Task, action model.TaskAction) error {
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
		if err := a.revalidateTaskAction(task, model.TaskActionReconcile); err != nil {
			return err
		}
		next := task.Clone()
		if preflightErr == nil {
			next.BlockedReason = new(model.BlockedUnknown)
			next.Error = new("Remote prerequisites are restored; task can be retried")
		} else {
			recordTaskError(&next, preflightErr)
		}
		next.UpdatedAt = model.Now()
		if err := a.store.ResetTaskCancellation(next); err != nil {
			return err
		}
		*task = next
		_ = a.store.Event(id, "operator", model.TaskActionReconcile.String())
		a.notify()
		return nil
	}
	live, err := a.Config()
	if err != nil {
		return err
	}
	if task.Config.DeliveryMode != live.DeliveryMode ||
		!slices.Contains(task.Config.EffectiveCategories(), task.Proposal.Category) {
		return invalidPlan(fmt.Sprintf("Task %s was planned under an incompatible delivery mode or category", id))
	}
	previousStatus := task.Status
	next := task.Clone()
	next.Status = model.StatusPublishing
	next.UpdatedAt = model.Now()
	if err := a.store.ResetTaskCancellation(next); err != nil {
		return err
	}
	*task = next
	_ = a.store.Event(id, "status", statusLabel(model.StatusPublishing))
	workCtx, cancel := a.registerTaskWorker(*task, reconciliationWorker)
	defer cancel()
	var published model.PullRequest
	var publishErr, actionErr error
	defer func() { a.finishTaskWorker(id, errors.Join(publishErr, actionErr)) }()
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

	if publishErr == nil {
		actionErr = a.published(task, published)
	} else {
		recordTaskError(task, publishErr)
		actionErr = a.transition(task, previousStatus)
	}
	_ = a.store.Event(id, "operator", model.TaskActionReconcile.String())
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
	a.withoutGate(func() { err = workspace.RemoveOwnedDirContext(a.ctx, parent, root) })
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
	current, err := store.Get(a.store, store.Tasks.Key(task.ID))
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
	if err := a.store.SaveTask(*current); err != nil {
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
	current, err := store.Get(a.store, store.Cycles.Key(cycle.ID))
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
	if err := store.Put(a.store, store.Cycles.Key(current.ID), *current); err != nil {
		return err
	}
	cycle.Lifecycle.DiscardedAt = current.Lifecycle.DiscardedAt
	a.clearCleanupReport(cleanupCycle, cycle.ID)
	return nil
}
