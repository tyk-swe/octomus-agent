package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func loadTask(t *testing.T, state *store.Store, id string) model.Task {
	t.Helper()
	task, err := store.Get[model.Task](state, "task", id)
	if err != nil || task == nil {
		t.Fatalf("load task %s: %+v, %v", id, task, err)
	}
	return *task
}

func blockedAs(task model.Task, reason model.BlockedReason) bool {
	return task.Status == model.StatusBlocked && task.BlockedReason != nil && *task.BlockedReason == reason
}

func assertUnpublished(t *testing.T, fixture *scriptedFixture, task model.Task) {
	t.Helper()
	if task.OutputCommit != nil || task.PRNumber != nil || len(publications(t, fixture.planningFixture)) != 0 {
		t.Fatalf("task authorized publication: %+v", task)
	}
}

func TestExecutionMalformedAndIncompleteReviewsNeverPublish(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		answer  string
		reasons []model.BlockedReason
	}{
		{"malformed", "The change looks fine to me.", []model.BlockedReason{model.BlockedReasonInvalidReview, model.BlockedReasonRunnerUnavailable}},
		{"schema-invalid", `{"completed": "yes", "summary": "Looks fine", "findings": []}`, []model.BlockedReason{model.BlockedReasonInvalidReview, model.BlockedReasonRunnerUnavailable}},
		{"incomplete", `{"completed": false, "summary": "Ran out of time", "findings": []}`, []model.BlockedReason{model.BlockedReasonInvalidReview}},
		{"blank-summary", `{"completed": true, "summary": "  ", "findings": []}`, []model.BlockedReason{model.BlockedReasonInvalidReview}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScriptedFixture(t)
			routes, script := fixture.routes, fixture.script
			script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
			script.Answer(routes.Reviewer, test.answer)
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture.planningFixture, task)

			saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
			if saved.Status != model.StatusBlocked || saved.BlockedReason == nil {
				t.Fatalf("invalid review did not block: %+v", saved)
			}
			accepted := false
			for _, reason := range test.reasons {
				accepted = accepted || *saved.BlockedReason == reason
			}
			if !accepted {
				t.Fatalf("%s review blocked as %v; want one of %v", test.name, *saved.BlockedReason, test.reasons)
			}
			if len(saved.Reviews) != 0 || len(saved.Verification) != 0 {
				t.Fatalf("an invalid review was recorded or verified: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
			}
			reviewers := sessionByRole(saved, "reviewer")
			if len(reviewers) != 1 || reviewers[0].Status == model.SessionCompleted {
				t.Fatalf("invalid review session = %+v", reviewers)
			}
			if executors := sessionByRole(saved, "executor"); len(executors) != 1 || executors[0].Status != model.SessionCompleted {
				t.Fatalf("executor session = %+v", executors)
			}
			assertUnpublished(t, fixture, saved)
			assertAdmissions(t, fixture.state, 2, "executor + reviewer")
			assertNoOpenClients(t, script)
		})
	}
}

