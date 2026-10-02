package engine

import (
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
	system := "system"
	events, err := fixture.state.Events(&system)
	if err != nil || len(events) != 2 || events[0].Message != "changed recovery cause" || events[1].Message != "first recovery cause" {
		t.Fatalf("first and changed causes: %+v, %v", events, err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	fail("changed recovery cause")
	fail("changed recovery cause")
	events, err = fixture.state.Events(&system)
	if err != nil || len(events) != 3 || events[0].Message != "changed recovery cause" {
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
