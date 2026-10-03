package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestRepeatedRecoveryErrorsPreserveEarlierActivity(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	fixture.configure(t, func(cfg *config.Config) { cfg.RetainEvents = 100 })
	app := fixture.pausedApp(t)
	cycle := groundingCycle(t, fixture, model.CycleModeAudit)
	if err := fixture.state.Event(cycle.ID, "planning_error", "Earlier planning evidence must remain available"); err != nil {
		t.Fatal(err)
	}
	schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery BEFORE UPDATE ON records
		WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')='interrupted'
		BEGIN SELECT RAISE(ABORT, 'synthetic repeated recovery refusal'); END`)
	for attempt := 0; attempt < 150; attempt++ {
		err := app.Tick()
		var recovery *recoveryError
		if !errors.As(err, &recovery) {
			t.Fatalf("attempt %d stopped propagating the recovery error: %v", attempt, err)
		}
		app.fail(err)
	}
	if !hasEvent(t, fixture.state, cycle.ID, "planning_error", "Earlier planning evidence") {
		t.Error("identical recovery retries evicted earlier activity at retain_events=100")
	}
	system := "system"
	events, err := fixture.state.Events(&system)
	if err != nil || len(events) != 1 || events[0].Kind != "recovery_error" {
		t.Fatalf("150 identical retries recorded %d events; want one recovery error: %v", len(events), err)
	}
}

func TestRecoveryActivityRecordsChangedCausesAndResetsAfterSuccess(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	fail := func(message string) { app.fail(&recoveryError{err: errors.New(message)}) }
	fail("first recovery cause")
	fail("first recovery cause")
	fail("changed recovery cause")
	fail("changed recovery cause")
	fail("first recovery cause")
	system := "system"
	events, err := fixture.state.Events(&system)
	if err != nil || len(events) != 2 || events[0].Message != "changed recovery cause" || events[1].Message != "first recovery cause" {
		t.Fatalf("first and changed causes: %+v, %v", events, err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	fail("changed recovery cause")
	fail("first recovery cause")
	fail("changed recovery cause")
	events, err = fixture.state.Events(&system)
	if err != nil || len(events) != 4 || events[0].Message != "first recovery cause" {
		t.Fatalf("a later recovery episode was not reported once: %+v, %v", events, err)
	}
}

func TestRecoveryActivityRetriesRefusedEventInserts(t *testing.T) {
	t.Parallel()
	for _, priorCause := range []string{"", "previously recorded cause"} {
		t.Run("previous="+priorCause, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			app := fixture.pausedApp(t)
			before, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			fail := func(message string) { app.fail(&recoveryError{err: errors.New(message)}) }
			wantEvents := 0
			if priorCause != "" {
				fail(priorCause)
				wantEvents++
			}
			schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery_activity BEFORE INSERT ON events
				WHEN NEW.kind='recovery_error'
				BEGIN SELECT RAISE(ABORT, 'synthetic activity refusal'); END`)
			fail("still unrecorded cause")
			fail("still unrecorded cause")
			view, err := app.StateView()
			if err != nil {
				t.Fatal(err)
			}
			active, ok := view["recovery_error"].(*string)
			if !ok || active == nil || *active != "still unrecorded cause" {
				t.Fatalf("failed activity insert hid the latest recovery cause: %+v", view)
			}
			system := "system"
			if events, err := fixture.state.Events(&system); err != nil || len(events) != wantEvents {
				t.Fatalf("refused event insertion: %+v, %v", events, err)
			}
			schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_recovery_activity")
			fail("still unrecorded cause")
			fail("still unrecorded cause")
			if events, err := fixture.state.Events(&system); err != nil || len(events) != wantEvents+1 || events[0].Message != "still unrecorded cause" {
				t.Fatalf("healed activity write was not retried and then coalesced: %+v, %v", events, err)
			}
			if after, err := app.Control(); err != nil || !wirejson.Equal(after, before) {
				t.Fatalf("activity retry changed control: %+v, %v", after, err)
			}
		})
	}
}

func TestRecoveryActivityLeavesOrdinaryErrorsUnchanged(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	if err := fixture.state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		app.fail(errors.New("ordinary scheduling failure"))
	}
	system := "system"
	events, err := fixture.state.Events(&system)
	if err != nil || len(events) != 2 || events[0].Kind != "error" || events[1].Kind != "error" {
		t.Fatalf("ordinary errors were coalesced or reclassified: %+v, %v", events, err)
	}
	if saved, err := app.Control(); err != nil || saved.Mode != model.OperatingModePaused {
		t.Fatalf("ordinary failure no longer pauses: %+v, %v", saved, err)
	}
}

