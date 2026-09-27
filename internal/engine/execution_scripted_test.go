package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func tickUntil(t *testing.T, app *App, done <-chan struct{}, label string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := app.Tick(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
	t.Fatalf("%s was not reached", label)
}

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

func TestExecutionNoProgressLimitStopsIdenticalRepairs(t *testing.T) {
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxRepairRounds = 4
		cfg.MaxNoProgressRounds = 1
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("Round one"), cleanReview("Round two"), cleanReview("Round three"))
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "Wrote fixed output", Effect: writeFile("feature.txt", "fixed\n")},
		runnertest.Reply{Answer: "Rewrote fixed output", Effect: writeFile("feature.txt", "fixed\n")},
		runnertest.Reply{Answer: "Unused", Effect: writeFile("feature.txt", "other\n")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedReasonVerificationFailed) {
		t.Fatalf("no-progress outcome = %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, "Repairs made no progress") {
		t.Fatalf("the block must name the no-progress limit: %q", optionalText(saved.Error))
	}
	if len(saved.Reviews) != 3 {
		t.Fatalf("reviews = %+v; want 3 rounds ending on the repeated revision", saved.Reviews)
	}
	if saved.Reviews[0].Revision == saved.Reviews[1].Revision || saved.Reviews[1].Revision != saved.Reviews[2].Revision {
		t.Fatalf("no-progress signature missing: %+v", saved.Reviews)
	}
	if turns := script.Turns(routes.Repair); len(turns) != 2 || script.Pending(routes.Repair) != 1 {
		t.Fatalf("repair turns = %d, pending = %d; the no-progress limit must stop before the repair cap", len(turns), script.Pending(routes.Repair))
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 6, "executor + 3 reviewers + 2 repairs")
	assertNoOpenClients(t, script)
}

