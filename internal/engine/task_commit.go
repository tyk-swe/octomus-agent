package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// commitTaskLocked is the orchestrator boundary. Caller holds gate, and supplies
// the unchanged saved predecessor rather than a partially edited working copy.
func (a *App) commitTaskLocked(expected model.Task, task *model.Task, change model.TaskChange) error {
	next := task.Clone()
	next.UpdatedAt = model.Now()
	committed, err := a.store.CommitTask(expected, next, change)
	if err != nil {
		if errors.Is(err, store.ErrTaskConflict) {
			return conflictError(err.Error())
		}
		return err
	}
	*task = committed
	return nil
}

// commitWorkerTask takes gate only for ownership validation and the transaction.
// Pending observations survive refusal; committed snapshots advance only on success.
func (a *App) commitWorkerTask(ctx context.Context, task *model.Task, change model.TaskChange) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	return a.commitWorkerTaskLocked(ctx, task, change)
}

func (a *App) commitWorkerTaskLocked(ctx context.Context, task *model.Task, change model.TaskChange) error {
	job := taskWorkerFrom(ctx)
	a.runtimeMu.Lock()
	owned := job != nil && a.runtime.tasks[task.ID] == job
	a.runtimeMu.Unlock()
	if !owned {
		return errors.New("Task worker no longer owns its checkpoint")
	}
	next := task.Clone()
	if change == model.TaskSettlement {
		next = model.SettleTaskEvidence(job.committed, next)
	}
	next.UpdatedAt = model.Now()
	job.pending = &next
	committed, err := a.store.CommitTask(job.committed, next, change)
	if err != nil {
		return err
	}
	job.committed, job.pending = committed.Clone(), nil
	*task = committed
	return nil
}

func (a *App) checkpointTask(ctx context.Context, task *model.Task) error {
	return a.commitWorkerTask(ctx, task, model.TaskCheckpoint)
}

func (a *App) transitionTask(ctx context.Context, task *model.Task, status model.Status) error {
	next := task.Clone()
	next.Status = status
	change := model.TaskCheckpoint
	if status == model.StatusBlocked || status == model.StatusCancelled || status == model.StatusFailed {
		change = model.TaskSettlement
	}
	if err := a.commitWorkerTask(ctx, &next, change); err != nil {
		return err
	}
	*task = next
	_ = a.store.Event(task.ID, model.EventStatus, statusLabel(task.Status))
	return nil
}

// Caller holds gate after the publication child has joined.
func (a *App) publishedWorkerLocked(ctx context.Context, task *model.Task, pr model.PullRequest) error {
	job := taskWorkerFrom(ctx)
	a.runtimeMu.Lock()
	owned := job != nil && a.runtime.tasks[task.ID] == job
	a.runtimeMu.Unlock()
	if !owned {
		return errors.New("Task worker no longer owns its publication")
	}
	if err := a.completePublicationLocked(task, pr); err != nil {
		return err
	}
	job.committed, job.pending = task.Clone(), nil
	return nil
}

func (a *App) completePublicationLocked(task *model.Task, pr model.PullRequest) error {
	next := task.Clone()
	next.PRNumber = &pr.Number
	next.PRURL = &pr.URL
	next.Error = nil
	next.BlockedReason = nil
	next.Status = model.StatusPublished
	next.UpdatedAt = model.Now()
	merge, err := a.initialMergeState(&next)
	if err != nil {
		return err
	}
	committed, err := a.store.CompletePublication(*task, next, pr, merge)
	if err != nil {
		return err
	}
	*task = committed
	a.runtimeMu.Lock()
	a.runtime.lastMergeCheck = time.Time{}
	a.runtimeMu.Unlock()
	a.notify()
	return nil
}

// admitTaskLocked is the gate-held scheduler admission path for an existing-PR task.
func (a *App) admitTaskLocked(task *model.Task) error {
	expected := task.Clone()
	next := task.Clone()
	next.Status = model.StatusExecuting
	if err := a.commitTaskLocked(expected, &next, model.TaskAdmission); err != nil {
		return err
	}
	*task = next
	_ = a.store.Event(task.ID, model.EventStatus, statusLabel(task.Status))
	return nil
}

func (a *App) blockTaskLocked(task *model.Task, err error) error {
	next := task.Clone()
	recordTaskError(&next, err)
	next.Status = model.StatusBlocked
	if err := a.commitTaskLocked(*task, &next, model.TaskSettlement); err != nil {
		return err
	}
	*task = next
	_ = a.store.Event(task.ID, model.EventStatus, statusLabel(task.Status))
	return nil
}

func recordTaskError(task *model.Task, err error) {
	reason := model.BlockedReasonFromError(err)
	if reason == model.BlockedUnknown && task.Status == model.StatusPublishing && task.OutputCommit != nil {
		// External publication may have succeeded before its durable settlement
		// failed. Keep known preflight refusals and the original diagnostic.
		reason = model.BlockedPublicationUncertain
	}
	task.BlockedReason = &reason
	task.Error = new(redact.Error(err))
}

// cancelledByOperator is the durable reason an operator cancellation records.
const cancelledByOperator = "Cancelled by the operator"

// taskCancellationApplies reports whether a durable operator cancellation still governs this task. A task that reached
// an output checkpoint is excluded: publication cannot be undone, so it stays uncertain for explicit reconciliation,
// exactly as restart recovery decides the same marker. The caller must hold no lock; this is a read.
func (a *App) taskCancellationApplies(task model.Task) (bool, error) {
	if task.OutputCommit != nil {
		return false, nil
	}
	return a.store.CancelMarked(task.ID)
}

// settleExitedTask commits what a worker's exit leaves in the durable record. The caller holds the gate, so settlement
// is serialized with operator cancellation.
//
// Cancellation markers also settle historical or refused cancellation writes.
// The worker's cause stays in an ancillary event, and terminal state without a
// marker stands. New worker checkpoints cannot overwrite cancellation.
//
// The error is returned so the caller can raise the recovery barrier at the worker join or fail the recovery scan.
func (a *App) settleExitedTask(task *model.Task, cause error) error {
	expected := task.Clone()
	next := task.Clone()
	model.FailRunning(next.Sessions, redact.Error(cause))
	cancelled, err := a.taskCancellationApplies(next)
	if err != nil {
		return err
	}
	if cancelled {
		next.Status = model.StatusCancelled
		next.Error, next.BlockedReason = new(cancelledByOperator), nil
	} else if next.Status.Active() {
		recordTaskError(&next, cause)
		next.Status = model.StatusBlocked
	} else if wirejson.Equal(expected, next) {
		return nil
	}
	if err := a.commitTaskLocked(expected, &next, model.TaskSettlement); err != nil {
		return err
	}
	*task = next
	if next.Status != expected.Status {
		_ = a.store.Event(task.ID, model.EventStatus, statusLabel(task.Status))
	}
	if cancelled {
		_ = a.store.Event(task.ID, model.EventError, redact.Error(cause))
	}
	return nil
}

func statusLabel(status model.Status) string {
	name := status.String()
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