func TestStateViewShowsActiveRecoveryWithoutChangingControl(t *testing.T) {
	t.Parallel()
	for _, mode := range []model.CycleMode{model.CycleModeAudit, model.CycleModeExecution} {
		for _, refuseActivity := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/activity_refused=%t", mode, refuseActivity), func(t *testing.T) {
				fixture := newScriptedPlanningFixture(t)
				app := fixture.pausedApp(t)
				control := model.DefaultControl()
				if mode == model.CycleModeExecution {
					control.SetMode(model.OperatingModeContinuous)
					control.NextCycleAt = time.Now().Add(time.Hour).Unix()
				}
				if err := fixture.state.SaveControl(control); err != nil {
					t.Fatal(err)
				}
				cycle := groundingCycle(t, fixture, mode)
				if refuseActivity {
					schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery_activity BEFORE INSERT ON events
						WHEN NEW.kind='recovery_error'
						BEGIN SELECT RAISE(ABORT, 'synthetic activity refusal'); END`)
				}
				app.fail(&recoveryError{err: errors.New("Recovery write refused: bearer fixturevisibilitysecret123")})
				view, err := app.StateView()
				if err != nil {
					t.Fatal(err)
				}
				message, ok := view["recovery_error"].(*string)
				if !ok || message == nil || !strings.Contains(*message, "Recovery write refused") || strings.Contains(*message, "fixturevisibilitysecret123") || view["status"] != "unhealthy" {
					t.Fatalf("active recovery is hidden, unredacted or loses priority to audit state: %+v", view)
				}
				if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) || saved.Error != nil {
					t.Fatalf("recovery visibility changed saved control: %+v, %v", saved, err)
				}
				if refuseActivity && hasEvent(t, fixture.state, "system", "recovery_error", "") {
					t.Fatal("activity refusal did not exercise transient-only visibility")
				}
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
				view, err = app.StateView()
				if err != nil {
					t.Fatal(err)
				}
				message, ok = view["recovery_error"].(*string)
				if !ok || message != nil || view["status"] == "unhealthy" || view["cycle_active"] != false {
					t.Fatalf("successful recovery retained a stale dashboard error: %+v", view)
				}
				if saved, err := store.Get[model.Cycle](fixture.state, "cycle", cycle.ID); err != nil || saved == nil || saved.Status != model.CycleInterrupted {
					t.Fatalf("recovery did not settle the visible orphan: %+v, %v", saved, err)
				}
				if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
					t.Fatalf("visibility reset changed saved control: %+v, %v", saved, err)
				}
			})
		}
	}
}

func TestAlternatingRecoveryErrorsPreserveEarlierActivity(t *testing.T) {
	t.Parallel()
	for _, flavor := range []string{"trigger oscillation", "intermittent writer contention"} {
		t.Run(flavor, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			fixture.configure(t, func(cfg *config.Config) { cfg.RetainEvents = 100 })
			app := fixture.pausedApp(t)
			cycle := groundingCycle(t, fixture, model.CycleModeAudit)
			before, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			if err := fixture.state.Event(cycle.ID, "planning_error", "Earlier evidence"); err != nil {
				t.Fatal(err)
			}
			schedulerSQL(t, fixture.state, "CREATE TEMP TABLE recovery_probe (n INTEGER)")
			schedulerSQL(t, fixture.state, "INSERT INTO recovery_probe VALUES (0)")
			schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery BEFORE UPDATE ON records
				WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')='interrupted'
				BEGIN UPDATE recovery_probe SET n=n+1;
				SELECT CASE WHEN (SELECT n FROM recovery_probe)%2=1 THEN RAISE(FAIL,'synthetic storage refusal A') ELSE RAISE(FAIL,'synthetic storage refusal B') END; END`)
			var writer *sql.Conn
			if flavor == "intermittent writer contention" {
				schedulerSQL(t, fixture.state, "PRAGMA busy_timeout=1")
				schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_recovery")
				schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery BEFORE UPDATE ON records
					WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')='interrupted'
					BEGIN SELECT RAISE(FAIL,'synthetic persistent terminal write refusal'); END`)
				db, err := sql.Open("sqlite", "file:"+fixture.state.Path())
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				writer, err = db.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
			}
			causes := map[string]int{}
			for attempt := 0; attempt < 150; attempt++ {
				held := writer != nil && attempt%2 == 0
				if held {
					if _, err := writer.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
						t.Fatal(err)
					}
				}
				err := app.Tick()
				if held {
					if _, err := writer.ExecContext(context.Background(), "ROLLBACK"); err != nil {
						t.Fatal(err)
					}
				}
				var recovery *recoveryError
				if !errors.As(err, &recovery) {
					t.Fatalf("attempt %d: %v", attempt, err)
				}
				if held && !strings.Contains(err.Error(), "locked") {
					t.Fatalf("expected actual writer contention, got %v", err)
				}
				causes[err.Error()]++
				app.fail(err)
				assertActiveRecoveryCause(t, app, err.Error())
			}
			system := "system"
			events, err := fixture.state.Events(&system)
			if err != nil || len(causes) != 2 || len(events) != 2 {
				t.Fatalf("150 failed recovery passes: causes=%v events=%d error=%v; want two causes and events", causes, len(events), err)
			}
			if !hasEvent(t, fixture.state, cycle.ID, "planning_error", "Earlier evidence") {
				t.Fatal("alternating recovery retries evicted earlier activity at retain_events=100")
			}
			saved, err := store.Get[model.Cycle](fixture.state, "cycle", cycle.ID)
			if err != nil || saved == nil || !wirejson.Equal(saved, &cycle) {
				t.Fatalf("retained cycle evidence changed: %v", err)
			}
			if after, err := app.Control(); err != nil || !wirejson.Equal(before, after) {
				t.Fatalf("control changed: %v", err)
			}
			assertAdmissions(t, fixture.state, 0, "all recovery passes failed")
		})
	}
}

func TestRecoveryActivityBoundsDistinctCausesAndRetriesOverflow(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	fixture.configure(t, func(cfg *config.Config) { cfg.RetainEvents = 100 })
	app := fixture.pausedApp(t)
	if err := fixture.state.Event("system", "prior_activity", "Earlier evidence"); err != nil {
		t.Fatal(err)
	}
	fail := func(cause string) {
		app.fail(&recoveryError{err: errors.New(cause)})
		assertActiveRecoveryCause(t, app, cause)
	}
	for cause := 0; cause < 8; cause++ {
		fail(fmt.Sprintf("recovery cause %d", cause))
	}
	schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery_activity BEFORE INSERT ON events
		WHEN NEW.kind='recovery_error'
		BEGIN SELECT RAISE(ABORT, 'synthetic activity refusal'); END`)
	for cause := 8; cause < 12; cause++ {
		fail(fmt.Sprintf("recovery cause %d", cause))
	}
	system := "system"
	if events, err := fixture.state.Events(&system); err != nil || len(events) != 9 {
		t.Fatalf("refused overflow insert should leave eight causes and earlier activity: %+v, %v", events, err)
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_recovery_activity")
	for cause := 8; cause < 300; cause++ {
		fail(fmt.Sprintf("recovery cause %d", cause))
		// Saturation must not evict prior causes or resume logging when they recur.
		fail(fmt.Sprintf("recovery cause %d", cause%8))
	}
	events, err := fixture.state.Events(&system)
	if err != nil || len(events) != 10 || events[0].Kind != "recovery_error" || !strings.Contains(events[0].Message, "Additional recovery causes") {
		t.Fatalf("distinct causes should produce eight entries and one overflow notice: %+v, %v", events, err)
	}
	if !hasEvent(t, fixture.state, system, "prior_activity", "Earlier evidence") {
		t.Fatal("high-cardinality recovery errors evicted earlier activity")
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	assertActiveRecoveryCause(t, app, "")
	for cause := 0; cause < 10; cause++ {
		fail(fmt.Sprintf("recovery cause %d", cause))
	}
	if events, err := fixture.state.Events(&system); err != nil || len(events) != 19 {
		t.Fatalf("successful recovery should reset causes and overflow notice for the next episode: %+v, %v", events, err)
	}
}

func TestRecoveryActivityDeduplicatesRedactedCauses(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	for _, secret := range []string{"fixturefirstsecret123", "fixturesecondsecret123", "fixturefirstsecret123"} {
		app.fail(&recoveryError{err: fmt.Errorf("Recovery write refused: bearer %s", secret)})
	}
	system := "system"
	events, err := fixture.state.Events(&system)
	if err != nil || len(events) != 1 || strings.Contains(events[0].Message, "secret123") {
		t.Fatalf("equivalent redacted causes should share one safe activity entry: %+v, %v", events, err)
	}
	assertActiveRecoveryCause(t, app, events[0].Message)
}

func assertActiveRecoveryCause(t *testing.T, app *App, want string) {
	t.Helper()
	view, err := app.StateView()
	if err != nil {
		t.Fatal(err)
	}
	active, ok := view["recovery_error"].(*string)
	if !ok || want == "" && active != nil || want != "" && (active == nil || *active != want || view["status"] != "unhealthy") {
		t.Fatalf("active recovery cause: got %v, want %q, state=%+v", active, want, view)
	}
}