func TestExecutionRepairPromptCarriesRoundEvidence(t *testing.T) {
	t.Run("verification failure", func(t *testing.T) {
		fixture := newScriptedFixture(t)
		failing := "echo FAIL-$((40+2)); false"
		fixture.configure(t, func(cfg *config.Config) {
			cfg.VerificationCommands = []string{failing}
			cfg.MaxRepairRounds = 1
		})
		routes, script := fixture.routes, fixture.script
		script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
		script.Answer(routes.Reviewer, cleanReview("Round one"), cleanReview("Round two"))
		script.Queue(routes.Repair, runnertest.Reply{Answer: "Repaired", Effect: writeFile("feature.txt", "repaired\n")})
		task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
		saveExecutionTask(t, fixture.planningFixture, task)

		saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
		if !blockedAs(saved, model.BlockedReasonVerificationFailed) {
			t.Fatalf("failed verification outcome = %+v", saved)
		}
		turns := script.Turns(routes.Repair)
		if len(turns) != 1 {
			t.Fatalf("repair turns = %+v; want one", turns)
		}
		prompt := turns[0].Prompt
		_, failures, found := strings.Cut(prompt, "Verification failures: ")
		if !found || !strings.HasPrefix(failures, `["`+failing+": ") || !strings.Contains(failures, "FAIL-42") {
			t.Fatalf("repair prompt lacks the failing command and its output: %q", prompt)
		}
		if !strings.Contains(prompt, "Findings: []. Verification failures: ") {
			t.Fatalf("a clean review must hand repair an empty findings list: %q", prompt)
		}
		assertUnpublished(t, fixture, saved)
		assertNoOpenClients(t, script)
	})

	t.Run("review findings", func(t *testing.T) {
		fixture := newScriptedFixture(t)
		fixture.configure(t, func(cfg *config.Config) {
			cfg.VerificationCommands = []string{"test -f feature.txt"}
			cfg.MaxRepairRounds = 1
		})
		routes, script := fixture.routes, fixture.script
		finding := map[string]any{"title": "Handle the empty input", "file": "feature.txt:1", "detail": "The draft ignores empty input.", "priority": "P1"}
		review := mustJSON(t, map[string]any{"completed": true, "summary": "One finding", "findings": []any{finding}})
		script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
		script.Answer(routes.Reviewer, review, review)
		script.Queue(routes.Repair, runnertest.Reply{Answer: "Repaired", Effect: writeFile("feature.txt", "repaired\n")})
		task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
		saveExecutionTask(t, fixture.planningFixture, task)

		saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
		if !blockedAs(saved, model.BlockedReasonVerificationFailed) {
			t.Fatalf("unresolved finding outcome = %+v", saved)
		}
		if len(saved.Reviews) != 2 || len(saved.Verification) != 0 {
			t.Fatalf("a review with findings must skip verification: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
		}
		turns := script.Turns(routes.Repair)
		if len(turns) != 1 {
			t.Fatalf("repair turns = %+v; want one", turns)
		}
		want := `Findings: [{"title":"Handle the empty input","file":"feature.txt:1","detail":"The draft ignores empty input.","priority":"P1"}]. Verification failures: []`
		if !strings.HasSuffix(turns[0].Prompt, want) {
			t.Fatalf("repair prompt lacks the review findings: %q", turns[0].Prompt)
		}
		assertUnpublished(t, fixture, saved)
		assertNoOpenClients(t, script)
	})
}

func TestExecutionWithoutChangesBlocksBeforeReview(t *testing.T) {
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

func TestExecutionCancellationDuringTurn(t *testing.T) {
	fixture := newScriptedFixture(t)
	routes, script := fixture.routes, fixture.script
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Never delivered", Effect: writeFile("feature.txt", "fixed\n"), Gate: gate})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)

	tickUntil(t, app, gate.Entered(), "held executor turn")
	if running := loadTask(t, fixture.state, task.ID); running.Status != model.StatusExecuting {
		t.Fatalf("held turn task = %+v", running)
	}
	if err := app.TaskAction(context.Background(), task.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusCancelled {
		t.Fatalf("cancelled turn outcome = %+v", saved)
	}
	if marked, err := fixture.state.MarkerSet("cancel", task.ID); err != nil || !marked {
		t.Fatalf("cancel marker = %v, %v", marked, err)
	}
	if saved.Error == nil || *saved.Error != "Cancelled by the operator" || saved.BlockedReason != nil {
		t.Fatalf("cancelled task error = %q, reason = %v", optionalText(saved.Error), saved.BlockedReason)
	}
	executors := sessionByRole(saved, "executor")
	if len(executors) != 1 || executors[0].Status != model.SessionFailed || executors[0].Summary != "Cancelled by the operator" {
		t.Fatalf("cancelled executor session = %+v", executors)
	}
	if !hasEvent(t, fixture.state, task.ID, "error", "context canceled") {
		t.Fatal("the underlying cancellation cause was not recorded as an error event")
	}
	if len(saved.Reviews) != 0 || len(script.Turns(routes.Reviewer)) != 0 {
		t.Fatalf("a cancelled task reached review: %+v", saved.Reviews)
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 1, "the cancelled executor turn")
	assertNoOpenClients(t, script)
}

func TestExecutionTaskTimeout(t *testing.T) {
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

func TestExecutionTimeoutJoinsCallbackBeforeFinalizing(t *testing.T) {
	fixture := newScriptedFixture(t)
	routes, script := fixture.routes, fixture.script
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseTurn := func() { releaseOnce.Do(func() { close(release) }) }
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Late executor answer", Effect: func(string) error {
		close(entered)
		<-release
		return nil
	}})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Config.TaskTimeoutSeconds = 2
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)
	t.Cleanup(releaseTurn)

	tickUntil(t, app, entered, "non-cancellable executor turn")
	time.Sleep(11 * time.Second)
	if app.Drained() {
		t.Fatal("the worker released runtime ownership while its turn could still write")
	}
	if held := loadTask(t, fixture.state, task.ID); held.Status != model.StatusExecuting || held.BlockedReason != nil {
		t.Fatalf("task finalized before the turn returned: %+v", held)
	}

	releaseTurn()
	if !testutil.WaitUntil(30*time.Second, app.Drained) {
		t.Fatal("worker did not finish after the turn returned")
	}
	saved := loadTask(t, fixture.state, task.ID)
	if !blockedAs(saved, model.BlockedReasonTimeout) || saved.Error == nil || !strings.Contains(*saved.Error, "time limit") {
		t.Fatalf("timeout evidence lost to the late write: %+v", saved)
	}
	executors := sessionByRole(saved, "executor")
	if len(executors) != 1 || executors[0].Status != model.SessionCompleted || executors[0].Summary != "Late executor answer" {
		t.Fatalf("late executor write = %+v", executors)
	}
	for _, session := range saved.Sessions {
		if session.Status == model.SessionRunning {
			t.Fatalf("finalized task kept a running session: %+v", saved.Sessions)
		}
	}
	assertUnpublished(t, fixture, saved)
	assertNoOpenClients(t, script)
}

