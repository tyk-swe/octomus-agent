package engine

// Execution lifecycle tests on the scripted runner adapter (package
// runnertest) with real local Git: review, verification and repair budgets,
// cancellation, task deadlines, executor-start retry and restart re-queue.
// Replies are keyed by route, never by prompt text. Only the tests that reach
// publication put the git.py shim on PATH (withGitHubIdentity), because remote
// validation still needs a github.com origin until the GitHub port (#8).

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// tickUntil ticks the scheduler, without joining workers, until done closes.
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

// loadTask reads the durable task record.
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

func assertAdmissions(t *testing.T, state *store.Store, want uint64, label string) {
	t.Helper()
	if used, err := state.SessionsToday(); err != nil || used != want {
		t.Fatalf("admissions = %d, %v; want %d (%s)", used, err, want, label)
	}
}

func assertNoOpenClients(t *testing.T, script *runnertest.Script) {
	t.Helper()
	if open := script.OpenClients(); open != 0 {
		t.Fatalf("%d runner clients left open", open)
	}
}

// assertUnpublished checks that neither the task record nor the GitHub peer
// saw a publication.
func assertUnpublished(t *testing.T, fixture *scriptedFixture, task model.Task) {
	t.Helper()
	if task.OutputCommit != nil || task.PRNumber != nil || len(publications(t, fixture.planningFixture)) != 0 {
		t.Fatalf("task authorized publication: %+v", task)
	}
}

// TestExecutionMalformedAndIncompleteReviewsNeverPublish: a reviewer answer
// that is unparseable, schema-invalid, incomplete or clean without a summary
// never counts as a clean review; the task blocks before verification.
func TestExecutionMalformedAndIncompleteReviewsNeverPublish(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer string
		// reasons are the acceptable blocked reasons: a structurally broken
		// answer fails the structured turn itself (runner_unavailable), while
		// a well-formed but unusable review is an invalid review.
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

// TestExecutionFailedVerificationExhaustsRepairBudget: every review is clean
// but verification fails every time; the repair budget, not the reviewer,
// decides the outcome, and one persistent repair thread carries every round.
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
	// Each repair makes progress, so only the repair budget can end the task.
	// The third reply must stay unconsumed.
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

// TestExecutionNoProgressLimitStopsIdenticalRepairs: a repair that reproduces
// the same tree yields the identical snapshot revision; the no-progress
// budget, not the repair cap, ends the task.
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
	// The first repair progresses; the second rewrites identical output.
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

// TestExecutionCancellationDuringTurn: the operator cancel reaches a running
// executor turn; the durable outcome is cancelled, not failed, and the turn's
// session is not recorded as completed.
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
	executors := sessionByRole(saved, "executor")
	if len(executors) != 1 || executors[0].Status != model.SessionFailed || strings.Contains(executors[0].Summary, "Never delivered") {
		t.Fatalf("cancelled executor session = %+v", executors)
	}
	if len(saved.Reviews) != 0 || len(script.Turns(routes.Reviewer)) != 0 {
		t.Fatalf("a cancelled task reached review: %+v", saved.Reviews)
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 1, "the cancelled executor turn")
	assertNoOpenClients(t, script)
}

// TestExecutionTaskTimeout: the task deadline fires while an executor turn is
// in flight; the durable outcome is a timed-out block, not a runner failure or
// a cancellation, and the running session is failed.
func TestExecutionTaskTimeout(t *testing.T) {
	fixture := newScriptedFixture(t)
	routes, script := fixture.routes, fixture.script
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Never delivered", Gate: gate})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	// The task snapshot carries the deadline; settings validation does not
	// apply to it, so the test need not wait out the ten-second minimum.
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
		t.Fatalf("timeout error evidence = %+v", saved.Error)
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

// TestExecutionTimeoutJoinsCallbackBeforeFinalizing: an executor turn that
// ignores cancellation outlives the task deadline and its cleanup grace. The
// worker must keep runtime ownership and leave the durable record alone until
// the turn returns, then record the timeout on top of the turn's late write.
func TestExecutionTimeoutJoinsCallbackBeforeFinalizing(t *testing.T) {
	fixture := newScriptedFixture(t)
	routes, script := fixture.routes, fixture.script
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseTurn := func() { releaseOnce.Do(func() { close(release) }) }
	// The effect models non-cancellable runner work: it ignores the task's
	// context entirely. The route has one reply, so it runs once.
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Late executor answer", Effect: func(string) error {
		close(entered)
		<-release
		return nil
	}})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Config.TaskTimeoutSeconds = 2
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)
	t.Cleanup(releaseTurn) // Runs before the app's Shutdown cleanup.

	tickUntil(t, app, entered, "non-cancellable executor turn")
	// Exceed the task deadline plus WithDeadline's eight-second grace.
	time.Sleep(11 * time.Second)
	if app.Drained() {
		t.Fatal("the worker released runtime ownership while its turn could still write")
	}
	if held := loadTask(t, fixture.state, task.ID); held.Status != model.StatusExecuting || held.BlockedReason != nil {
		t.Fatalf("task finalized before the turn returned: %+v", held)
	}

	releaseTurn()
	deadline := time.Now().Add(30 * time.Second)
	for !app.Drained() {
		if time.Now().After(deadline) {
			t.Fatal("worker did not finish after the turn returned")
		}
		time.Sleep(20 * time.Millisecond)
	}
	saved := loadTask(t, fixture.state, task.ID)
	if !blockedAs(saved, model.BlockedReasonTimeout) || saved.Error == nil || !strings.Contains(*saved.Error, "time limit") {
		t.Fatalf("timeout evidence lost to the late write: %+v", saved)
	}
	// The turn's late write is kept, underneath the terminal timeout record.
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

// TestExecutionDeadlineCallbackPanicBlocks: a panic inside the executor turn
// runs on the deadline callback's goroutine, beyond the worker's own recovery;
// supervision still blocks the task, fails the running session and records the
// panic.
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
	events, err := fixture.state.Events(&task.ID)
	if err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, event := range events {
		recorded = recorded || (event.Kind == "error" && strings.Contains(event.Message, "executor exploded"))
	}
	if !recorded {
		t.Fatal("panic did not record a supervisor error event")
	}
	assertNoOpenClients(t, script)
}

// TestExecutionFailedExecutorStartRetries: a runner start failure keeps the
// reserved admission and the initialized clone, blocks without a session, and
// an operator retry redelivers in the same clone with exactly one new executor
// admission.
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

// TestExecutionRestartRequeuesInitializedTask: a task that died mid-execution
// with an intact workspace is re-queued on restart and resumes its executor
// session instead of starting over.
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
	// The previous process started the executor thread; the runner still
	// knows it, so a resume can succeed.
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
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
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
	// The seeding start plus exactly one engine start, which resumed the thread.
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
