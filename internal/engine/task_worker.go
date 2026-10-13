package engine

import (
	"context"
	"errors"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// Caller holds gate after durable admission, before starting the child. Both
// execution and reconciliation own the task and its branch through child join
// and durable settlement. Shutdown sees the registered cancellation function.
// Scheduled execution is joined by runTask; synchronous reconciliation is joined
// by TaskAction's whole-action wait group, including its preflight and settlement.
type taskWorkerContextKey struct{}

func taskWorkerFrom(ctx context.Context) *taskJob {
	job, _ := ctx.Value(taskWorkerContextKey{}).(*taskJob)
	return job
}

func (a *App) registerTaskWorker(task model.Task, kind taskWorkerKind) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a.ctx)
	a.runtimeMu.Lock()
	job := &taskJob{branch: task.Branch, cancel: cancel, kind: kind, committed: task.Clone()}
	if a.runtime.tasks[task.ID] != nil {
		a.runtimeMu.Unlock()
		cancel()
		panic("Task already has a worker")
	}
	a.runtime.tasks[task.ID] = job
	a.runtimeMu.Unlock()
	return context.WithValue(ctx, taskWorkerContextKey{}, job), cancel
}

// Caller holds gate after joining all child work. An unresolved terminal write
// raises the recovery barrier before the ownership claim is released. Ordinary
// initialized execution stays active on shutdown for restart recovery;
// reconciliation always preserves an uncertain publication for operator action.
// A durable cancellation marker wins over late evidence without permitting
// worker state to replace the operator decision.
func (a *App) finishTaskWorker(id string, cause error) {
	a.runtimeMu.Lock()
	job := a.runtime.tasks[id]
	a.runtimeMu.Unlock()
	if job == nil {
		return
	}
	settlementErr := a.flushTaskSettlement(job, cause)
	// Invalid or unrelated stale evidence cannot be rebased or retried. Settle the
	// canonical record instead of trapping recovery behind an impossible write.
	if errors.Is(settlementErr, store.ErrTaskConflict) || errors.Is(settlementErr, store.ErrTaskInvalid) {
		cause, settlementErr, job.pending = settlementErr, nil, nil
	}
	current, loadErr := store.Get(a.store, store.Tasks.Key(id))
	interrupted := job.kind == executionWorker && a.ctx.Err() != nil && current != nil && workspace.Initialized(*current)
	if settlementErr == nil {
		if loadErr != nil {
			settlementErr = loadErr
		} else if current != nil && !interrupted {
			if cause == nil || errors.Is(cause, context.Canceled) {
				cause = causeOrUnexpected(nil)
			}
			if err := a.settleExitedTask(current, cause); err != nil {
				job.committed = current.Clone()
				pending := current.Clone()
				job.pending = &pending
				settlementErr = err
			}
		}
	}
	if settlementErr != nil {
		a.setRecoveryError(settlementErr)
		// Keep refused evidence until storage recovers. Admission stays blocked, and
		// recovery excludes these records from orphan and retention scans.
		a.runtimeMu.Lock()
		if a.runtime.settlements == nil {
			a.runtime.settlements = make(map[string]*taskJob)
		}
		a.runtime.settlements[id] = job
		a.runtimeMu.Unlock()
	}

	a.runtimeMu.Lock()
	delete(a.runtime.tasks, id)
	if loadErr != nil || current != nil && current.Status == model.StatusCancelled {
		a.runtime.cancelScanDone = false
	}
	a.runtimeMu.Unlock()
	a.notify()
}

func causeOrUnexpected(cause error) error {
	if cause != nil {
		return cause
	}
	return errors.New("Task worker exited unexpectedly; inspect the preserved workspace")
}

// Caller holds gate. No worker is running when this is called by recovery.
func (a *App) flushTaskSettlement(job *taskJob, cause error) error {
	if job.pending == nil {
		return nil
	}
	next := model.SettleTaskEvidence(job.committed, *job.pending)
	interrupted := job.kind == executionWorker && a.ctx.Err() != nil && workspace.Initialized(job.committed) && next.Status.Active()
	if !interrupted {
		model.FailRunning(next.Sessions, redact.Error(causeOrUnexpected(cause)))
		if next.Status.Active() {
			recordTaskError(&next, causeOrUnexpected(cause))
			next.Status = model.StatusBlocked
		}
	}
	next.UpdatedAt = model.Now()
	job.pending = &next
	committed, err := a.store.CommitTask(job.committed, next, model.TaskSettlement)
	if err != nil {
		return err
	}
	if committed.Status != job.committed.Status {
		_ = a.store.Event(committed.ID, model.EventStatus, statusLabel(committed.Status))
	}
	job.committed, job.pending = committed.Clone(), nil
	return nil
}

func (a *App) settlePendingTasks() error {
	a.runtimeMu.Lock()
	pending := make(map[string]*taskJob, len(a.runtime.settlements))
	for id, job := range a.runtime.settlements {
		pending[id] = job
	}
	a.runtimeMu.Unlock()
	for id, job := range pending {
		if err := a.flushTaskSettlement(job, nil); err != nil {
			if !errors.Is(err, store.ErrTaskConflict) && !errors.Is(err, store.ErrTaskInvalid) {
				return err
			}
			// Invalid evidence and later operator records cannot be overwritten.
			job.pending = nil
		}
		a.runtimeMu.Lock()
		delete(a.runtime.settlements, id)
		a.runtime.cancelScanDone = false
		a.runtimeMu.Unlock()
	}
	return nil
}