func TestExecutionDeadlineCallbackPanicBlocks(t *testing.T) {
	fixture := newScriptedFixture(t)
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Effect: func(string) error { panic("executor exploded") }})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil || !strings.Contains(*saved.Error, "Task worker panicked: executor exploded") {
		t.Fatalf("panic outcome = %+v", saved)
	}
	executors := sessionByRole(saved, "executor")
	if len(saved.Sessions) != 1 || len(executors) != 1 || executors[0].Status != model.SessionFailed {
		t.Fatalf("panic did not fail the running session: %+v", saved.Sessions)
	}
	if !hasEvent(t, fixture.state, task.ID, "error", "executor exploded") {
		t.Fatal("panic did not record a supervisor error event")
	}
	assertNoOpenClients(t, script)
}

func TestExecutionFailedExecutorStartRetries(t *testing.T) {
	fixture := newScriptedFixture(t, withGitHubIdentity())
	routes, script := fixture.routes, fixture.script
	script.FailStart(routes.Executor, errors.New("Fixture failed start"))
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("Complete"))
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)

	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if !blockedAs(saved, model.BlockedReasonRunnerUnavailable) {
		t.Fatalf("failed start outcome = %+v", saved)
	}
	if saved.ExecutionSession != nil || len(saved.Sessions) != 0 || saved.Error == nil || !strings.Contains(*saved.Error, "Fixture failed start") {
		t.Fatalf("failed start evidence = %+v", saved)
	}
	if saved.Workspace == "" || saved.ComparisonBase == "" {
		t.Fatalf("failed start lost the initialized clone: %+v", saved)
	}
	clone := saved.Workspace
	assertAdmissions(t, fixture.state, 1, "the reserved executor admission")

	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	saved = driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusPublished || saved.PRNumber == nil {
		t.Fatalf("retried delivery = %+v", saved)
	}
	if saved.Workspace != clone {
		t.Fatalf("retry moved the workspace from %s to %s", clone, saved.Workspace)
	}
	starts := script.Starts(routes.Executor)
	if len(starts) != 2 || starts[0].Cwd != clone || starts[1].Cwd != clone || starts[1].Resume != nil ||
		saved.ExecutionSession == nil || starts[1].Session != *saved.ExecutionSession {
		t.Fatalf("executor starts = %+v; want a failed then a fresh start in the same clone", starts)
	}
	if executors := sessionByRole(saved, "executor"); len(executors) != 1 || executors[0].Status != model.SessionCompleted {
		t.Fatalf("executor sessions = %+v", executors)
	}
	assertAdmissions(t, fixture.state, 3, "1 failed start + executor + reviewer")
	if entries := publications(t, fixture.planningFixture); len(entries) != 1 || entries[0]["action"] != "create" {
		t.Fatalf("publications = %+v; want one create", entries)
	}
	assertNoOpenClients(t, script)
}

