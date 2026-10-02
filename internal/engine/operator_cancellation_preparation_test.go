package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func refuseQueuedCancellation(t *testing.T, app *App, state *store.Store, id string) {
	t.Helper()
	schedulerSQL(t, state, fmt.Sprintf(`CREATE TEMP TRIGGER fail_cancel BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='cancelled'
		BEGIN SELECT RAISE(ABORT, 'synthetic cancellation refusal'); END`, id))
	if err := app.TaskAction(context.Background(), id, "cancel"); err == nil || !strings.Contains(err.Error(), "synthetic cancellation refusal") {
		t.Fatalf("cancel error = %v", err)
	}
	schedulerSQL(t, state, "DROP TRIGGER fail_cancel")
}

func cancellationEvents(t *testing.T, state *store.Store, id string) int {
	t.Helper()
	events, err := state.Events(&id)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Kind == "operator" && event.Message == "cancel" {
			count++
		}
	}
	return count
}

func TestQueuedCancellationPrecedesInvalidPlan(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"unordered writers", "unknown dependency"} {
		t.Run(invalid, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			control.NextCycleAt = int64(^uint64(0) >> 1)
			saveSettings(t, state, cfg, control)
			first := queuedTask(cfg, "cancelled-first", "octomus/existing", "octomus/existing")
			second := queuedTask(cfg, "remaining", "octomus/existing", "octomus/existing")
			if invalid == "unknown dependency" {
				first.Proposal.Dependencies = []string{"missing"}
				second.Proposal.Target, second.Branch = "octomus/other", "octomus/other"
			}
			for _, task := range []model.Task{first, second} {
				if err := state.Put("task", task.ID, task); err != nil {
					t.Fatal(err)
				}
			}
			var dispatched atomic.Int32
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
				dispatched.Add(1)
				return nil
			})))
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			refuseQueuedCancellation(t, app, state, first.ID)
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			if saved := loadTask(t, state, first.ID); saved.Status != model.StatusCancelled || saved.BlockedReason != nil {
				t.Fatalf("plan validation displaced cancellation: status=%s reason=%v", saved.Status, saved.BlockedReason)
			}
			if saved := loadTask(t, state, second.ID); !blockedAs(saved, model.BlockedReasonInvalidPlan) {
				t.Fatalf("remaining invalid plan escaped validation: %+v", saved)
			}
			if count := dispatched.Load(); count != 0 {
				t.Fatalf("invalid plan dispatched %d tasks", count)
			}
		})
	}
}

func TestQueuedCancellationRecordsOperatorEvent(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = int64(^uint64(0) >> 1)
	saveSettings(t, state, cfg, control)
	task := queuedTask(cfg, "pending-cancellation", cfg.DefaultBranch, "octomus/pending")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	refuseQueuedCancellation(t, app, state, task.ID)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if saved := loadTask(t, state, task.ID); saved.Status != model.StatusCancelled {
		t.Fatalf("pending cancellation = %s", saved.Status)
	}
	if count := cancellationEvents(t, state, task.ID); count != 1 {
		t.Fatalf("recovered operator/cancel events = %d; want 1", count)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if count := cancellationEvents(t, state, task.ID); count != 1 {
		t.Fatalf("repeated Tick duplicated cancellation event: %d", count)
	}
}

func TestQueuedCancellationEventRefusalPreservesTerminalState(t *testing.T) {
	t.Parallel()
	for _, recovered := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovered=%t", recovered), func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			control.NextCycleAt = int64(^uint64(0) >> 1)
			saveSettings(t, state, cfg, control)
			task := queuedTask(cfg, "event-refusal", cfg.DefaultBranch, "octomus/pending")
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			if recovered {
				refuseQueuedCancellation(t, app, state, task.ID)
			}
			schedulerSQL(t, state, `CREATE TEMP TRIGGER refuse_cancel_event BEFORE INSERT ON events
				WHEN NEW.kind='operator' AND NEW.message='cancel'
				BEGIN SELECT RAISE(ABORT, 'synthetic cancellation event refusal'); END`)
			var err error
			if recovered {
				err = app.Tick()
			} else {
				err = app.TaskAction(context.Background(), task.ID, "cancel")
			}
			if recovered {
				if err == nil || !strings.Contains(err.Error(), "synthetic cancellation event refusal") {
					t.Fatalf("scheduler cancellation event error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("durable operator cancellation reported failure: %v", err)
			}
			if saved := loadTask(t, state, task.ID); saved.Status != model.StatusCancelled {
				t.Fatalf("event refusal undid durable cancellation: %s", saved.Status)
			}
			schedulerSQL(t, state, "DROP TRIGGER refuse_cancel_event")
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			if err := app.TaskAction(context.Background(), task.ID, "cancel"); !IsActionConflict(err) {
				t.Fatalf("terminal cancellation repeated: %v", err)
			}
			// The scheduler reports its event error; the operator request acknowledges
			// its committed cancellation. Neither path may replay or resume the task.
			if count := cancellationEvents(t, state, task.ID); count != 0 {
				t.Fatalf("refused event was replayed: %d", count)
			}
			assertAdmissions(t, state, 0, "event refusal never resumes the cancelled task")
		})
	}
}

