package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

var ErrTaskNotFound = errors.New("Task not found")

type actionConflict struct{ msg string }

func (e *actionConflict) Error() string { return e.msg }

func conflictError(message string) error { return &actionConflict{message} }

func IsActionConflict(err error) bool {
	var c *actionConflict
	return errors.As(err, &c) || model.BlockedReasonFromError(err) != model.BlockedReasonUnknown
}

func (a *App) TaskAction(_ context.Context, id, action string) error {
	switch action {
	case "cancel", "retry", "supersede", "archive", "discard", "reconcile":
	default:
		return ErrUnknownTaskAction
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
		now := model.Now()
		task.Lifecycle.ArchivedAt = &now
		actionErr = a.saveTask(task)
	case "discard":
		actionErr = a.discardTask(task)
	}
	if actionErr == nil {
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
	policy := model.AttemptPolicyFromConfig(cfg)
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
		livePolicy := model.AttemptPolicyFromConfig(live)
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
		return nil, ErrTaskNotFound
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

func recordTaskError(task *model.Task, err error) {
	reason := model.BlockedReasonFromError(err)
	task.BlockedReason = &reason
	message := redact.Error(err)
	task.Error = &message
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
			reason := model.BlockedReasonUnknown
			task.BlockedReason = &reason
			task.Error = stringPointer("Remote prerequisites are restored; task can be retried")
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
	a.runtime.reconcilingPublication = true
	a.runtimeMu.Unlock()
	defer func() {
		a.runtimeMu.Lock()
		delete(a.runtime.tasks, id)
		a.runtime.reconcilingPublication = false
		a.runtimeMu.Unlock()
		a.notify()
	}()

	var published model.PullRequest
	var publishErr error
	a.withoutGate(func() {
		limit := time.Duration(task.ExecutionConfig().TaskTimeoutSeconds) * time.Second
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
				publishErr = fmt.Errorf("Publication reconciliation was interrupted; reconcile again: %w", model.BlockedReasonPublicationUncertain)
			} else {
				publishErr = model.BlockedReasonTimeout
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
		a.setActiveRecoveryError(loadErr)
	}
	return actionErr
}