func TestExecutionRestartRequeuesInitializedTask(t *testing.T) {
	fixture := newScriptedFixture(t, withGitHubIdentity())
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"test -f feature.txt"}
	})
	routes, script := fixture.routes, fixture.script
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	ctx := context.Background()
	ws := filepath.Join(fixture.dataDir, "tasks", task.ID, "workspace")
	if err := gitops.CloneAt(ctx, fixture.cfg, ws, task.SourceRevision); err != nil {
		t.Fatal(err)
	}
	previous, err := script.Connector()(ctx, routes.Executor.Backend, fixture.cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := previous.Start(routes.Executor, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	task.Workspace = ws
	task.ComparisonBase = task.SourceRevision
	task.ExecutionSession = &thread
	task.Status = model.StatusExecuting
	task.Sessions = []model.Session{{ID: thread, Role: "executor", Status: model.SessionRunning}}
	saveExecutionTask(t, fixture.planningFixture, task)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("Complete"))

	app := New(fixture.state, fixture.dataDir, WithRunnerConnector(script.Connector()))
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	if err := app.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered := loadTask(t, fixture.state, task.ID)
	if recovered.Status != model.StatusQueued || recovered.Attempts != 1 {
		t.Fatalf("interrupted task recovery = %+v", recovered)
	}
	if executors := sessionByRole(recovered, "executor"); len(executors) != 1 || executors[0].Status != model.SessionInterrupted {
		t.Fatalf("running session not interrupted: %+v", executors)
	}
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("requeued delivery = %+v", saved)
	}
	if saved.Workspace != ws || saved.ExecutionSession == nil || *saved.ExecutionSession != thread {
		t.Fatalf("requeued task changed its workspace or thread: %+v", saved)
	}
	if executors := sessionByRole(saved, "executor"); len(executors) != 1 || executors[0].Status != model.SessionCompleted || executors[0].Summary != "Created feature.txt" {
		t.Fatalf("resumed executor session = %+v", executors)
	}
	starts := script.Starts(routes.Executor)
	if len(starts) != 2 || starts[1].Resume == nil || *starts[1].Resume != thread || starts[1].Cwd != ws {
		t.Fatalf("executor starts = %+v; want one resume of %s", starts, thread)
	}
	if turns := script.Turns(routes.Executor); len(turns) != 1 || turns[0].Session != thread {
		t.Fatalf("executor turns = %+v", turns)
	}
	assertAdmissions(t, fixture.state, 2, "resumed executor + reviewer")
	assertNoOpenClients(t, script)
}

func advanceRemoteMain(fixture *scriptedFixture) error {
	remote := filepath.Join(fixture.root, "remote.git")
	git := func(args ...string) (string, error) {
		out, err := gitCommand(fixture.root, append([]string{"--git-dir", remote}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %v: %w: %s", args, err, out)
		}
		return strings.TrimSpace(string(out)), nil
	}
	tree, err := git("rev-parse", "main^{tree}")
	if err != nil {
		return err
	}
	next, err := git("-c", "user.name=External", "-c", "user.email=fixture@example.com", "commit-tree", tree, "-p", "main", "-m", "External main change")
	if err != nil {
		return err
	}
	_, err = git("update-ref", "refs/heads/main", next)
	return err
}

func existingPrTask(t *testing.T, fixture *scriptedFixture) model.Task {
	t.Helper()
	head := existingPrBranch(t, fixture.planningFixture)
	task := executionTask(t, fixture.planningFixture, "octomus/existing")
	task.Branch = "octomus/existing"
	task.SourceRevision = head
	number := uint64(42)
	task.PRNumber = &number
	url := "https://github.com/fixture/project/pull/42"
	task.PRURL = &url
	return task
}

func TestExecutionExistingPrStaleBaseBlocksBeforeCheckpoint(t *testing.T) {
	fixture := newScriptedFixture(t, withGitHubIdentity())
	routes, script := fixture.routes, fixture.script
	task := existingPrTask(t, fixture)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: func(cwd string) error {
		if err := writeFile("feature.txt", "fixed\n")(cwd); err != nil {
			return err
		}
		return advanceRemoteMain(fixture)
	}})
	script.Answer(routes.Reviewer, cleanReview("Complete"))
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedReasonStaleBase) {
		t.Fatalf("moved default branch outcome = %+v", saved)
	}
	if saved.OutputCommit != nil || len(publications(t, fixture.planningFixture)) != 0 {
		t.Fatalf("an unpublishable checkpoint was recorded: %+v", saved)
	}
	if !slices.Contains(saved.AllowedActions(), "cancel") {
		t.Fatalf("stale existing-PR task lost its cancel action: %v", saved.AllowedActions())
	}
	if len(saved.Reviews) != 1 || len(saved.Verification) == 0 {
		t.Fatalf("the block must follow a clean, verified review: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
	}
	assertNoOpenClients(t, script)
}

