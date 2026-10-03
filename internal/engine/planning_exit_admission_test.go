package engine

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

func TestPlanningWorkerExitBlocksWaitingAdmissionsBeforeTick(t *testing.T) {
	// Register SQLite functions before parallel package tests start. The hook
	// holds the real worker's final control write while it owns the engine gate.
	for _, action := range []string{"audit", "cycle", "resume", "direct audit", "final preflight"} {
		t.Run(action, func(t *testing.T) {
			entered := make(chan model.Control, 1)
			release := make(chan struct{})
			releaseWorker := sync.OnceFunc(func() { close(release) })
			defer releaseWorker()
			var holdOnce sync.Once
			hook := "hold_planning_exit_" + strings.ReplaceAll(model.ID(), "-", "")
			if err := sqlite.RegisterScalarFunction(hook, 1, func(_ *sqlite.FunctionContext, values []driver.Value) (driver.Value, error) {
				var control model.Control
				if err := json.Unmarshal([]byte(values[0].(string)), &control); err != nil {
					return nil, err
				}
				holdOnce.Do(func() {
					entered <- control
					<-release
				})
				return int64(1), nil
			}); err != nil {
				t.Fatal(err)
			}
			fixture := newScriptedPlanningFixture(t)
			completePlan(t, fixture).queue(fixture)
			completePlan(t, fixture).queue(fixture)
			app := fixture.pausedApp(t)
			schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_exit_terminal BEFORE UPDATE ON records
				WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')!='running'
				BEGIN SELECT RAISE(ABORT, 'synthetic terminal checkpoint refusal bearer fixtureworkersecret123'); END`)
			schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER hold_worker_control BEFORE UPDATE ON records
				WHEN NEW.kind='settings' AND NEW.id='control' AND json_extract(NEW.data,'$.error') LIKE '%%synthetic terminal checkpoint refusal%%'
				BEGIN SELECT %s(NEW.data); END`, hook))
			id, err := app.StartAudit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			expectedControl := waitPlanningStorage(t, entered)
			assertNoOpenClients(t, fixture.script)
			result := make(chan error, 1)
			waiting := make(chan struct{})
			go func() {
				close(waiting)
				var err error
				switch action {
				case "direct audit":
					_, err = app.StartAudit(context.Background())
				case "final preflight":
					app.gate.Lock()
					_, err = app.beginCycle(fixture.cfg, expectedControl, model.CycleModeAudit)
					app.gate.Unlock()
				default:
					_, err = app.ControlAction(action)
				}
				result <- err
			}()
			<-waiting
			releaseWorker()
			// No Tick or fail call may repair the worker's admission boundary.
			requireRecoveryConflict(t, waitPlanningStorage(t, result))
			app.wg.Wait()
			if !app.runtimeIdle() {
				t.Error("worker did not release runtime ownership after cleanup")
			}
			if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, expectedControl) {
				t.Errorf("waiting %s changed the worker's saved control: %+v, %v", action, saved, err)
			}
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 1 || cycles[0].ID != id || cycles[0].Status != model.CycleRunning {
				t.Errorf("waiting %s admitted another cycle before recovery: count=%d, error=%v", action, len(cycles), err)
			}
			assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "worker exit cannot admit another pass before Tick")
			view, err := app.StateView()
			if err != nil {
				t.Fatal(err)
			}
			cause, ok := view["recovery_error"].(*string)
			if !ok || cause == nil || !strings.Contains(*cause, "synthetic terminal checkpoint refusal") || strings.Contains(*cause, "fixtureworkersecret123") {
				t.Errorf("worker exit did not expose its bounded recovery cause: %+v", cause)
			}
			if t.Failed() {
				return
			}
			if _, err := app.ControlAction("pause"); err != nil {
				t.Fatalf("worker recovery blocked pause: %v", err)
			}
			schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_exit_terminal")
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			assertActiveRecoveryCause(t, app, "")
			if _, err := app.StartAudit(context.Background()); err != nil {
				t.Fatalf("healed worker checkpoint still blocked audit: %v", err)
			}
			app.wg.Wait()
			assertAdmissions(t, fixture.state, 2*fixture.cfg.PlanningAdmissionsRequired(), "only the explicit audit after healing may run")
			assertNoOpenClients(t, fixture.script)
		})
	}
}

func TestOrdinaryPlanningFailureWithSavedTerminalStateDoesNotBlockRecovery(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	fixture.script.Queue(fixture.routes.Orchestrator, runnertest.Reply{Err: errors.New("synthetic ordinary grounding failure")})
	app := fixture.pausedApp(t)
	id, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	cycle, err := store.Get[model.Cycle](fixture.state, "cycle", id)
	if err != nil || cycle == nil || cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "synthetic ordinary grounding failure") {
		t.Fatalf("ordinary failure did not save its terminal state: %+v, %v", cycle, err)
	}
	assertActiveRecoveryCause(t, app, "")
	completePlan(t, fixture).queue(fixture)
	if _, err := app.StartAudit(context.Background()); err != nil {
		t.Fatalf("saved ordinary failure unnecessarily blocked the next audit: %v", err)
	}
	app.wg.Wait()
	assertAdmissions(t, fixture.state, 1+fixture.cfg.PlanningAdmissionsRequired(), "saved failure permits a fresh explicit audit without recovery")
	assertNoOpenClients(t, fixture.script)
}

