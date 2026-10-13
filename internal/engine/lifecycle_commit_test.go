package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestTaskTransitionCommitOutcome(t *testing.T) {
	t.Parallel()
	for _, refusal := range []string{"task", "activity"} {
		t.Run(refusal, func(t *testing.T) {
			f := newFixture(t)
			task := executionTask(t, f, f.cfg.DefaultBranch)
			task.Status = model.StatusExecuting
			putTask(t, f, task)
			before := task.Clone()
			db := f.rawDB(t)
			statement := `CREATE TRIGGER refuse BEFORE UPDATE ON records WHEN NEW.kind='task'
				BEGIN SELECT RAISE(FAIL,'task checkpoint refused'); END`
			if refusal == "activity" {
				statement = `CREATE TRIGGER refuse BEFORE INSERT ON events WHEN NEW.kind='status'
					BEGIN SELECT RAISE(FAIL,'activity refused'); END`
			}
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			app := f.pausedApp(t)
			err := app.transitionTask(testTaskWorker(t, app, &task), &task, model.StatusReviewing)
			saved := loadTask(t, f.state, task.ID)
			if refusal == "task" {
				if err == nil || !wirejson.Equal(task, before) || !wirejson.Equal(saved, before) {
					t.Fatalf("refused transition changed authority: task=%+v saved=%+v err=%v", task, saved, err)
				}
			} else if err != nil || task.Status != model.StatusReviewing || !wirejson.Equal(saved, task) {
				t.Fatalf("ancillary failure obscured committed transition: task=%+v saved=%+v err=%v", task, saved, err)
			}
		})
	}
}

func TestRecoveryAndCancellationActivityIsAncillary(t *testing.T) {
	for _, name := range []string{"restart recovery", "queued cancellation"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			task := executionTask(t, f, f.cfg.DefaultBranch)
			task.Status = model.StatusExecuting
			if name == "queued cancellation" {
				task.Status = model.StatusQueued
			}
			putTask(t, f, task)
			_, err := f.rawDB(t).Exec(`CREATE TRIGGER refuse_activity BEFORE INSERT ON records WHEN NEW.kind='event' BEGIN SELECT RAISE(ABORT, 'refuse ancillary activity'); END`)
			must0(t, err)
			app := New(f.state, f.dataDir)
			t.Cleanup(app.Shutdown)
			if name == "restart recovery" {
				must0(t, app.Recover())
				if loadTask(t, f.state, task.ID).Status != model.StatusBlocked {
					t.Fatal("recovery checkpoint did not commit")
				}
			} else {
				must0(t, f.state.MarkCancel(task.ID))
				tasks := []model.Task{task}
				changed, err := app.cancelQueuedTasks(tasks)
				must0(t, err)
				if !changed || tasks[0].Status != model.StatusCancelled || loadTask(t, f.state, task.ID).Status != model.StatusCancelled {
					t.Fatal("cancellation checkpoint did not commit")
				}
			}
		})
	}
}

func TestPublicationCommitDoesNotMutateOnFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	task := checkpointedTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusPublishing
	putTask(t, f, task)
	before := task.Clone()
	if _, err := f.rawDB(t).Exec(`CREATE TRIGGER refuse BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status')='published'
		BEGIN SELECT RAISE(FAIL,'publication settlement refused'); END`); err != nil {
		t.Fatal(err)
	}
	app := f.pausedApp(t)
	p := model.PullRequest{Number: 4, URL: "https://github.com/fixture/project/pull/4", Head: *task.OutputCommit}
	if err := app.published(&task, p); err == nil {
		t.Fatal("publication settlement was not refused")
	}
	if !wirejson.Equal(task, before) || !wirejson.Equal(loadTask(t, f.state, task.ID), before) {
		t.Fatal("refused settlement changed caller state or its durable checkpoint")
	}
}