func TestExecutionExistingPrComparisonBaseSurvivesMainMovingAfterClone(t *testing.T) {
	fixture := newScriptedFixture(t, withGitHubIdentity())
	routes, script := fixture.routes, fixture.script
	task := existingPrTask(t, fixture)
	ws := filepath.Join(fixture.dataDir, "tasks", task.ID, "workspace")
	moved := filepath.Join(fixture.root, "main-moved")
	uploadPack := filepath.Join(fixture.root, "moving-upload-pack")
	remote := filepath.Join(fixture.root, "remote.git")
	body := fmt.Sprintf(`#!/bin/sh
if [ -d %[1]q ] && [ ! -e %[2]q ]; then
  touch %[2]q
  tree=$(/usr/bin/git --git-dir %[3]q rev-parse 'main^{tree}') || exit 1
  next=$(/usr/bin/git --git-dir %[3]q -c user.name=External -c user.email=fixture@example.com commit-tree "$tree" -p main -m 'External main change') || exit 1
  /usr/bin/git --git-dir %[3]q update-ref refs/heads/main "$next" || exit 1
fi
exec git-upload-pack "$@"
`, filepath.Join(ws, ".git"), moved, remote)
	if err := os.WriteFile(uploadPack, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, fixture.repo, "config", "remote.origin.uploadpack", uploadPack)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("Complete"))
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("main never moved after the clone: %v", err)
	}
	if !blockedAs(saved, model.BlockedReasonStaleBase) || saved.OutputCommit != nil {
		t.Fatalf("main moving after the clone = %+v; want a stale base before the checkpoint", saved)
	}
	if base := git(t, saved.Workspace, "merge-base", task.DefaultRevision, task.SourceRevision); saved.ComparisonBase != base {
		t.Fatalf("comparison base = %q; want the merge base %s of the verified default revision", saved.ComparisonBase, base)
	}
	if len(saved.Reviews) != 1 || saved.Reviews[0].ComparisonBase != saved.ComparisonBase {
		t.Fatalf("review rounds must record the comparison base: %+v", saved.Reviews)
	}
	if executors := sessionByRole(saved, "executor"); len(executors) != 1 || executors[0].Status != model.SessionCompleted {
		t.Fatalf("initialization did not complete before the executor: %+v", executors)
	}
	assertNoOpenClients(t, script)
}

func taskEventKinds(t *testing.T, state *store.Store, id string) map[string]int {
	t.Helper()
	events, err := state.Events(&id)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, event := range events {
		kinds[event.Kind]++
	}
	return kinds
}

func optionalText(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func hasEvent(t *testing.T, state *store.Store, id, kind, text string) bool {
	t.Helper()
	events, err := state.Events(&id)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == kind && strings.Contains(event.Message, text) {
			return true
		}
	}
	return false
}

