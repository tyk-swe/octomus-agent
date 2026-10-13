package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func repairCheckpointFixture(t *testing.T) (*fixture, *App, model.Task) {
	t.Helper()
	f := newFixture(t)
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Status, task.RepairRounds = model.StatusRepairing, new(uint64(0))
	task.Sessions = []model.Session{model.NewSession("repair-1", "repair", task.Route)}
	putTask(t, f, task)
	return f, f.pausedApp(t), task
}

func completePendingRepair(task *model.Task) {
	task.Sessions[0].MarkCompleted("Repair completed")
	task.RepairRounds = new(uint64(1))
	task.RepairProgress = &model.RepairProgress{AwaitingReview: true}
}

func TestWorkerCancellationRetainsPendingRepairOnce(t *testing.T) {
	t.Parallel()
	f, app, task := repairCheckpointFixture(t)
	ctx := testTaskWorker(t, app, &task)
	must0(t, app.TaskAction(task.ID, model.TaskActionCancel))
	completePendingRepair(&task)
	if err := app.checkpointTask(ctx, &task); !errors.Is(err, model.ErrTaskCancelled) {
		t.Fatalf("late checkpoint: %v", err)
	}
	if saved := loadTask(t, f.state, task.ID); saved.Status != model.StatusCancelled || *saved.RepairRounds != 0 {
		t.Fatal("late checkpoint overwrote cancellation")
	}
	app.gate.Lock()
	app.finishTaskWorker(task.ID, context.Canceled)
	app.gate.Unlock()
	saved := loadTask(t, f.state, task.ID)
	if saved.Status != model.StatusCancelled || *saved.RepairRounds != 1 || saved.Sessions[0].Status != model.SessionCompleted {
		t.Fatal("worker settlement lost cancellation or repair accounting")
	}
	if err := app.checkpointTask(ctx, &task); err == nil {
		t.Fatal("released handle accepted another checkpoint")
	}
	if saved := loadTask(t, f.state, task.ID); *saved.RepairRounds != 1 {
		t.Fatal("repair accounting was charged twice")
	}
}

func TestReplacedWorkerHandleCannotWrite(t *testing.T) {
	t.Parallel()
	f, app, task := repairCheckpointFixture(t)
	old := testTaskWorker(t, app, &task)
	app.gate.Lock()
	app.finishTaskWorker(task.ID, errors.New("First worker stopped"))
	app.gate.Unlock()
	replacement := loadTask(t, f.state, task.ID)
	replacement.Status, replacement.Error, replacement.BlockedReason = model.StatusExecuting, nil, nil
	putTask(t, f, replacement)
	fresh := testTaskWorker(t, app, &replacement)
	if err := app.checkpointTask(old, &task); err == nil || !strings.Contains(err.Error(), "no longer owns") {
		t.Fatalf("replaced handle: %v", err)
	}
	must0(t, app.checkpointTask(fresh, &replacement))
}

func TestRefusedSettlementRetainsEvidenceUntilRecovery(t *testing.T) {
	t.Parallel()
	f, app, task := repairCheckpointFixture(t)
	ctx := testTaskWorker(t, app, &task)
	completePendingRepair(&task)
	db := f.rawDB(t)
	_, err := db.Exec(`CREATE TRIGGER refuse_task_writes BEFORE UPDATE ON records WHEN NEW.kind='task' BEGIN SELECT RAISE(FAIL,'task checkpoint refused'); END`)
	must0(t, err)
	if err := app.checkpointTask(ctx, &task); err == nil {
		t.Fatal("checkpoint accepted an injected refusal")
	}
	app.gate.Lock()
	app.finishTaskWorker(task.ID, errors.New("Storage unavailable"))
	app.gate.Unlock()
	app.runtimeMu.Lock()
	blocked := app.runtime.activeRecoveryError != nil && app.runtime.settlements[task.ID] != nil && app.runtime.tasks[task.ID] == nil
	app.runtimeMu.Unlock()
	if !blocked {
		t.Fatal("refused evidence lost its recovery owner or admission barrier")
	}
	if err := app.tick(); err == nil {
		t.Fatal("recovery cleared a refused settlement")
	}
	_, err = db.Exec("DROP TRIGGER refuse_task_writes")
	must0(t, err)
	must0(t, app.tick())
	saved := loadTask(t, f.state, task.ID)
	if saved.Status != model.StatusBlocked || *saved.RepairRounds != 1 || saved.Sessions[0].Status != model.SessionCompleted {
		t.Fatal("recovery lost completed repair evidence")
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, "Storage unavailable") {
		t.Fatal("recovery lost the original worker diagnostic")
	}
	app.runtimeMu.Lock()
	cleared := app.runtime.activeRecoveryError == nil && len(app.runtime.settlements) == 0
	app.runtimeMu.Unlock()
	if !cleared {
		t.Fatal("successful settlement left admission blocked")
	}
}