func TestReconciliationSettlementRetainsPublicationFailure(t *testing.T) {
	f := newFixture(t)
	task := checkpointedTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusBlocked
	task.BlockedReason = new(model.BlockedPublicationUncertain)
	putTask(t, f, task)
	_, err := f.rawDB(t).Exec(`CREATE TRIGGER refuse_publication_settlement BEFORE UPDATE OF data ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status')='published' BEGIN SELECT RAISE(ABORT, 'refuse publication settlement'); END`)
	must0(t, err)
	app := New(f.state, f.dataDir)
	t.Cleanup(app.Shutdown)
	err = app.TaskAction(task.ID, model.TaskActionReconcile)
	if err == nil || !strings.Contains(err.Error(), "refuse publication settlement") {
		t.Fatalf("publication settlement failure = %v", err)
	}
	current := loadTask(t, f.state, task.ID)
	if current.Status != model.StatusBlocked || current.BlockedReason == nil || *current.BlockedReason != model.BlockedPublicationUncertain || current.Error == nil || !strings.Contains(*current.Error, "refuse publication settlement") {
		t.Fatalf("settlement lost uncertain outcome or storage diagnostic: %+v", current)
	}
	if current.OutputCommit == nil || !app.Drained() {
		t.Fatal("settlement lost checkpoint or retained worker")
	}
}

func TestExecutionSettlementRetainsPublicationFailure(t *testing.T) {
	f := newFixture(t)
	task := checkpointedTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusExecuting
	putTask(t, f, task)
	_, err := f.rawDB(t).Exec(`CREATE TRIGGER refuse_publication_settlement BEFORE UPDATE OF data ON records WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status')='published' BEGIN SELECT RAISE(ABORT, 'refuse publication settlement'); END`)
	must0(t, err)
	app := New(f.state, f.dataDir)
	t.Cleanup(app.Shutdown)
	must0(t, app.superviseTask(testTaskWorker(t, app, &task), task))
	current := loadTask(t, f.state, task.ID)
	if current.Status != model.StatusBlocked || current.BlockedReason == nil || *current.BlockedReason != model.BlockedPublicationUncertain || current.Error == nil || !strings.Contains(*current.Error, "refuse publication settlement") {
		t.Fatalf("execution lost uncertain outcome or storage diagnostic: %+v", current)
	}
	if current.OutputCommit == nil {
		t.Fatal("execution lost publication checkpoint")
	}
	task.Status = model.StatusPublishing
	recordTaskError(&task, model.BlockedRemoteConflict)
	if task.BlockedReason == nil || *task.BlockedReason != model.BlockedRemoteConflict {
		t.Fatal("known publication refusal became uncertain")
	}
}

func TestRetryCommitsPolicyAndCancelMarkerTogether(t *testing.T) {
	t.Parallel()
	for _, refusal := range []string{"task", "policy"} {
		t.Run(refusal, func(t *testing.T) {
			f := newFixture(t)
			task := checkpointedTask(t, f, f.cfg.DefaultBranch)
			task.Status = model.StatusBlocked
			putTask(t, f, task)
			must0(t, f.state.MarkCancel(task.ID))
			before := task.Clone()
			statement := `CREATE TRIGGER refuse BEFORE UPDATE ON records
				WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status')='queued'
				BEGIN SELECT RAISE(FAIL,'retry refused'); END`
			if refusal == "policy" {
				statement = `CREATE TRIGGER refuse BEFORE INSERT ON events WHEN NEW.kind='attempt_policy'
					BEGIN SELECT RAISE(FAIL,'policy refused'); END`
			}
			_, err := f.rawDB(t).Exec(statement)
			must0(t, err)
			app := f.pausedApp(t)
			if err := app.retryTask(&task); err == nil {
				t.Fatal("retry was not refused")
			}
			marked, err := f.state.CancelMarked(task.ID)
			if err != nil || !marked || !wirejson.Equal(task, before) || !wirejson.Equal(loadTask(t, f.state, task.ID), before) {
				t.Fatalf("refused retry partially committed: marker=%v error=%v task=%+v", marked, err, task)
			}
			events, err := f.state.Events(&task.ID)
			must0(t, err)
			for _, event := range events {
				if event.Kind == model.EventAttemptPolicy {
					t.Fatal("policy evidence committed without its retry")
				}
			}
		})
	}
}

