package engine

// A refused operator cancellation leaves only its durable marker; settlement of the
// worker must not discard that intent, and the worker's own cause must stay visible.

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// refusedCancellation stages the durable state a refused cancellation leaves behind: the
// cancellation marker is committed, every task write is refused, and the service then
// loses the injected fault. The task stays active with a live session.
func refusedCancellation(t *testing.T) (*fixture, model.Task, *App) {
	t.Helper()
	f := newFixture(t)
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusReviewing
	task.Sessions = []model.Session{model.NewSession("reviewer", "reviewer", task.Route)}
	putTask(t, f, task)
	db := f.rawDB(t)
	_, err := db.Exec(`CREATE TRIGGER refuse_task_writes BEFORE UPDATE ON records
		WHEN NEW.kind='task' BEGIN SELECT RAISE(FAIL,'task checkpoint refused'); END`)
	must0(t, err)
	app := f.pausedApp(t)
	if err := app.TaskAction(task.ID, model.TaskActionCancel); err == nil {
		t.Fatal("a refused cancellation must be reported to the operator")
	}
	marked, err := f.state.CancelMarked(task.ID)
	if err != nil || !marked {
		t.Fatalf("refused cancellation lost its durable marker: %t, %v", marked, err)
	}
	if saved := loadTask(t, f.state, task.ID); saved.Status != model.StatusReviewing {
		t.Fatalf("refused cancellation changed the task: %s", saved.Status)
	}
	_, err = db.Exec("DROP TRIGGER refuse_task_writes")
	must0(t, err)
	return f, task, app
}

// assertSettledCancellation checks the durable record a settled cancellation leaves: cancelled with no blocked
// reason, the operator's own summary, no running session, and the worker's cause kept as an event.
func assertSettledCancellation(t *testing.T, f *fixture, task model.Task, cause string) {
	t.Helper()
	saved := loadTask(t, f.state, task.ID)
	if saved.Status != model.StatusCancelled || saved.BlockedReason != nil {
		t.Fatalf("settlement replaced a refused cancellation: status=%s reason=%v", saved.Status, saved.BlockedReason)
	}
	if saved.Error == nil || *saved.Error != "Cancelled by the operator" {
		t.Fatalf("cancellation summary = %v", saved.Error)
	}
	for _, session := range saved.Sessions {
		if session.Status == model.SessionRunning {
			t.Fatalf("cancelled task kept a running session: %+v", saved.Sessions)
		}
	}
	events, err := f.state.Events(&task.ID)
	must0(t, err)
	for _, event := range events {
		if event.Kind == model.EventError && strings.Contains(event.Message, cause) {
			return
		}
	}
	t.Fatalf("worker cause %q was dropped from the task's events: %+v", cause, events)
}

// The worker join owns the durable record right after the worker's own last write, so it must keep a cancellation
// the worker may have replaced while it was failing.
func TestExitedWorkerSettlementKeepsRefusedCancellation(t *testing.T) {
	t.Parallel()
	f, task, app := refusedCancellation(t)
	app.gate.Lock()
	_, cancel := app.registerTaskWorker(task, executionWorker)
	cancel()
	app.finishTaskWorker(task.ID, errors.New("worker failed"))
	app.gate.Unlock()
	assertSettledCancellation(t, f, task, "worker failed")
	// With the fault gone the recovery pass must complete, not stay blocked.
	if err := app.tick(); err != nil || app.recoveryConflict() != nil {
		t.Fatalf("recovery did not make progress: %v, %v", err, app.recoveryConflict())
	}
}

// The scheduler's orphan scan settles a workerless active task the same way, so the
// refused cancellation cannot survive as a blocked task.
func TestOrphanScanKeepsRefusedCancellation(t *testing.T) {
	t.Parallel()
	f, task, app := refusedCancellation(t)
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	assertSettledCancellation(t, f, task, "worker exited unexpectedly")
	if app.recoveryConflict() != nil {
		t.Fatalf("recovery stayed blocked: %v", app.recoveryConflict())
	}
}

// A durable cancellation never overturns a committed outcome: an unmarked workerless task still blocks, and a task
// that reached an output checkpoint stays publication_uncertain for explicit reconciliation.
func TestCancellationMarkerDoesNotOverturnOutputCheckpoint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	orphan := executionTask(t, f, f.cfg.DefaultBranch)
	orphan.Status = model.StatusExecuting
	putTask(t, f, orphan)
	publishing := checkpointedTask(t, f, f.cfg.DefaultBranch)
	publishing.Status = model.StatusPublishing
	putTask(t, f, publishing)
	must0(t, f.state.MarkCancel(publishing.ID))
	app := f.pausedApp(t)
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	if saved := loadTask(t, f.state, orphan.ID); saved.Status != model.StatusBlocked {
		t.Fatalf("an unmarked workerless task was not blocked: %+v", saved)
	}
	saved := loadTask(t, f.state, publishing.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedPublicationUncertain {
		t.Fatalf("output checkpoint settlement = %+v", saved)
	}
}
