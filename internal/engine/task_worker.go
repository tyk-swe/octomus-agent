package engine

import (
	"context"
	"errors"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// Caller holds gate after durable admission, before starting the child. Both
// execution and reconciliation own the task and its branch through child join
// and durable settlement. Shutdown sees the registered cancellation function.
// Scheduled execution is joined by runTask; synchronous reconciliation is joined
// by TaskAction's whole-action wait group, including its preflight and settlement.
func (a *App) registerTaskWorker(task model.Task, kind taskWorkerKind) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(a.ctx)
	a.runtimeMu.Lock()
	a.runtime.tasks[task.ID] = taskJob{branch: task.Branch, cancel: cancel, kind: kind}
	a.runtimeMu.Unlock()
	return ctx, cancel
}

// Caller holds gate after joining all child work. An unresolved terminal write
// raises the recovery barrier before the ownership claim is released. Ordinary
// initialized execution stays active on shutdown for restart recovery;
// reconciliation always preserves an uncertain publication for operator action.
func (a *App) finishTaskWorker(id string, cause error) {
	a.runtimeMu.Lock()
	job := a.runtime.tasks[id]
	a.runtimeMu.Unlock()
	current, loadErr := store.Get[model.Task](a.Store, "task", id)
	interrupted := job.kind == executionWorker && a.ctx.Err() != nil && current != nil && workspace.Initialized(*current)
	if loadErr != nil {
		a.setRecoveryError(loadErr)
	} else if current != nil && current.Status.Active() && !interrupted {
		if cause == nil || errors.Is(cause, context.Canceled) {
			cause = errors.New("Task worker exited unexpectedly; inspect the preserved workspace")
		}
		a.settleExitedTask(current, cause)
	}
	a.runtimeMu.Lock()
	delete(a.runtime.tasks, id)
	if loadErr != nil || current != nil && current.Status == model.StatusCancelled {
		a.runtime.cancelScanDone = false
	}
	a.runtimeMu.Unlock()
	a.notify()
}