func TestExecutionReviewerWorkspaceEditBlocks(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"test -f feature.txt"}
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Queue(routes.Reviewer, runnertest.Reply{Answer: cleanReview("Looks fine"), Effect: writeFile("stray.txt", "reviewer edit\n")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedReasonWorkspaceInvalid) {
		t.Fatalf("reviewer workspace edit outcome = %+v", saved)
	}
	if len(saved.Reviews) != 0 || len(saved.Verification) != 0 {
		t.Fatalf("a review of an edited workspace was recorded or verified: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
	}
	reviewers := sessionByRole(saved, "reviewer")
	if len(reviewers) != 1 || reviewers[0].Status != model.SessionFailed || !strings.Contains(reviewers[0].Summary, "Looks fine") {
		t.Fatalf("reviewer session = %+v; want it failed with the rejected answer", reviewers)
	}
	if len(script.Turns(routes.Repair)) != 0 {
		t.Fatal("a rejected review reached repair")
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 2, "executor + reviewer")
	assertNoOpenClients(t, script)
}

func TestExecutionFailedVerificationExhaustsRepairBudget(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxRepairRounds = 2
		cfg.MaxNoProgressRounds = 5
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("Round one"), cleanReview("Round two"), cleanReview("Round three"))
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "Repair one", Effect: writeFile("feature.txt", "repair one\n")},
		runnertest.Reply{Answer: "Repair two", Effect: writeFile("feature.txt", "repair two\n")},
		runnertest.Reply{Answer: "Repair three", Effect: writeFile("feature.txt", "repair three\n")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedReasonVerificationFailed) {
		t.Fatalf("failed verification outcome = %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, "Repair budget exhausted (max_repair_rounds 2)") {
		t.Fatalf("the block must name the exhausted repair budget: %q", optionalText(saved.Error))
	}
	if len(saved.Reviews) != 3 || len(saved.Verification) != 3 {
		t.Fatalf("evidence: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
	}
	revisions := map[string]bool{}
	for i, round := range saved.Reviews {
		if !round.Result.Clean() || saved.Verification[i].Success || saved.Verification[i].Revision != round.Revision {
			t.Fatalf("round %d: review=%+v verification=%+v", i, round, saved.Verification[i])
		}
		revisions[round.Revision] = true
	}
	if len(revisions) != 3 {
		t.Fatalf("every repair must progress to a new revision: %+v", saved.Reviews)
	}
	reviewTurns := script.Turns(routes.Reviewer)
	for i, round := range saved.Reviews {
		if !strings.Contains(reviewTurns[i].Prompt, "git diff "+saved.ComparisonBase+" HEAD. Recorded HEAD: "+round.Revision+".") {
			t.Fatalf("review %d prompt does not name its full diff and revision: %q", i, reviewTurns[i].Prompt)
		}
	}
	for i, turn := range script.Turns(routes.Repair) {
		if !strings.Contains(turn.Prompt, `Verification failures: ["false: `) {
			t.Fatalf("repair %d prompt lacks the verification failure: %q", i, turn.Prompt)
		}
	}
	repairs := sessionByRole(saved, "repair")
	if len(repairs) != 1 || saved.RepairSession == nil || repairs[0].ID != *saved.RepairSession || repairs[0].Status != model.SessionCompleted {
		t.Fatalf("repair session not persistent: %+v", saved.Sessions)
	}
	starts := script.Starts(routes.Repair)
	if len(starts) != 2 || starts[0].Resume != nil || starts[1].Resume == nil || *starts[1].Resume != *saved.RepairSession {
		t.Fatalf("repair must resume its thread on the second round: %+v", starts)
	}
	if pending := script.Pending(routes.Repair); pending != 1 {
		t.Fatalf("repair replies left = %d; the budget must stop after two repairs", pending)
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 6, "executor + 3 reviewers + 2 repairs")
	assertNoOpenClients(t, script)
}

func TestExecutionWithoutChangesBlocksBeforeReview(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		effect func(cwd string) error
		want   string
	}{
		{"no commit", nil, "No changes were committed on top of the source revision"},
		{"net-empty commit", func(cwd string) error {
			command := gitCommand(cwd, "-c", "user.name=Executor", "-c", "user.email=executor@example.test",
				"commit", "--allow-empty", "-m", "Nothing changed")
			if output, err := command.CombinedOutput(); err != nil {
				return fmt.Errorf("empty commit: %v: %s", err, output)
			}
			return nil
		}, "The change set is empty against the source revision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScriptedFixture(t)
			routes, script := fixture.routes, fixture.script
			script.Queue(routes.Executor, runnertest.Reply{Answer: "Nothing needed changing", Effect: test.effect})
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture.planningFixture, task)

			saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
			if !blockedAs(saved, model.BlockedReasonVerificationFailed) {
				t.Fatalf("unchanged outcome = %+v", saved)
			}
			if saved.Error == nil || !strings.Contains(*saved.Error, test.want) {
				t.Fatalf("unchanged error = %q; want it to contain %q", optionalText(saved.Error), test.want)
			}
			if executors := sessionByRole(saved, "executor"); len(executors) != 1 || executors[0].Status != model.SessionCompleted {
				t.Fatalf("executor session = %+v", executors)
			}
			if len(saved.Reviews) != 0 || len(script.Turns(routes.Reviewer)) != 0 || len(script.Turns(routes.Repair)) != 0 {
				t.Fatalf("an unchanged task reached review or repair: %+v", saved.Reviews)
			}
			assertUnpublished(t, fixture, saved)
			assertAdmissions(t, fixture.state, 1, "the executor turn only")
			assertNoOpenClients(t, script)
		})
	}
}

func TestExecutionTaskTimeout(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	routes, script := fixture.routes, fixture.script
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Never delivered", Gate: gate})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Config.TaskTimeoutSeconds = 3
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)

	type outcome struct {
		task model.Task
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		saved, err := driveTaskResult(fixture.planningFixture, app, task.ID)
		done <- outcome{saved, err}
	}()
	select {
	case <-gate.Entered():
	case <-time.After(30 * time.Second):
		t.Fatal("the executor turn was never reached before the deadline")
	}
	finished := <-done
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	saved := finished.task
	if !blockedAs(saved, model.BlockedReasonTimeout) {
		t.Fatalf("timeout outcome = %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, "time limit") {
		t.Fatalf("timeout error evidence = %s", optionalText(saved.Error))
	}
	executors := sessionByRole(saved, "executor")
	if len(executors) != 1 || executors[0].Status != model.SessionFailed || !strings.Contains(executors[0].Summary, "time limit") {
		t.Fatalf("timed-out executor session = %+v", executors)
	}
	if marked, _ := fixture.state.MarkerSet("cancel", task.ID); marked {
		t.Fatal("a timeout must not be recorded as an operator cancellation")
	}
	assertUnpublished(t, fixture, saved)
	assertNoOpenClients(t, script)
}

func optionalText(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}
