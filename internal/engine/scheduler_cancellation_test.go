package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

// A connection-local view or trigger invokes this hook during a real store
// operation, so cancellation lands after a scheduler check without timing sleeps.
func cancellationStore(t *testing.T) (*store.Store, string, *context.CancelFunc) {
	t.Helper()
	name := "cancel_scheduler_" + strings.ReplaceAll(model.ID(), "-", "")
	var cancel context.CancelFunc
	if err := sqlite.RegisterScalarFunction(name, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		cancel()
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	return testStore(t), name, &cancel
}

func schedulerSQL(t *testing.T, state *store.Store, statement string) {
	t.Helper()
	if err := state.Snapshot(func(conn *sql.Conn) error {
		_, err := conn.ExecContext(store.Background(), statement)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerCancellationAfterStoreReadsPreservesQueuedWorkAndControl(t *testing.T) {
	// Register SQLite hooks before the package's parallel database tests start.
	for _, stage := range []string{"task scan", "run completion", "invalid cycle", "planning capacity", "reserved admission", "PR refresh", "settings read"} {
		t.Run(stage, func(t *testing.T) {
			state, hook, cancel := cancellationStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			control.NextCycleAt = time.Now().Unix() + 3600
			task := queuedTask(cfg, "preserved", "tyk/existing", "tyk/existing")
			table := "record_meta"
			switch stage {
			case "run completion":
				control.SetMode(model.OperatingModeRunOnce)
				control.Batch = &model.RunBatch{ID: model.ID(), Phase: model.BatchPhaseExecuting}
				task.Status = model.StatusPublished
				task.RunID = &control.Batch.ID
				table = "batch_members"
			case "settings read":
				table = "records"
			case "reserved admission", "PR refresh":
				task.Proposal.Target = cfg.DefaultBranch
				table = "pr_reservations"
			case "invalid cycle":
				task.Proposal.Dependencies = []string{"missing"}
			case "planning capacity":
				cfg.MaxSessionsPerDay = 1
				control.SetMode(model.OperatingModeRunOnce)
				control.Batch = &model.RunBatch{ID: model.ID(), Phase: model.BatchPhaseDraining}
				table = "usage"
				schedulerSQL(t, state, fmt.Sprintf("INSERT INTO usage VALUES ('%s',0)", model.Today()))
			}
			saveSettings(t, state, cfg, control)
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			if stage == "reserved admission" || stage == "PR refresh" {
				if err := state.SeedPrReservation(task); err != nil {
					t.Fatal(err)
				}
			}
			started := make(chan struct{}, 1)
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, _ model.Task) error {
				started <- struct{}{}
				return ctx.Err()
			})))
			t.Cleanup(app.Shutdown)
			if stage != "settings read" {
				deferHousekeeping(app)
			}
			*cancel = app.cancel
			schedulerSQL(t, state, fmt.Sprintf("CREATE TEMP VIEW %s AS SELECT * FROM main.%s WHERE %s()=1", table, table, hook))
			var err error
			switch stage {
			case "invalid cycle":
				_, err = app.validateQueuedCycles([]model.Task{task})
			case "planning capacity":
				err = app.maybePlan(cfg, control)
			case "PR refresh":
				app.startPrRefresh(cfg)
			case "reserved admission":
				app.gate.Lock()
				_, _, err = app.dispatch(cfg, control, []model.Task{task})
				app.gate.Unlock()
			default:
				err = app.Tick()
			}
			if err != nil {
				t.Fatal(err)
			}
			if app.Context().Err() != context.Canceled {
				t.Fatal("fixture did not cancel inside the store read")
			}
			app.Shutdown()
			if stage == "PR refresh" && !app.runtime.lastPrAttempt.IsZero() {
				t.Error("cancelled capacity read started a PR refresh")
			}
			if stage == "settings read" && (!app.runtime.lastRetention.IsZero() || !app.runtime.lastObserve.IsZero()) {
				t.Error("cancelled settings read started housekeeping")
			}
			if len(started) != 0 {
				t.Error("scheduler dispatched a worker after the store read was cancelled")
			}
			if saved := loadTask(t, state, task.ID); !wirejson.Equal(saved, task) {
				t.Errorf("cancelled %s changed task status from %s to %s", stage, task.Status, saved.Status)
			}
			if live, err := app.Control(); err != nil || !wirejson.Equal(live, control) {
				t.Errorf("cancelled %s changed control: %+v, %v", stage, live, err)
			}
			if events, err := state.Events(nil); err != nil || len(events) != 0 {
				t.Errorf("cancelled %s recorded events: %+v, %v", stage, events, err)
			}
		})
	}
}

