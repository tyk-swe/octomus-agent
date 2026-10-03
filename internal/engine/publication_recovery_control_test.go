package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestPublicationRecoveryBlocksControlsBeforeFailureReporting(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	app := fixture.pausedApp(t)
	task := queuedTask(fixture.cfg, model.ID(), fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+model.ID())
	task.Status = model.StatusPublishing
	task.OutputCommit = stringPointer("retained-publication-output")
	saveExecutionTask(t, fixture.planningFixture, task)
	before, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_publication_control_recovery BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status')='blocked'
		BEGIN SELECT RAISE(ABORT, 'synthetic publication recovery refusal'); END`)
	recoveryErr := app.Tick()
	if recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "synthetic publication recovery refusal") {
		t.Fatalf("publication recovery refusal = %v", recoveryErr)
	}
	// No fail() call: controls must see the barrier as soon as Tick returns.
	if _, err := app.ControlAction("cycle"); !IsActionConflict(err) || !strings.Contains(err.Error(), "recovery") {
		t.Fatalf("publication recovery allowed a run-once batch before reporting: %v", err)
	}
	if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, before) {
		t.Fatalf("blocked publication recovery changed control: %+v, %v", saved, err)
	}
	assertActiveRecoveryCause(t, app, recoveryErr.Error())
	schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_publication_control_recovery")
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	assertActiveRecoveryCause(t, app, "")
	if _, err := app.ControlAction("resume"); err != nil {
		t.Fatalf("healed publication recovery still blocked controls: %v", err)
	}
	assertAdmissions(t, fixture.state, 0, "recovery and control changes cannot replay model work")
	if entries := publications(t, fixture.planningFixture); len(entries) != 0 {
		t.Fatalf("recovery replayed delivery: %+v", entries)
	}
}

func TestPublicationWorkerExitReadFailureBlocksAdmission(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	task := queuedTask(fixture.cfg, model.ID(), fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+model.ID())
	task.Status = model.StatusPublishing
	task.OutputCommit = stringPointer("retained-publication-output")
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.pausedApp(t)
	app.taskRunner = TaskRunnerFunc(func(context.Context, model.Task) error {
		raw, err := wirejson.GenericMap(task)
		if err != nil {
			return err
		}
		raw["sessions"] = "synthetic final-read refusal"
		return fixture.state.Put("task", task.ID, raw)
	})
	app.runTask(task)
	app.wg.Wait()
	if !app.Drained() {
		t.Fatal("worker retained its runtime claim")
	}
	if _, err := app.ControlAction("cycle"); !IsActionConflict(err) || !strings.Contains(err.Error(), "recovery") {
		t.Fatalf("unreadable worker outcome allowed new work before Tick: %v", err)
	}
	if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "sessions") {
		t.Fatalf("unreadable publication recovery = %v", err)
	}
	saveExecutionTask(t, fixture.planningFixture, task)
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	assertActiveRecoveryCause(t, app, "")
	if _, err := app.ControlAction("cycle"); err != nil {
		t.Fatalf("healed publication still blocked controls: %v", err)
	}
	assertAdmissions(t, fixture.state, 0, "worker settlement must not invoke model work")
}