func TestQueuedCancellationAfterCycleValidationFinishesRunOnce(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeRunOnce)
	control.Batch = &model.RunBatch{ID: "cancelled-batch", Phase: model.BatchPhaseExecuting}
	saveSettings(t, state, cfg, control)
	task := queuedTask(cfg, "cached-cancellation", cfg.DefaultBranch, "octomus/pending")
	task.RunID = &control.Batch.ID
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	app.runtime.checkedCycles[task.CycleID] = struct{}{}
	refuseQueuedCancellation(t, app, state, task.ID)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if saved := loadTask(t, state, task.ID); saved.Status != model.StatusCancelled {
		t.Fatalf("checked cycle skipped cancellation: %s", saved.Status)
	}
	if pending, unresolved, err := state.BatchCounts(control.Batch.ID); err != nil || pending != 0 || unresolved != 1 {
		t.Fatalf("cancelled batch counts = %d/%d, %v", pending, unresolved, err)
	}
	if saved, err := app.Control(); err != nil || saved.Mode != model.OperatingModeRunOnce {
		t.Fatalf("cancellation preparation did not yield before batch completion: %+v, %v", saved, err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if saved, err := app.Control(); err != nil || saved.Mode != model.OperatingModePaused || saved.Batch != nil {
		t.Fatalf("cancelled batch did not finish: %+v, %v", saved, err)
	}
	if count := cancellationEvents(t, state, task.ID); count != 1 {
		t.Fatalf("checked-cycle cancellation events = %d", count)
	}
	assertAdmissions(t, state, 0, "cancelled run-once task starts no work")
}

func TestQueuedCancellationAndValidationWaitForCleanup(t *testing.T) {
	t.Parallel()
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("marked=%t", marked), func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			control.NextCycleAt = int64(^uint64(0) >> 1)
			saveSettings(t, state, cfg, control)
			task := queuedTask(cfg, "claimed-invalid", "octomus/existing", "octomus/existing")
			task.Proposal.Dependencies = []string{"missing"}
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			if marked {
				refuseQueuedCancellation(t, app, state, task.ID)
			}
			if !app.claimCleanup(cleanupTask, task.ID) {
				t.Fatal("fresh cleanup claim failed")
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			if saved := loadTask(t, state, task.ID); saved.Status != model.StatusQueued {
				t.Fatalf("preparation changed cleanup-owned task: %s", saved.Status)
			}
			if _, checked := app.runtime.checkedCycles[task.CycleID]; checked {
				t.Fatal("deferred invalid plan was marked checked")
			}
			app.releaseCleanup(cleanupTask, task.ID)
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			saved := loadTask(t, state, task.ID)
			if marked && saved.Status != model.StatusCancelled || !marked && !blockedAs(saved, model.BlockedReasonInvalidPlan) {
				t.Fatalf("released task escaped cancellation or validation: status=%s reason=%v", saved.Status, saved.BlockedReason)
			}
			assertAdmissions(t, state, 0, "cleanup does not bypass validation or cancellation")
		})
	}
}

func TestQueuedCancellationOutsideWindowPrecedesPlanValidation(t *testing.T) {
	t.Parallel()
	testutil.SkipVolumeUnderRace(t)
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = int64(^uint64(0) >> 1)
	saveSettings(t, state, cfg, control)
	var last model.Task
	for i := 0; i < 501; i++ {
		task := queuedTask(cfg, fmt.Sprintf("member-%03d", i), cfg.DefaultBranch, fmt.Sprintf("octomus/member-%03d", i))
		if i == 0 {
			task.Proposal.Dependencies = []string{"missing"}
		}
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
		last = task
	}
	visible, err := state.SchedulingTasks(nil)
	if err != nil || len(visible) != 500 || visible[len(visible)-1].ID == last.ID {
		t.Fatalf("fixture did not exceed the queue window: tasks=%d, %v", len(visible), err)
	}
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	refuseQueuedCancellation(t, app, state, last.ID)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if saved := loadTask(t, state, last.ID); saved.Status != model.StatusCancelled {
		t.Fatalf("out-of-window cancellation lost to validation: %s", saved.Status)
	}
	if saved := loadTask(t, state, visible[0].ID); !blockedAs(saved, model.BlockedReasonInvalidPlan) {
		t.Fatalf("full plan was not validated: %+v", saved)
	}
	if count := cancellationEvents(t, state, last.ID); count != 1 {
		t.Fatalf("out-of-window cancellation events = %d", count)
	}
	assertAdmissions(t, state, 0, "invalid complete plan admits no tasks")
}
