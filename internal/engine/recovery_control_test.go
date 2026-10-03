package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func refusePlanningRecovery(t *testing.T, app *App) {
	t.Helper()
	schedulerSQL(t, app.Store, `CREATE TEMP TRIGGER refuse_control_recovery BEFORE UPDATE ON records
		WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')='interrupted'
		BEGIN SELECT RAISE(ABORT, 'synthetic control recovery refusal'); END`)
}

func requireRecoveryConflict(t *testing.T, err error) {
	t.Helper()
	if !IsActionConflict(err) || !strings.Contains(err.Error(), "recovery") {
		t.Errorf("admission during recovery = %v; want a recovery conflict", err)
	}
}

func TestRecoveryBlocksControlAdmissionsUntilHealing(t *testing.T) {
	t.Parallel()
	for _, reportFailure := range []bool{false, true} {
		for _, action := range []string{"audit", "cycle", "resume", "direct audit"} {
			t.Run(fmt.Sprintf("%s/reported=%t", action, reportFailure), func(t *testing.T) {
				fixture := newScriptedPlanningFixture(t)
				completePlan(t, fixture).queue(fixture)
				app := fixture.pausedApp(t)
				orphan := groundingCycle(t, fixture, model.CycleModeAudit)
				control := model.DefaultControl()
				control.CycleNumber = orphan.Number
				control.NextCycleAt = time.Now().Add(time.Hour).Unix()
				message := "earlier planning failure"
				control.Error = &message
				if err := fixture.state.SaveControl(control); err != nil {
					t.Fatal(err)
				}
				refusePlanningRecovery(t, app)
				err := app.Tick()
				var recovery *recoveryError
				if !errors.As(err, &recovery) {
					t.Fatalf("recovery write was not refused: %v", err)
				}
				if reportFailure {
					app.fail(err)
				}
				if !app.runtimeIdle() {
					t.Fatal("recovery must not change worker-idle semantics used by configuration and diagnostics")
				}
				start := func() error {
					if action == "direct audit" {
						_, err := app.StartAudit(context.Background())
						return err
					}
					_, err := app.ControlAction(action)
					return err
				}
				requireRecoveryConflict(t, start())
				app.wg.Wait()
				if calls := fixture.script.Calls(); len(calls) != 0 {
					t.Errorf("blocked admission reached runner preflight: %+v", calls)
				}
				if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
					t.Errorf("refused control changed saved mode, batch, error, or backoff: %+v, %v; want %+v", saved, err, control)
				}
				if cycles, err := store.List[model.Cycle](fixture.state, "cycle"); err != nil || len(cycles) != 1 || !wirejson.Equal(cycles[0], orphan) {
					t.Errorf("refused control changed cycle evidence or started another cycle: %+v, %v", cycles, err)
				}
				assertAdmissions(t, fixture.state, 0, "recovery blocks operator admissions before and after failure reporting")
				if t.Failed() {
					return
				}
				assertActiveRecoveryCause(t, app, err.Error())
				schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_control_recovery")
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
				assertActiveRecoveryCause(t, app, "")
				if err := start(); err != nil {
					t.Fatalf("healthy control remained unavailable after recovery: %v", err)
				}
				app.wg.Wait()
				if action == "audit" || action == "direct audit" {
					assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "the explicit healthy audit can start planning")
				}
				assertNoOpenClients(t, fixture.script)
			})
		}
	}
}

func TestRecoveryAllowsPauseWithoutClearingTheBarrier(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	groundingCycle(t, fixture, model.CycleModeAudit)
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = time.Now().Add(time.Hour).Unix()
	message := "earlier failure"
	control.Error = &message
	if err := fixture.state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	refusePlanningRecovery(t, app)
	err := app.Tick()
	if err == nil {
		t.Fatal("recovery write was not refused")
	}
	if _, err := app.ControlAction("pause"); err != nil {
		t.Fatalf("recovery blocked pause: %v", err)
	}
	control.SetMode(model.OperatingModePaused)
	if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
		t.Fatalf("pause changed unrelated policy: %+v, %v", saved, err)
	}
	assertActiveRecoveryCause(t, app, err.Error())
	_, err = app.ControlAction("resume")
	requireRecoveryConflict(t, err)
	_, err = app.ControlAction("bogus")
	if !errors.Is(err, ErrUnknownControl) {
		t.Fatalf("recovery hid the unknown-control error: %v", err)
	}
	assertAdmissions(t, fixture.state, 0, "pause during recovery cannot admit work")
}