func TestPlanningWorkerExitBlocksResumeUntilBatchPauseSettles(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	fixture.script.Queue(fixture.routes.Orchestrator, runnertest.Reply{Err: errors.New("synthetic ordinary grounding failure")})
	app := fixture.pausedApp(t)
	schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_exit_pause BEFORE UPDATE ON records
		WHEN NEW.kind='settings' AND NEW.id='control' AND json_extract(NEW.data,'$.mode')='paused'
		BEGIN SELECT RAISE(ABORT, 'synthetic planning pause checkpoint refusal'); END`)
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModeRunOnce || control.Batch == nil || control.Batch.Phase != model.BatchPhasePlanning || !app.runtimeIdle() {
		t.Fatalf("worker did not retain its unsettled planning batch: %+v, %v", control, err)
	}
	cycles, err := store.List[model.Cycle](fixture.state, "cycle")
	if err != nil || len(cycles) != 1 || cycles[0].Status != model.CycleFailed {
		t.Fatalf("fixture did not save the failed cycle before refusing its batch pause: %+v, %v", cycles, err)
	}
	// No recovery Tick has run since the worker exited.
	_, err = app.ControlAction("resume")
	requireRecoveryConflict(t, err)
	if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
		t.Errorf("resume erased the unsettled planning batch: %+v, %v", saved, err)
	}
	assertAdmissions(t, fixture.state, 1, "an unsettled batch cannot admit more planning")
	if t.Failed() {
		return
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_exit_pause")
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if saved, err := app.Control(); err != nil || saved.Mode != model.OperatingModePaused || saved.Batch != nil {
		t.Fatalf("healing did not settle the workerless batch: %+v, %v", saved, err)
	}
	assertActiveRecoveryCause(t, app, "")
	assertNoOpenClients(t, fixture.script)
}

func TestPlanningWorkerExitControlReadFailureBlocksAdmissionBeforeTick(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"audit", "run once", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			grounding := runnertest.NewGate()
			fixture.script.Queue(fixture.routes.Orchestrator, runnertest.Reply{Gate: grounding, Err: errors.New("synthetic ordinary grounding failure")})
			app := fixture.pausedApp(t)
			t.Cleanup(grounding.Release)
			// The terminal cycle write succeeds, but its trigger makes only the
			// following control read fail typed decoding. Restoring the exact saved
			// control below simulates reads healing before the first recovery Tick.
			schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_exit_control_read AFTER UPDATE ON records
				WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')='failed'
				BEGIN UPDATE records SET data=json_set(data,'$.next_cycle_at','synthetic final-control-read refusal')
				WHERE kind='settings' AND id='control'; END`)
			switch mode {
			case "audit":
				if _, err := app.StartAudit(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "run once":
				if err := app.RunOnce(); err != nil {
					t.Fatal(err)
				}
			case "continuous":
				if err := app.Resume(); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "audit" {
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
			}
			waitPlanningStorage(t, grounding.Entered())
			before, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			before.NextCycleAt = time.Now().Add(time.Hour).Unix()
			before.Error = stringPointer("earlier saved failure")
			if err := fixture.state.SaveControl(before); err != nil {
				t.Fatal(err)
			}
			grounding.Release()
			app.wg.Wait()
			_, loadErr := app.Control()
			if loadErr == nil || !strings.Contains(loadErr.Error(), "next_cycle_at") || !app.runtimeIdle() {
				t.Fatalf("fixture did not fail the released worker's final control read: %v, idle=%t", loadErr, app.runtimeIdle())
			}
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 1 || cycles[0].Status != model.CycleFailed {
				t.Fatalf("terminal cycle write failed instead of the final control read: %+v, %v", cycles, err)
			}
			schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_exit_control_read")
			if err := fixture.state.SaveControl(before); err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"audit", "cycle", "resume"} {
				_, err := app.ControlAction(action)
				requireRecoveryConflict(t, err)
			}
			if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, before) {
				t.Errorf("a control changed the unsettled policy before recovery: %+v, %v; want %+v", saved, err, before)
			}
			assertAdmissions(t, fixture.state, 1, "final control read failure cannot admit another planning pass")
			if t.Failed() {
				return
			}
			assertActiveRecoveryCause(t, app, loadErr.Error())
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			assertActiveRecoveryCause(t, app, "")
			healed, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			if mode == "run once" {
				if healed.Mode != model.OperatingModePaused || healed.Batch != nil || healed.NextCycleAt != before.NextCycleAt || healed.Error == nil || *healed.Error != "Run once was interrupted before its planning transaction committed" {
					t.Fatalf("healing did not settle the workerless planning batch: %+v", healed)
				}
			} else if !wirejson.Equal(healed, before) {
				t.Fatalf("recovery changed the saved ordinary mode, error, or schedule: %+v; want %+v", healed, before)
			}
			if _, err := app.ControlAction("resume"); err != nil {
				t.Fatalf("healed final control read still blocked resume: %v", err)
			}
			assertAdmissions(t, fixture.state, 1, "control settlement must not replay planning")
			assertNoOpenClients(t, fixture.script)
		})
	}
}
