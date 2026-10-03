package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

func TestCancelledTaskFinalizationRecoversAfterRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			testCancelledTaskFinalizationRecovery(t, restart)
		})
	}
}

func testCancelledTaskFinalizationRecovery(t *testing.T, restart bool) {
	t.Helper()
	fixture := newScriptedFixture(t)
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Answer: "Never delivered", Gate: gate})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)
	tickUntil(t, app, gate.Entered(), "held executor turn")
	// CancelTask changes only status and updated_at. Refuse session finalization
	// regardless of whether the operator or worker writes the cancelled status
	// first; the worker can finish before CancelTask acquires the store lock.
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER refuse_cancel_finalization BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='cancelled'
			AND json_extract(NEW.data,'$.sessions') IS NOT json_extract(OLD.data,'$.sessions')
		BEGIN SELECT RAISE(ABORT, 'synthetic cancellation finalization refusal'); END`, task.ID))
	if err := app.TaskAction(context.Background(), task.ID, "cancel"); err != nil {
		t.Fatalf("operator cancel: %v", err)
	}
	app.wg.Wait()
	if err := app.Pause(); err != nil {
		t.Fatal(err)
	}
	before := loadTask(t, fixture.state, task.ID)
	if before.Status != model.StatusCancelled || len(before.Sessions) != 1 || before.Sessions[0].Status != model.SessionRunning {
		t.Fatalf("refused finalization state: status=%s sessions=%+v", before.Status, before.Sessions)
	}
	if !hasEvent(t, fixture.state, task.ID, "worker_error", "synthetic cancellation finalization refusal") {
		t.Fatal("worker finalization refusal was not recorded")
	}
	if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "synthetic cancellation finalization refusal") {
		t.Fatalf("recovery while storage still refuses = %v", err)
	}
	if saved := loadTask(t, fixture.state, task.ID); !wirejson.Equal(saved, before) {
		t.Fatal("refused recovery changed the cancelled task")
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_cancel_finalization")
	if restart {
		app.Shutdown()
		app = fixture.pausedApp(t)
		if err := app.Recover(); err != nil {
			t.Fatal(err)
		}
	} else if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	after := loadTask(t, fixture.state, task.ID)
	if after.Status != model.StatusCancelled || len(after.Sessions) != 1 || after.Sessions[0].Status != model.SessionInterrupted {
		t.Fatalf("cancelled task retained an orphan running session after restart: status=%s sessions=%+v", after.Status, after.Sessions)
	}
	expected := before.Clone()
	model.InterruptRunning(expected.Sessions)
	expected.UpdatedAt = after.UpdatedAt
	if !wirejson.Equal(after, expected) {
		t.Fatal("recovery changed cancellation state beyond its running session and update timestamp")
	}
	if reserved, err := fixture.state.HasPrReservation(task.ID); err != nil || reserved {
		t.Fatalf("cancelled task reservation = %t, %v", reserved, err)
	}
	if marked, err := fixture.state.MarkerSet("cancel", task.ID); err != nil || !marked {
		t.Fatalf("cancel marker = %t, %v", marked, err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if saved := loadTask(t, fixture.state, task.ID); !wirejson.Equal(saved, after) {
		t.Fatal("a finalized cancelled task changed on the next recovery pass")
	}
	assertAdmissions(t, fixture.state, 1, "restart must settle cancellation without another turn")
	assertUnpublished(t, fixture, after)
	assertNoOpenClients(t, fixture.script)
}

func TestCancelledSessionRecoveryPreservesOwnedAndFinalizedTasks(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	var tasks []model.Task
	for _, id := range []string{"live", "cleanup", "archived", "finalized"} {
		task := queuedTask(cfg, id, cfg.DefaultBranch, "tyk/"+id)
		task.Status = model.StatusCancelled
		task.Sessions = []model.Session{
			{ID: "done", Status: model.SessionCompleted, Summary: "completed evidence"},
			{ID: "failed", Status: model.SessionFailed, Summary: "failed evidence"},
			{ID: "interrupted", Status: model.SessionInterrupted, Summary: "interrupted evidence"},
		}
		if id != "finalized" {
			task.Sessions = append(task.Sessions, model.Session{ID: "unfinished", Status: model.SessionRunning})
		}
		if id == "archived" {
			task.Lifecycle.ArchivedAt = stringPointer(model.Now())
			task.Lifecycle.DiscardedAt = stringPointer(model.Now())
		}
		if err := state.Put("task", id, task); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	app.runtime.tasks["live"] = taskJob{}
	if !app.claimCleanup(cleanupTask, "cleanup") {
		t.Fatal("claim cleanup")
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		saved := loadTask(t, state, task.ID)
		if task.ID == "archived" {
			model.InterruptRunning(task.Sessions)
			task.UpdatedAt = saved.UpdatedAt
		}
		if !wirejson.Equal(saved, task) {
			t.Fatalf("unexpected recovery of %s: %+v", task.ID, saved)
		}
	}
	delete(app.runtime.tasks, "live")
	app.releaseCleanup(cleanupTask, "cleanup")
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live", "cleanup"} {
		if saved := loadTask(t, state, id); saved.Sessions[3].Status != model.SessionInterrupted {
			t.Fatalf("released %s claim still prevents session recovery", id)
		}
	}
}

func TestCancelledSessionRecoveryDoesNotRescanFinalizedHistoryEachTick(t *testing.T) {
	var taskReads atomic.Uint32
	hook := "cancelled_task_read_" + strings.ReplaceAll(model.ID(), "-", "")
	if err := sqlite.RegisterScalarFunction(hook, 3, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if args[0] == "task" {
			taskReads.Add(1)
		}
		return args[2], nil
	}); err != nil {
		t.Fatal(err)
	}
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	task := queuedTask(cfg, "finalized-history", cfg.DefaultBranch, "tyk/finalized-history")
	task.Status = model.StatusCancelled
	task.Sessions = []model.Session{{ID: "done", Status: model.SessionFailed}}
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	// Observe actual JSON payload reads without relying on wall-clock timing.
	schedulerSQL(t, state, fmt.Sprintf(`CREATE TEMP VIEW records AS
		SELECT kind,id,%s(kind,id,data) AS data FROM main.records`, hook))
	defer schedulerSQL(t, state, "DROP VIEW records")
	for range 3 {
		if err := app.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	if taskReads.Load() != 0 {
		t.Fatalf("finalized cancellation history reread on ordinary ticks: %d", taskReads.Load())
	}
	if !app.claimCleanup(cleanupTask, task.ID) {
		t.Fatal("claim task cleanup")
	}
	app.releaseCleanup(cleanupTask, task.ID)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if taskReads.Load() == 0 {
		t.Fatal("cleanup release did not invalidate cancellation recovery")
	}
	taskReads.Store(0)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if taskReads.Load() != 0 {
		t.Fatal("successful recheck did not stop recurring cancellation history reads")
	}
}

func TestCancelledSessionRecoveryRechecksOperatorTerminalTransitions(t *testing.T) {
	for _, action := range []string{"cancel", "archive", "supersede"} {
		t.Run(action, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			saveSettings(t, state, cfg, model.DefaultControl())
			task := queuedTask(cfg, "orphaned-session", cfg.DefaultBranch, "tyk/orphaned-session")
			task.Status = model.StatusBlocked
			task.BlockedReason = blockedReasonPtr(model.BlockedReasonRunnerUnavailable)
			task.Sessions = []model.Session{{ID: "unfinished", Status: model.SessionRunning}}
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			if err := app.TaskAction(context.Background(), task.ID, action); err != nil {
				t.Fatal(err)
			}
			before := loadTask(t, state, task.ID)
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			after := loadTask(t, state, task.ID)
			expected := before.Clone()
			model.InterruptRunning(expected.Sessions)
			expected.UpdatedAt = after.UpdatedAt
			if after.Sessions[0].Status != model.SessionInterrupted || !wirejson.Equal(after, expected) {
				t.Fatalf("operator %s did not invalidate cancellation recovery: %+v", action, after)
			}
		})
	}
}

func TestTaskExitFallbackFinalizesSessionsAfterOneShotWriteRefusal(t *testing.T) {
	var blockedWrites atomic.Uint32
	hook := "task_terminal_write_" + strings.ReplaceAll(model.ID(), "-", "")
	if err := sqlite.RegisterScalarFunction(hook, 0, func(_ *sqlite.FunctionContext, _ []driver.Value) (driver.Value, error) {
		return int64(blockedWrites.Add(1)), nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture := newScriptedFixture(t)
	fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Err: errors.New("synthetic executor failure")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusExecuting
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.pausedApp(t)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER refuse_first_blocked_write BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='blocked' AND %s()=1
		BEGIN SELECT RAISE(ABORT, 'synthetic one-shot terminal refusal'); END`, task.ID, hook))
	app.runTask(task)
	app.wg.Wait()
	saved := loadTask(t, fixture.state, task.ID)
	if blockedWrites.Load() != 2 || saved.Status != model.StatusBlocked {
		t.Fatalf("fallback writes=%d status=%s", blockedWrites.Load(), saved.Status)
	}
	if !hasEvent(t, fixture.state, task.ID, "worker_error", "synthetic one-shot terminal refusal") {
		t.Fatal("supervisor terminal write refusal was not recorded")
	}
	if len(saved.Sessions) != 1 || saved.Sessions[0].Status != model.SessionFailed || saved.Sessions[0].Summary == "" {
		t.Fatalf("fallback retained an orphan running session after worker exit: %+v", saved.Sessions)
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 1, "fallback must finalize without another model turn")
	assertNoOpenClients(t, fixture.script)
}
