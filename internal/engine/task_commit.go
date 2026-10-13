package engine

import (
	"context"
	"errors"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
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
func (a *App) publishedWorkerLocked(ctx context.Context, task *model.Task, p model.PullRequest) error {
	job := taskWorkerFrom(ctx)
	a.runtimeMu.Lock()
	owned := job != nil && a.runtime.tasks[task.ID] == job
	a.runtimeMu.Unlock()
	if !owned {
		return errors.New("Task worker no longer owns its publication")
	}
	if err := a.published(task, p); err != nil {
		return err
	}
	job.committed, job.pending = task.Clone(), nil
	return nil
}