func TestExitedTaskSettlementRetainsRecoveryBarrier(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusExecuting
	task.Sessions = []model.Session{model.NewSession("unfinished", "executor", task.Route)}
	putTask(t, f, task)
	db := f.rawDB(t)
	_, err := db.Exec(`CREATE TRIGGER refuse BEFORE UPDATE ON records WHEN NEW.kind='task'
		BEGIN SELECT RAISE(FAIL,'terminal checkpoint refused'); END`)
	must0(t, err)
	app := f.pausedApp(t)
	deferHousekeeping(app)
	app.gate.Lock()
	_, cancel := app.registerTaskWorker(task, executionWorker)
	cancel()
	app.finishTaskWorker(task.ID, errors.New("worker failed"))
	app.gate.Unlock()
	if app.recoveryConflict() == nil || loadTask(t, f.state, task.ID).Status != model.StatusExecuting {
		t.Fatal("failed settlement released authority without a recovery barrier")
	}
	if err := app.tick(); err == nil || app.recoveryConflict() == nil {
		t.Fatal("refused recovery cleared the barrier")
	}
	_, err = db.Exec("DROP TRIGGER refuse")
	must0(t, err)
	must0(t, app.tick())
	saved := loadTask(t, f.state, task.ID)
	if saved.Status != model.StatusBlocked || saved.Sessions[0].Status != model.SessionFailed || app.recoveryConflict() != nil {
		t.Fatalf("recovered ordinary worker = %+v", saved)
	}
}

func TestOrphanRecoveryDoesNotAdmitBetweenPages(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	for i := 0; i <= store.PageLimit; i++ {
		task := queuedTask(cfg, fmt.Sprintf("orphan-%d", i), "tyk/exited", "tyk/exited")
		task.Status = model.StatusExecuting
		must0(t, store.Put(state, store.Tasks.Key(task.ID), task))
	}
	app := New(state, t.TempDir())
	cleanupApp(t, app)
	deferHousekeeping(app)
	must0(t, app.tick())
	active, err := state.ActiveTasks()
	if err != nil || len(active) != 0 {
		t.Fatalf("recovery admitted with workerless tasks on a later page: %d, %v", len(active), err)
	}
}

func TestBaselineSettlementRetainsRecoveryBarrier(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	check := makeCheck(f.cfg, model.BaselineStatusRunning)
	must0(t, store.Put(f.state, store.Baselines.Key(check.ID), check))
	db := f.rawDB(t)
	_, err := db.Exec(`CREATE TRIGGER refuse_baseline BEFORE UPDATE ON records WHEN NEW.kind='baseline'
		BEGIN SELECT RAISE(FAIL,'baseline settlement refused'); END`)
	must0(t, err)
	app := f.pausedApp(t)
	deferHousekeeping(app)
	ctx, cancel := context.WithCancel(app.ctx)
	cancel()
	app.runtimeMu.Lock()
	app.runtime.baseline = &baselineJob{id: check.ID, cancel: cancel}
	app.runtimeMu.Unlock()
	app.baselineWorker(ctx, check.ID)
	if app.recoveryConflict() == nil || !app.Drained() {
		t.Fatal("baseline released its claim without a recovery barrier")
	}
	if err := app.tick(); err == nil {
		t.Fatal("baseline recovery ignored refused settlement")
	}
	_, err = db.Exec("DROP TRIGGER refuse_baseline")
	must0(t, err)
	must0(t, app.tick())
	saved, err := store.Get(f.state, store.Baselines.Key(check.ID))
	if err != nil || saved == nil || saved.Status != model.BaselineStatusInterrupted || app.recoveryConflict() != nil {
		t.Fatalf("baseline did not recover its terminal state: %+v, %v", saved, err)
	}
}

func TestBrokerReadinessDoesNotBlockPause(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	socket := testutil.UnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}))
	app := f.pausedApp(t, WithSandbox(sandbox.NewRemote(socket)))
	deferHousekeeping(app)
	controlState, err := app.Control()
	must0(t, err)
	controlState.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(controlState))
	done := make(chan error, 1)
	go func() { done <- app.tick() }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tick never reached the broker read")
	}
	paused := make(chan error, 1)
	go func() { paused <- control(app, model.ControlPause) }()
	select {
	case err := <-paused:
		must0(t, err)
	case <-time.After(3 * time.Second):
		app.cancel()
		t.Fatal("broker read blocked operator pause")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("obsolete readiness failure survived pause: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tick did not finish after broker response")
	}
	state, err := app.Control()
	if err != nil || state.Mode != model.OperatingModePaused || !app.Drained() {
		t.Fatalf("stale admission after pause: %+v, %v", state, err)
	}
}
