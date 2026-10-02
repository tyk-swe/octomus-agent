package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
)

func TestFailedRetriedExecutorRecordsCurrentTurnFailure(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor,
		runnertest.Reply{Err: errors.New("Original executor failure")},
		runnertest.Reply{Err: errors.New("Current executor failure")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)

	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if !blockedAs(saved, model.BlockedReasonRunnerUnavailable) {
		t.Fatalf("original executor outcome = %+v", saved)
	}
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	saved = driveTask(t, fixture.planningFixture, app, task.ID)
	if !blockedAs(saved, model.BlockedReasonRunnerUnavailable) {
		t.Fatalf("retried executor outcome = %+v", saved)
	}
	executors := sessionByRole(saved, "executor")
	if len(executors) != 1 || executors[0].Status != model.SessionFailed || !strings.Contains(executors[0].Summary, "Current executor failure") {
		t.Fatalf("retried executor retained an earlier turn's failure: %+v", executors)
	}
	starts := script.Starts(routes.Executor)
	if len(starts) != 2 || starts[1].Resume == nil || *starts[1].Resume != starts[0].Session {
		t.Fatalf("fixture did not resume the executor thread: %+v", starts)
	}
	assertUnpublished(t, fixture, saved)
	assertNoOpenClients(t, script)
}

func TestFailedResumedRepairRecordsCurrentTurnFailure(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("First draft reviewed"), cleanReview("Second draft reviewed"))
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "Completed the first repair", Effect: writeFile("feature.txt", "second draft\n")},
		runnertest.Reply{Err: errors.New("Current repair turn failed")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedReasonRunnerUnavailable) {
		t.Fatalf("failed repair outcome = %+v", saved)
	}
	repairs := sessionByRole(saved, "repair")
	if len(repairs) != 1 || repairs[0].Status != model.SessionFailed || !strings.Contains(repairs[0].Summary, "Current repair turn failed") {
		t.Fatalf("failed resumed repair retained an earlier turn's summary: %+v", repairs)
	}
	starts := script.Starts(routes.Repair)
	if len(starts) != 2 || starts[1].Resume == nil || *starts[1].Resume != starts[0].Session {
		t.Fatalf("fixture did not resume the repair thread: %+v", starts)
	}
	assertUnpublished(t, fixture, saved)
	assertNoOpenClients(t, script)
}