func TestExecutionShutdownLeavesInitializedTaskForRecovery(t *testing.T) {
	fixture := newScriptedFixture(t, withGitHubIdentity())
	routes, script := fixture.routes, fixture.script
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Never delivered", Gate: gate})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)

	tickUntil(t, app, gate.Entered(), "held executor turn")
	app.Shutdown()
	stopped := loadTask(t, fixture.state, task.ID)
	if stopped.Status != model.StatusExecuting || stopped.ExecutionSession == nil || stopped.Error != nil || stopped.BlockedReason != nil {
		t.Fatalf("graceful stop recorded a task outcome: %+v", stopped)
	}
	thread := *stopped.ExecutionSession
	if executors := sessionByRole(stopped, "executor"); len(executors) != 1 || executors[0].Status != model.SessionRunning {
		t.Fatalf("graceful stop finalized the executor session: %+v", executors)
	}
	if kinds := taskEventKinds(t, fixture.state, task.ID); kinds["interrupted"] != 1 || kinds["error"] != 0 {
		t.Fatalf("graceful stop events = %v; want one interruption and no error", kinds)
	}
	if marked, _ := fixture.state.MarkerSet("cancel", task.ID); marked {
		t.Fatal("a graceful stop must not be recorded as an operator cancellation")
	}
	assertNoOpenClients(t, script)

	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("Complete"))
	restarted := New(fixture.state, fixture.dataDir, WithRunnerConnector(script.Connector()))
	t.Cleanup(restarted.Shutdown)
	deferHousekeeping(restarted)
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered := loadTask(t, fixture.state, task.ID)
	if recovered.Status != model.StatusQueued || recovered.Attempts != 1 || recovered.BlockedReason != nil {
		t.Fatalf("interrupted task recovery = %+v", recovered)
	}
	if executors := sessionByRole(recovered, "executor"); len(executors) != 1 || executors[0].Status != model.SessionInterrupted {
		t.Fatalf("running session not interrupted by recovery: %+v", executors)
	}
	if err := restarted.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture.planningFixture, restarted, task.ID)
	if saved.Status != model.StatusPublished || saved.ExecutionSession == nil || *saved.ExecutionSession != thread {
		t.Fatalf("recovered delivery = %+v", saved)
	}
	starts := script.Starts(routes.Executor)
	if len(starts) != 2 || starts[0].Resume != nil || starts[1].Resume == nil || *starts[1].Resume != thread {
		t.Fatalf("executor starts = %+v; want the interrupted thread resumed", starts)
	}
	if entries := publications(t, fixture.planningFixture); len(entries) != 1 || entries[0]["action"] != "create" {
		t.Fatalf("publications = %+v; want exactly one create", entries)
	}
	assertAdmissions(t, fixture.state, 3, "interrupted executor + resumed executor + reviewer")
	assertNoOpenClients(t, script)
}

func TestExecutionShutdownBeforeInitializationStaysRetryable(t *testing.T) {
	fixture := newScriptedFixture(t)
	heldUploadPack(t, fixture.planningFixture)
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusExecuting
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.pausedApp(t)
	app.runTask(task)
	waitForPreflights(t, fixture.planningFixture, 1)

	app.Shutdown()
	stopped := loadTask(t, fixture.state, task.ID)
	if stopped.Status != model.StatusBlocked || stopped.ExecutionSession != nil || stopped.Workspace != "" {
		t.Fatalf("stop before initialization = %+v", stopped)
	}
	if !slices.Contains(stopped.AllowedActions(), "retry") {
		t.Fatalf("stop before initialization is not retryable: %v (%+v)", stopped.AllowedActions(), stopped)
	}
	restarted := New(fixture.state, fixture.dataDir, WithRunnerConnector(fixture.script.Connector()))
	t.Cleanup(restarted.Shutdown)
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	if recovered := loadTask(t, fixture.state, task.ID); !wirejson.Equal(&recovered, &stopped) {
		t.Fatalf("recovery changed a retryable block: %+v", recovered)
	}
	assertAdmissions(t, fixture.state, 0, "no work was admitted before the preflight")
	assertNoOpenClients(t, fixture.script)
}