func TestWorkerCheckpointCannotChangeAttemptPolicy(t *testing.T) {
	t.Parallel()
	f, app, task := repairCheckpointFixture(t)
	ctx := testTaskWorker(t, app, &task)
	task.Attempts++
	if err := app.checkpointTask(ctx, &task); err == nil {
		t.Fatal("worker checkpoint changed operator attempt accounting")
	}
	if saved := loadTask(t, f.state, task.ID); saved.Attempts != 0 {
		t.Fatal("refused checkpoint changed canonical attempt accounting")
	}
	// A failure settlement retains evidence without adopting unauthorized intent.
	app.gate.Lock()
	app.finishTaskWorker(task.ID, store.ErrTaskConflict)
	app.gate.Unlock()
	if saved := loadTask(t, f.state, task.ID); saved.Attempts != 0 || saved.Status != model.StatusBlocked {
		t.Fatal("failure settlement adopted worker-owned policy changes")
	}
}

func TestInvalidWorkerEvidenceDoesNotTrapRecovery(t *testing.T) {
	t.Parallel()
	f, app, task := repairCheckpointFixture(t)
	ctx := testTaskWorker(t, app, &task)
	task.Sessions[0].ID = "changed-session"
	err := app.checkpointTask(ctx, &task)
	if !errors.Is(err, store.ErrTaskInvalid) {
		t.Fatalf("invalid session checkpoint: %v", err)
	}
	app.gate.Lock()
	app.finishTaskWorker(task.ID, err)
	app.gate.Unlock()
	saved := loadTask(t, f.state, task.ID)
	if saved.Status != model.StatusBlocked || saved.Sessions[0].ID != "repair-1" || saved.Sessions[0].Status == model.SessionRunning {
		t.Fatalf("invalid evidence replaced canonical settlement: %+v", saved)
	}
	app.runtimeMu.Lock()
	blocked := app.runtime.activeRecoveryError != nil || len(app.runtime.settlements) != 0
	app.runtimeMu.Unlock()
	if blocked {
		t.Fatal("invalid evidence left a permanent recovery barrier")
	}
}

func TestReleasedWorkerCannotDeferReplacementOnShutdown(t *testing.T) {
	t.Parallel()
	f, app, task := repairCheckpointFixture(t)
	old := testTaskWorker(t, app, &task)
	app.gate.Lock()
	app.finishTaskWorker(task.ID, errors.New("First worker stopped"))
	app.gate.Unlock()
	replacement := loadTask(t, f.state, task.ID)
	replacement.Status, replacement.Error, replacement.BlockedReason = model.StatusExecuting, nil, nil
	putTask(t, f, replacement)
	fresh := testTaskWorker(t, app, &replacement)
	app.cancel()
	if err := app.superviseTask(old, task); err == nil || !strings.Contains(err.Error(), "no longer owns") {
		t.Fatalf("released worker shutdown callback: %v", err)
	}
	if saved := loadTask(t, f.state, task.ID); saved.Status != model.StatusExecuting || saved.Error != nil {
		t.Fatal("released worker changed replacement admission")
	}
	must0(t, app.superviseTask(fresh, replacement))
	if saved := loadTask(t, f.state, task.ID); saved.Status != model.StatusQueued || saved.Attempts != 0 {
		t.Fatal("owning worker did not defer admission without charging a retry")
	}
}