func TestTickBlocksWaitingControlBeforeFailureReporting(t *testing.T) {
	// Register this connection-local trigger hook before parallel tests start.
	entered, release := make(chan struct{}), make(chan struct{})
	releaseRecovery := sync.OnceFunc(func() { close(release) })
	defer releaseRecovery()
	hook := "hold_recovery_" + strings.ReplaceAll(model.ID(), "-", "")
	if err := sqlite.RegisterScalarFunction(hook, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		close(entered)
		<-release
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	groundingCycle(t, fixture, model.CycleModeAudit)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER hold_control_recovery BEFORE UPDATE ON records
		WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')='interrupted'
		BEGIN SELECT %s(); SELECT RAISE(ABORT, 'synthetic queued-control recovery refusal'); END`, hook))
	tick := make(chan error, 1)
	go func() { tick <- app.Tick() }()
	waitPlanningStorage(t, entered)
	control := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		_, err := app.ControlAction("cycle")
		control <- err
	}()
	<-started
	releaseRecovery()
	if err := waitPlanningStorage(t, tick); err == nil {
		t.Fatal("recovery write was not refused")
	}
	// Deliberately do not call fail: a waiting control gets the gate before the
	// Run loop reports Tick's error, and must already see the recovery barrier.
	requireRecoveryConflict(t, waitPlanningStorage(t, control))
	if saved, err := app.Control(); err != nil || saved.Mode != model.OperatingModePaused || saved.Batch != nil {
		t.Fatalf("waiting control started a run-once batch: %+v, %v", saved, err)
	}
}

func TestRecoveryBlocksPlanningHeldInRemotePreflight(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode          string
		failPreflight bool
	}{
		{mode: "audit"}, {mode: "run once"}, {mode: "continuous"},
		{mode: "run once", failPreflight: true}, {mode: "continuous", failPreflight: true},
	} {
		t.Run(fmt.Sprintf("%s/preflight_failed=%t", tc.mode, tc.failPreflight), func(t *testing.T) {
			mode := tc.mode
			fixture := newScriptedPlanningFixture(t)
			completePlan(t, fixture).queue(fixture)
			hold := filepath.Join(fixture.root, "reconcile-hold")
			if err := os.WriteFile(hold, []byte("1"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(hold) })
			app := fixture.pausedApp(t)
			audit := make(chan error, 1)
			switch mode {
			case "audit":
				go func() { _, err := app.StartAudit(context.Background()); audit <- err }()
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
			waitForFixtureFile(t, filepath.Join(fixture.root, "reconcile-entered"), "remote preflight did not reach its barrier")
			control, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			orphan := groundingCycle(t, fixture, model.CycleModeAudit)
			refusePlanningRecovery(t, app)
			recoveryErr := app.Tick()
			if recoveryErr == nil {
				t.Fatal("recovery write was not refused during preflight")
			}
			if tc.failPreflight {
				fixture.script.SetCatalog()
			}
			if err := os.Remove(hold); err != nil {
				t.Fatal(err)
			}
			if mode == "audit" {
				requireRecoveryConflict(t, waitPlanningStorage(t, audit))
			}
			app.wg.Wait()
			if cycles, err := store.List[model.Cycle](fixture.state, "cycle"); err != nil || len(cycles) != 1 || !wirejson.Equal(cycles[0], orphan) {
				t.Errorf("preflight committed new work during recovery: %+v, %v", cycles, err)
			}
			saved, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			if tc.failPreflight {
				// A genuine, independent route failure still owns its ordinary
				// pause/backoff semantics, even while recovery blocks admission.
				if saved.Error == nil || !strings.Contains(*saved.Error, "Runner route preflight failed") {
					t.Errorf("recovery suppressed a genuine preflight failure: %+v", saved)
				}
				if mode == "run once" && (saved.Mode != model.OperatingModePaused || saved.Batch != nil) ||
					mode == "continuous" && (saved.Mode != model.OperatingModeContinuous || saved.NextCycleAt <= time.Now().Unix()) {
					t.Errorf("ordinary preflight failure lost its pause/backoff semantics: %+v", saved)
				}
			} else if !wirejson.Equal(saved, control) {
				t.Errorf("recovery conflict changed the saved scheduling policy: %+v; want %+v", saved, control)
			}
			if !app.runtimeIdle() {
				t.Error("refused preflight kept the runtime occupied")
			}
			if hasEvent(t, fixture.state, "system", "planning_error", "") != tc.failPreflight {
				t.Error("planning failure activity did not distinguish recovery conflict from a genuine doctor failure")
			}
			assertAdmissions(t, fixture.state, 0, "held preflight cannot admit after recovery begins")
			assertActiveRecoveryCause(t, app, recoveryErr.Error())
			assertNoOpenClients(t, fixture.script)
			if t.Failed() {
				return
			}
			fixture.script.SetCatalog(runnertest.CatalogFor(fixture.routes.all()...)...)
			schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_control_recovery")
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			if tc.failPreflight {
				app.wg.Wait()
				assertActiveRecoveryCause(t, app, "")
				if healed, err := app.Control(); err != nil || !wirejson.Equal(healed, saved) {
					t.Fatalf("recovery healing cleared a genuine preflight failure: %+v, %v", healed, err)
				}
				assertAdmissions(t, fixture.state, 0, "healing preserves ordinary preflight pause/backoff")
				return
			}
			if mode == "audit" {
				assertAdmissions(t, fixture.state, 0, "healing preserves the paused policy")
				if _, err := app.StartAudit(context.Background()); err != nil {
					t.Fatalf("explicit audit after healing: %v", err)
				}
			}
			app.wg.Wait()
			assertActiveRecoveryCause(t, app, "")
			assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "healed recovery lets the original planning policy continue")
			if saved, err := app.Control(); err != nil || saved.Mode != control.Mode || saved.CycleNumber != control.CycleNumber+1 || (control.Batch != nil && (saved.Batch == nil || saved.Batch.ID != control.Batch.ID)) {
				t.Fatalf("healthy planning did not retain the original mode and batch: %+v, %v", saved, err)
			}
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 2 {
				t.Fatalf("healed recovery did not retain the orphan and complete one fresh plan: %+v, %v", cycles, err)
			}
			for _, cycle := range cycles {
				want := model.CycleCompleted
				if cycle.ID == orphan.ID {
					want = model.CycleInterrupted
				}
				if cycle.Status != want {
					t.Errorf("cycle %s status=%s; want %s", cycle.ID, cycle.Status, want)
				}
			}
			assertNoOpenClients(t, fixture.script)
		})
	}
}
