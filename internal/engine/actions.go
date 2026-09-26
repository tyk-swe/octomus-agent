// actions.go owns the operator task controls: cancel, retry, supersede,
// archive, discard and publication reconcile. Every control reads the durable
// record under gate, runs remote checks with the gate released, then
// revalidates that nothing authoritative changed before writing — the same
// contract enforced by the API layer.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// ErrTaskNotFound reports a control addressed at an unknown durable task.
var ErrTaskNotFound = errors.New("Task not found")

// actionConflict is the operator-visible 409 returned when an
// action is ineligible or durable state changed during remote checks.
type actionConflict struct{ msg string }

func (e *actionConflict) Error() string { return e.msg }

func conflictError(message string) error { return &actionConflict{message} }

// IsActionConflict reports whether err is an eligibility/concurrency conflict —
// errors mapped to HTTP 409.
func IsActionConflict(err error) bool {
	var c *actionConflict
	return errors.As(err, &c) || model.BlockedReasonFromError(err) != model.BlockedReasonUnknown
}

// TaskAction applies one operator control to a durable task. Gate is held for
// eligibility and durable writes, released around remote checks, and
// re-acquired for revalidation. Remote preflights run under the app shutdown scope, never the caller's request
// scope, so a disconnect cannot interrupt remote checks or publication.
func (a *App) TaskAction(_ context.Context, id, action string) error {
	a.gate.Lock()
	if err := a.ctx.Err(); err != nil {
		a.gate.Unlock()
		return err
	}
	// Register under the shutdown gate, before any remote preflight releases
	// it. Ownership lasts through revalidation, durable writes and events.
	a.wg.Add(1)
	defer a.wg.Done()
	task, err := a.eligibleTask(id, action)
	if err != nil {
		a.gate.Unlock()
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
		actionErr = a.DiscardTask(task)
	default:
		a.gate.Unlock()
		return ErrUnknownTaskAction
	}
	if actionErr == nil {
		actionErr = a.Store.Event(id, "operator", action)
	}
	a.gate.Unlock()
	a.notify()
	return actionErr
}