func TestDispatchCancellationStopsLaterTaskAdmissions(t *testing.T) {
	// Register SQLite hooks before the package's parallel database tests start.
	for _, target := range []string{"existing branch", "new PR"} {
		t.Run(target, func(t *testing.T) {
			state, hook, cancel := cancellationStore(t)
			cfg := testConfig(t.TempDir())
			cfg.ExecutionConcurrency = 2
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			saveSettings(t, state, cfg, control)
			first := queuedTask(cfg, "first", "tyk/first", "tyk/first")
			second := queuedTask(cfg, "second", "tyk/second", "tyk/second")
			if target == "new PR" {
				first.Proposal.Target, second.Proposal.Target = cfg.DefaultBranch, cfg.DefaultBranch
			}
			for _, task := range []model.Task{first, second} {
				if err := state.Put("task", task.ID, task); err != nil {
					t.Fatal(err)
				}
			}
			started := make(chan string, 2)
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, task model.Task) error {
				started <- task.ID
				return ctx.Err()
			})))
			t.Cleanup(app.Shutdown)
			*cancel = app.cancel
			if target == "new PR" {
				inventory := model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: []model.PullRequest{}}
				if persisted, err := state.PersistPrInventory(inventory, nil); err != nil || !persisted {
					t.Fatalf("inventory: %t, %v", persisted, err)
				}
				app.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory, fetchedAt: time.Now()}
			}
			// The first admission began before cancellation and may complete. No
			// later task in the same dispatch pass may begin an admission.
			schedulerSQL(t, state, fmt.Sprintf(`CREATE TEMP TRIGGER cancel_after_first_admission AFTER UPDATE ON records
				WHEN NEW.kind='task' AND NEW.id='first' AND json_extract(NEW.data,'$.status')='executing'
				BEGIN SELECT %s(); END`, hook))
			app.gate.Lock()
			admitted, _, err := app.dispatch(cfg, control, []model.Task{first, second})
			app.gate.Unlock()
			if err != nil || !admitted {
				t.Fatalf("first admission: %t, %v", admitted, err)
			}
			if app.Context().Err() != context.Canceled {
				t.Fatal("fixture did not cancel during the first admission")
			}
			app.Shutdown()
			if len(started) != 1 || <-started != first.ID {
				t.Error("dispatch started a later worker after cancellation")
			}
			if saved := loadTask(t, state, second.ID); !wirejson.Equal(saved, second) {
				t.Errorf("dispatch changed the later queued task to %s", saved.Status)
			}
			if reserved, err := state.HasPrReservation(second.ID); err != nil || reserved {
				t.Errorf("later task acquired a PR reservation: %t, %v", reserved, err)
			}
		})
	}
}

func TestDispatchCancellationBeforeWorkerStartsPreservesRestartEligibility(t *testing.T) {
	// Register SQLite hooks before the package's parallel database tests start.
	for _, target := range []string{"existing branch", "new PR"} {
		t.Run(target, func(t *testing.T) {
			state, hook, cancel := cancellationStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			saveSettings(t, state, cfg, control)
			task := queuedTask(cfg, "newly-admitted", "tyk/existing", "tyk/existing")
			if target == "new PR" {
				task.Proposal.Target = cfg.DefaultBranch
			}
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			*cancel = app.cancel
			if target == "new PR" {
				inventory := model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: []model.PullRequest{}}
				if persisted, err := state.PersistPrInventory(inventory, nil); err != nil || !persisted {
					t.Fatalf("inventory: %t, %v", persisted, err)
				}
				app.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory, fetchedAt: time.Now()}
			}
			schedulerSQL(t, state, fmt.Sprintf(`CREATE TEMP TRIGGER cancel_during_admission AFTER UPDATE ON records
				WHEN NEW.kind='task' AND NEW.id='newly-admitted' AND json_extract(NEW.data,'$.status')='executing'
				BEGIN SELECT %s(); END`, hook))
			app.gate.Lock()
			admitted, _, err := app.dispatch(cfg, control, []model.Task{task})
			app.gate.Unlock()
			if err != nil || !admitted || app.Context().Err() != context.Canceled {
				t.Fatalf("cancelled admission: admitted %t, context %v, error %v", admitted, app.Context().Err(), err)
			}
			app.Shutdown()
			stopped := loadTask(t, state, task.ID)
			if stopped.Status != model.StatusQueued || stopped.Error != nil || stopped.BlockedReason != nil || stopped.Attempts != task.Attempts {
				t.Fatalf("unstarted task lost restart eligibility: status %s, attempts %d, error %q", stopped.Status, stopped.Attempts, optionalText(stopped.Error))
			}
			if stopped.Workspace != "" || stopped.ExecutionSession != nil || len(stopped.Sessions) != 0 || !app.Drained() {
				t.Fatalf("unstarted task retained work: %+v; drained %t", stopped, app.Drained())
			}
			if reserved, err := state.HasPrReservation(task.ID); err != nil || reserved != (target == "new PR") {
				t.Fatalf("admission reservation: %t, %v", reserved, err)
			}
			restarted := New(state, app.DataDir)
			t.Cleanup(restarted.Shutdown)
			if err := restarted.Recover(); err != nil {
				t.Fatal(err)
			}
			if recovered := loadTask(t, state, task.ID); !wirejson.Equal(recovered, stopped) {
				t.Fatalf("restart changed the deferred task: %+v", recovered)
			}
		})
	}
}

func TestUnstartedTaskDeferralPreservesOperatorCancellation(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"cancel marker", "cancelled record"} {
		t.Run(stage, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			task := queuedTask(cfg, model.ID(), "tyk/existing", "tyk/existing")
			task.Status = model.StatusExecuting
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			app := New(state, t.TempDir())
			t.Cleanup(app.Shutdown)
			app.gate.Lock()
			app.cancel()
			app.runTask(task)
			if stage == "cancel marker" {
				if err := state.MarkCancel(task.ID); err != nil {
					app.gate.Unlock()
					t.Fatal(err)
				}
			} else if cancelled, err := state.CancelTask(task.ID); err != nil || !cancelled {
				app.gate.Unlock()
				t.Fatalf("cancel task: %t, %v", cancelled, err)
			}
			app.gate.Unlock()
			app.Shutdown()
			if stopped := loadTask(t, state, task.ID); stopped.Status != model.StatusCancelled || stopped.Attempts != task.Attempts {
				t.Fatalf("unstarted task overwrote %s: %+v", stage, stopped)
			}
		})
	}
}
