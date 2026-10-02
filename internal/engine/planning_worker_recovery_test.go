package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestTickRecoversPlanningWorkerTerminalWriteRefusal(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"audit", "run once", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			completePlan(t, fixture).queue(fixture)
			app := fixture.pausedApp(t)
			schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_cycle_terminal BEFORE UPDATE ON records
				WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')!='running'
				BEGIN SELECT RAISE(ABORT, 'synthetic cycle terminal refusal'); END`)
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
			app.wg.Wait()
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 1 {
				t.Fatalf("cycles: %+v, %v", cycles, err)
			}
			before := cycles[0]
			if !app.runtimeIdle() || before.Status != model.CycleRunning {
				t.Fatalf("worker exit: idle=%v, cycle=%+v", app.runtimeIdle(), before)
			}
			if events := planningErrors(t, fixture.state, before.ID); len(events) != 1 || !strings.Contains(events[0].Message, "synthetic cycle terminal refusal") {
				t.Fatalf("terminal refusal evidence: %+v", events)
			}
			if len(before.Sessions) != int(fixture.cfg.PlanningAdmissionsRequired()) || len(before.Assessments) != 2 || before.Grounding == nil {
				t.Fatalf("worker did not retain its completed planning evidence: %+v", before)
			}
			for _, session := range before.Sessions {
				if session.Status != model.SessionCompleted {
					t.Fatalf("planning session did not complete: %+v", session)
				}
			}
			control, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			// The scheduler must retain the record and return the write error until storage heals.
			if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "synthetic cycle terminal refusal") {
				t.Errorf("recovery under the remaining storage refusal = %v", err)
			}
			if current, err := store.Get[model.Cycle](fixture.state, "cycle", before.ID); err != nil || current == nil || !wirejson.Equal(current, before) {
				t.Fatalf("refused recovery changed saved evidence: %+v, %v", current, err)
			}
			schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_cycle_terminal")
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			saved, err := store.Get[model.Cycle](fixture.state, "cycle", before.ID)
			if err != nil || saved == nil || saved.Status != model.CycleInterrupted || saved.CompletedAt == nil || saved.Error == nil || *saved.Error != interruptedPlanningMessage {
				t.Fatalf("healed scheduler left workerless planning unresolved: %+v, %v", saved, err)
			}
			expected := before.Clone()
			expected.Status, expected.CompletedAt, expected.Error = saved.Status, saved.CompletedAt, saved.Error
			if !wirejson.Equal(saved, expected) {
				t.Fatal("recovery changed retained proposals, assessments, sessions, grounding or lifecycle")
			}
			if current, err := app.Control(); err != nil || !wirejson.Equal(current, control) {
				t.Fatalf("cycle recovery changed operating mode or backoff: %+v, %v", current, err)
			}
			if tasks, err := store.List[model.Task](fixture.state, "task"); err != nil || len(tasks) != 0 {
				t.Fatalf("uncommitted plan queued tasks: %+v, %v", tasks, err)
			}
			if decisions, err := fixture.state.DecisionMemory(fixture.cfg.GitHubRepo); err != nil || len(decisions) != 0 {
				t.Fatalf("uncommitted plan saved decisions: %+v, %v", decisions, err)
			}
			assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "recovery must not replay planning")
			view, err := app.StateView()
			if err != nil || view["cycle_active"] != false || view["active_cycle_mode"].(*model.CycleMode) != nil || view["status"] == "auditing" {
				t.Fatalf("recovered dashboard still prevents the next operator action: %+v, %v", view, err)
			}
			if err := app.CycleAction(before.ID, "archive"); err != nil {
				t.Fatalf("recovered cycle could not be archived: %v", err)
			}
			// Use the dashboard's ordinary controls to start a fresh audit after recovery.
			if mode == "continuous" {
				if _, err := app.ControlAction("pause"); err != nil {
					t.Fatal(err)
				}
			}
			completePlan(t, fixture).queue(fixture)
			if _, err := app.ControlAction("audit"); err != nil {
				t.Fatalf("operator could not start the next audit: %v", err)
			}
			app.wg.Wait()
			cycles, err = store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 2 {
				t.Fatalf("fresh audit cycles: %+v, %v", cycles, err)
			}
			for _, cycle := range cycles {
				if cycle.ID == before.ID {
					if cycle.Status != model.CycleInterrupted || cycle.Lifecycle.ArchivedAt == nil {
						t.Fatalf("fresh audit rewrote prior evidence: %+v", cycle)
					}
				} else if cycle.Status != model.CycleCompleted || cycle.Mode != model.CycleModeAudit {
					t.Fatalf("fresh audit did not finish: %+v", cycle)
				}
			}
			assertAdmissions(t, fixture.state, 2*fixture.cfg.PlanningAdmissionsRequired(), "only the explicit next audit may use more turns")
			assertNoOpenClients(t, fixture.script)
		})
	}
}

func TestTickRecoversOrphanCycleWithoutInterruptingCurrentWorker(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	plan := completePlan(t, fixture)
	grounding := runnertest.NewGate()
	plan.grounding.Gate = grounding
	plan.queue(fixture)
	app := fixture.pausedApp(t)
	t.Cleanup(grounding.Release)
	id, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitPlanningStorage(t, grounding.Entered())
	before, err := store.Get[model.Cycle](fixture.state, "cycle", id)
	if err != nil || before == nil {
		t.Fatalf("active cycle: %+v, %v", before, err)
	}
	orphan := before.Clone()
	orphan.ID = model.ID()
	orphan.Number++
	orphan.Sessions = []model.Session{model.NewSession("retained-orphan-session", "grounding", fixture.routes.Orchestrator)}
	if err := fixture.state.Put("cycle", orphan.ID, orphan); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get[model.Cycle](fixture.state, "cycle", id)
	if err != nil || current == nil || !wirejson.Equal(current, before) {
		t.Fatalf("recovery changed the active worker's evidence: %+v, %v", current, err)
	}
	saved, err := store.Get[model.Cycle](fixture.state, "cycle", orphan.ID)
	if err != nil || saved == nil || saved.Status != model.CycleInterrupted || saved.Sessions[0].Status != model.SessionInterrupted {
		t.Fatalf("workerless cycle was not interrupted: %+v, %v", saved, err)
	}
	expected := orphan.Clone()
	model.InterruptRunning(expected.Sessions)
	expected.Status, expected.CompletedAt, expected.Error = saved.Status, saved.CompletedAt, saved.Error
	if !wirejson.Equal(saved, expected) {
		t.Fatal("recovery changed retained evidence beyond interruption metadata")
	}
	if err := app.CycleAction(id, "archive"); !IsActionConflict(err) {
		t.Fatalf("active cycle accepted archive: %v", err)
	}
	if _, err := app.ControlAction("resume"); !IsActionConflict(err) {
		t.Fatalf("active audit lost its operating-mode protection: %v", err)
	}
	grounding.Release()
	app.wg.Wait()
	current, err = store.Get[model.Cycle](fixture.state, "cycle", id)
	if err != nil || current == nil {
		t.Fatalf("completed live cycle: %+v, %v", current, err)
	}
	assertScriptedPlanningPass(t, fixture, *current)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if unchanged, err := store.Get[model.Cycle](fixture.state, "cycle", id); err != nil || unchanged == nil || !wirejson.Equal(unchanged, current) {
		t.Fatalf("later recovery rewrote a committed plan: %+v, %v", unchanged, err)
	}
}

func TestTickExcludesActiveCycleBeforeDecodingEvidence(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	active := groundingCycle(t, fixture, model.CycleModeAudit)
	orphan := groundingCycle(t, fixture, model.CycleModeExecution)
	ctx, cancel := context.WithCancel(app.Context())
	defer cancel()
	app.runtimeMu.Lock()
	app.runtime.cycle = &cycleJob{id: active.ID, mode: active.Mode, cancel: cancel}
	app.runtimeMu.Unlock()

	// A typed-decoding tripwire demonstrates that the scheduler never hydrates
	// the live worker's potentially large evidence just to exclude its identity.
	raw, err := wirejson.GenericMap(active)
	if err != nil {
		t.Fatal(err)
	}
	raw["sessions"] = "synthetic evidence decoding tripwire"
	if err := fixture.state.Put("cycle", active.ID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.RunningCycles(); err == nil || !strings.Contains(err.Error(), active.ID) {
		t.Fatalf("unfiltered running-cycle query did not hit the active evidence tripwire: %v", err)
	}
	if err := app.Tick(); err != nil {
		t.Fatalf("scheduler decoded active evidence during orphan recovery: %v", err)
	}
	if current, found, err := fixture.state.GetValue("cycle", active.ID); err != nil || !found || !wirejson.Equal(current, raw) {
		t.Fatalf("scheduler rewrote active evidence: %+v, %v", current, err)
	}
	if ctx.Err() != nil {
		t.Fatal("scheduler cancelled the current worker")
	}
	if saved, err := store.Get[model.Cycle](fixture.state, "cycle", orphan.ID); err != nil || saved == nil || saved.Status != model.CycleInterrupted {
		t.Fatalf("excluding active evidence prevented orphan recovery: %+v, %v", saved, err)
	}
}

func TestTickRetriesOrphanedRunOnceControlRecovery(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	completePlan(t, fixture).queue(fixture)
	app := fixture.pausedApp(t)
	schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_cycle_terminal BEFORE UPDATE ON records
		WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')!='running'
		BEGIN SELECT RAISE(ABORT, 'synthetic cycle terminal refusal'); END`)
	schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_planning_pause BEFORE UPDATE ON records
		WHEN NEW.kind='settings' AND NEW.id='control' AND json_extract(NEW.data,'$.mode')='paused'
		BEGIN SELECT RAISE(ABORT, 'synthetic planning pause refusal'); END`)
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModeRunOnce || control.Batch == nil || control.Batch.Phase != model.BatchPhasePlanning || control.Batch.CycleID == nil {
		t.Fatalf("refused worker finalization: %+v, %v", control, err)
	}
	id := *control.Batch.CycleID
	before, err := store.Get[model.Cycle](fixture.state, "cycle", id)
	if err != nil || before == nil || before.Status != model.CycleRunning || !app.runtimeIdle() {
		t.Fatalf("worker did not leave an orphaned planning cycle: %+v, %v", before, err)
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_cycle_terminal")
	for attempt := 0; attempt < 2; attempt++ {
		if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "synthetic planning pause refusal") {
			t.Errorf("control recovery attempt %d = %v; want retryable pause refusal", attempt, err)
		}
		current, err := app.Control()
		if err != nil || !wirejson.Equal(current, control) {
			t.Fatalf("refused pause changed the batch: %+v, %v", current, err)
		}
	}
	interrupted, err := store.Get[model.Cycle](fixture.state, "cycle", id)
	if err != nil || interrupted == nil || interrupted.Status != model.CycleInterrupted {
		t.Fatalf("cycle did not settle before retrying control: %+v, %v", interrupted, err)
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_planning_pause")
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	saved, err := app.Control()
	if err != nil || saved.Mode != model.OperatingModePaused || saved.Batch != nil || saved.Error == nil || *saved.Error != "Run once was interrupted before its planning transaction committed" {
		t.Fatalf("healed storage left the planning batch stuck: %+v, %v", saved, err)
	}
	if cycle, err := store.Get[model.Cycle](fixture.state, "cycle", id); err != nil || !wirejson.Equal(cycle, interrupted) {
		t.Fatalf("control retry rewrote the retained cycle: %+v, %v", cycle, err)
	}
	if tasks, err := store.List[model.Task](fixture.state, "task"); err != nil || len(tasks) != 0 {
		t.Fatalf("interrupted plan dispatched work: %+v, %v", tasks, err)
	}
	assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "control recovery cannot replay planning")
	completePlan(t, fixture).queue(fixture)
	if _, err := app.ControlAction("cycle"); err != nil {
		t.Fatalf("operator could not start a fresh run once: %v", err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	next, err := app.Control()
	if err != nil || next.Mode != model.OperatingModeRunOnce || next.Batch == nil || next.Batch.Phase != model.BatchPhaseExecuting || next.Batch.CycleID == nil || *next.Batch.CycleID == id {
		t.Fatalf("fresh run once failed to commit its new plan: %+v, %v", next, err)
	}
	assertAdmissions(t, fixture.state, 2*fixture.cfg.PlanningAdmissionsRequired(), "only the operator's fresh run may plan again")
}

func TestTickKeepsLiveRunOncePlanningControl(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	plan := completePlan(t, fixture)
	gate := runnertest.NewGate()
	plan.grounding.Gate = gate
	plan.queue(fixture)
	app := fixture.pausedApp(t)
	t.Cleanup(gate.Release)
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	waitPlanningStorage(t, gate.Entered())
	before, err := app.Control()
	if err != nil || before.Batch == nil || before.Batch.Phase != model.BatchPhasePlanning {
		t.Fatalf("live planning control: %+v, %v", before, err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	if current, err := app.Control(); err != nil || !wirejson.Equal(current, before) {
		t.Fatalf("live planning was paused: %+v, %v", current, err)
	}
	gate.Release()
	app.wg.Wait()
	current, err := app.Control()
	if err != nil || current.Batch == nil || current.Batch.Phase != model.BatchPhaseExecuting {
		t.Fatalf("live planning did not commit: %+v, %v", current, err)
	}
}