// cancelTask marks the durable cancellation intent, then flips the stored
// status only when no publication checkpoint exists — a task whose output
// commit was recorded keeps it for reconciliation instead.
func (a *App) cancelTask(id string) error {
	a.runtimeMu.Lock()
	job, running := a.runtime.tasks[id]
	a.runtimeMu.Unlock()
	if running {
		job.cancel()
	}
	// The marker record is the durable intent: the running task never writes
	// this kind, so its final save cannot clobber it.
	if err := a.Store.MarkCancel(id); err != nil {
		return err
	}
	// The worker may have advanced or finished since eligibility was read.
	// Check the stored publication checkpoint in the same write as cancellation.
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
		a.gate.Unlock()
		preflightErr := a.retryPreflight(a.ctx, task)
		a.gate.Lock()
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
	// A new attempt starts its repair-round budget from the reviews recorded so far.
	task.ReviewBaseline = uint64(len(task.Reviews))
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

// eligibleTask loads the durable record and checks the action is currently
// allowed — the eligibility gate runs before any work.
func (a *App) eligibleTask(id, action string) (*model.Task, error) {
	task, err := store.Get[model.Task](a.Store, "task", id)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrTaskNotFound
	}
	allowed := false
	for _, candidate := range task.AllowedActions() {
		if candidate == action {
			allowed = true
		}
	}
	if !allowed {
		return nil, conflictError("This action is not eligible for the task's recorded failure and workspace state")
	}
	// An owned workspace cleanup in flight wins over every action — retry,
	// reconcile, cancel, archive and a second discard all wait for the
	// removal owner to finish rather than racing a half-removed workspace.
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

// revalidateTaskAction repeats eligibility and identity after remote checks:
// any authoritative change while the gate was released makes the operator
// re-inspect rather than act on stale evidence.
func (a *App) revalidateTaskAction(original *model.Task, action string) error {
	current, err := a.eligibleTask(original.ID, action)
	if err != nil {
		return err
	}
	if !sameRecordJSON(current, original) {
		return conflictError("Task changed during remote checks; inspect its current state before trying again")
	}
	return nil
}

func sameRecordJSON(a, b *model.Task) bool {
	left, err := wirejson.Marshal(a)
	if err != nil {
		return false
	}
	right, err := wirejson.Marshal(b)
	if err != nil {
		return false
	}
	return string(left) == string(right)
}

// recordTaskError classifies err into the task's blocked reason and stores its
// redacted message as the task error, without changing the task's status.
func recordTaskError(task *model.Task, err error) {
	reason := model.BlockedReasonFromError(err)
	task.BlockedReason = &reason
	message := store.ErrorMessage(err)
	task.Error = &message
}

// reconcileLocked runs the reconcile action. Without a recorded output it
// rechecks the task's remote prerequisites and records the result; with one it
// publishes that output under the task deadline. Callers hold a.gate on entry;
// reconcileLocked releases it around remote work and before it returns, and
// records the operator event itself so a disconnected caller cannot skip it.
func (a *App) reconcileLocked(id string, task *model.Task) error {
	a.runtimeMu.Lock()
	baseline := a.runtime.baseline != nil
	busy := len(a.runtime.tasks) > 0
	a.runtimeMu.Unlock()
	if baseline {
		a.gate.Unlock()
		return conflictError("Wait for the baseline check to finish")
	}
	if busy {
		a.gate.Unlock()
		return conflictError("Wait for active tasks before publication reconciliation")
	}
	if task.OutputCommit == nil {
		a.gate.Unlock()
		preflightErr := a.retryPreflight(a.ctx, task)
		a.gate.Lock()
		if err := a.revalidateTaskAction(task, "reconcile"); err != nil {
			a.gate.Unlock()
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
			a.gate.Unlock()
			return err
		}
		if err := a.saveTask(task); err != nil {
			a.gate.Unlock()
			return err
		}
		err := a.Store.Event(id, "operator", "reconcile")
		a.gate.Unlock()
		a.notify()
		return err
	}
	// Reconciliation revives the task; the operator-cancel marker is spent.
	if err := a.Store.ClearCancel(id); err != nil {
		a.gate.Unlock()
		return err
	}
	previousStatus := task.Status
	if err := a.transition(task, model.StatusPublishing); err != nil {
		a.gate.Unlock()
		return err
	}
	workCtx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	a.runtimeMu.Lock()
	a.runtime.tasks[id] = taskJob{branch: task.Branch, cancel: cancel}
	a.runtime.reconcilingPublication = true
	a.runtimeMu.Unlock()
	a.gate.Unlock()

	var published model.PullRequest
	limit := time.Duration(task.ExecutionConfig().TaskTimeoutSeconds) * time.Second
	// Uses the normal task deadline cleanup, so a stalled publication stops its
	// owned process groups before it is reported. The join keeps runtime
	// ownership until Publish has returned, so no retry or dispatch can start
	// a second publication beside one that outlived the cleanup grace.
	result, publishErr := runJoined(workCtx, cancel, limit, "Publication reconciliation panicked", func() error {
		p, err := gitops.Publish(workCtx, *task)
		if err == nil {
			published = p
		}
		return err
	})
	// A publication that completed after the deadline fired is delivered.
	if publishErr != nil && result.Expired {
		if result.AlreadyCancelled {
			publishErr = fmt.Errorf("Publication reconciliation was interrupted; reconcile again: %w", model.BlockedReasonPublicationUncertain)
		} else {
			publishErr = model.BlockedReasonTimeout
		}
	}

	a.gate.Lock()
	var actionErr error
	if publishErr == nil {
		actionErr = a.published(task, published)
	} else {
		recordTaskError(task, publishErr)
		actionErr = a.transition(task, previousStatus)
	}
	if eventErr := a.Store.Event(id, "operator", "reconcile"); actionErr == nil {
		actionErr = eventErr
	}
	// The task-guard half of completion: a reconcile that died mid-publication
	// must not leave a durable "publishing" record reading as live work.
	current, loadErr := store.Get[model.Task](a.Store, "task", id)
	if loadErr == nil && current != nil && current.Status.Active() {
		_ = a.setTaskError(current, errors.New("Task worker exited unexpectedly; inspect the preserved workspace"))
	}
	a.runtimeMu.Lock()
	delete(a.runtime.tasks, id)
	a.runtime.reconcilingPublication = false
	a.runtimeMu.Unlock()
	a.gate.Unlock()
	a.notify()
	return actionErr
}
