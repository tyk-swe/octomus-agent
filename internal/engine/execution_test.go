package engine

// Task execution: executor, fresh reviewers, the persistent repair thread, verification and publication.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
)

func TestExecutionPublishes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("First pass looks complete"), cleanReview("Repair verified"))
	script.Queue(routes.Repair, runnertest.Reply{Answer: "Wrote the fixed output", Effect: writeFile("feature.txt", "fixed output\n")})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if saved.Status != model.StatusPublished || saved.PRNumber == nil || saved.OutputCommit == nil {
		t.Fatalf("scripted task did not publish: %+v", saved)
	}
	if len(saved.Reviews) != 2 || len(saved.Verification) != 2 || saved.Verification[0].Success || !saved.Verification[1].Success {
		t.Fatalf("review/verification evidence: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
	}
	for role, want := range map[string]int{"executor": 1, "reviewer": 2, "repair": 1} {
		sessions := sessionByRole(saved, role)
		if len(sessions) != want {
			t.Fatalf("%s sessions = %+v", role, sessions)
		}
		for _, session := range sessions {
			if session.Status != model.SessionCompleted {
				t.Fatalf("%s session not completed: %+v", role, session)
			}
		}
	}
	if summary := sessionByRole(saved, "executor")[0].Summary; summary != "Created feature.txt" {
		t.Fatalf("executor summary = %q", summary)
	}
	executorTurns := script.Turns(routes.Executor)
	if len(executorTurns) != 1 || executorTurns[0].Cwd != saved.Workspace || executorTurns[0].Schema != nil ||
		executorTurns[0].Session != *saved.ExecutionSession {
		t.Fatalf("executor turns = %+v", executorTurns)
	}
	reviewStarts := script.Starts(routes.Reviewer)
	if len(reviewStarts) != 2 || reviewStarts[0].Resume != nil || reviewStarts[1].Resume != nil || reviewStarts[0].Session == reviewStarts[1].Session {
		t.Fatalf("reviewers must be fresh sessions: %+v", reviewStarts)
	}
	for _, turn := range script.Turns(routes.Reviewer) {
		if turn.Schema == nil {
			t.Fatalf("reviewer turn without a structured schema: %+v", turn)
		}
	}
	if repairs := script.Turns(routes.Repair); len(repairs) != 1 || saved.RepairSession == nil || repairs[0].Session != *saved.RepairSession {
		t.Fatalf("repair turns = %+v (session %v)", repairs, saved.RepairSession)
	}
	for _, route := range routes.all() {
		if pending := script.Pending(route); pending != 0 {
			t.Fatalf("%d replies left on %s", pending, route)
		}
	}
	assertNoOpenClients(t, script)
	assertAdmissions(t, f.state, 4, "executor + 2 reviewers + repair")
	if head := remoteHead(t, f, saved.Branch); head != *saved.OutputCommit {
		t.Fatalf("published head %s, output %s", head, *saved.OutputCommit)
	}
	prs := prsJSON(t, f)
	if len(prs) != 1 {
		t.Fatalf("expected exactly one PR, got %d", len(prs))
	}
	head, _ := prs[0]["head"].(map[string]any)
	body, _ := prs[0]["body"].(string)
	if head["ref"] != saved.Branch || prs[0]["state"] != "open" || !strings.Contains(body, "<!-- octomus:task:"+saved.ID+" -->") {
		t.Fatalf("publication PR identity wrong: %+v", prs[0])
	}
}

func TestPublicationIsRedacted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"grep -q fixed feature.txt", "echo " + secretToken}
	})
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Implemented using " + secretToken, Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("clean"))
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Proposal.Title = "Ship it " + secretToken
	task.Proposal.Problem = "Missing output; see " + secretToken
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if saved.Status != model.StatusPublished || saved.OutputCommit == nil {
		t.Fatalf("task did not publish: %+v", saved)
	}
	if saved.Proposal.Title != "Ship it "+secretToken || saved.Proposal.Problem != "Missing output; see "+secretToken {
		t.Fatalf("canonical proposal was rewritten: %+v", saved.Proposal)
	}
	if saved.Config.VerificationCommands[1] != "echo "+secretToken ||
		saved.Verification[len(saved.Verification)-1].Command != "echo "+secretToken {
		t.Fatalf("canonical command evidence changed: %+v / %+v", saved.Config.VerificationCommands, saved.Verification)
	}
	prs := prsJSON(t, f)
	if len(prs) != 1 {
		t.Fatalf("expected exactly one PR: %+v", prs)
	}
	title, _ := prs[0]["title"].(string)
	body, _ := prs[0]["body"].(string)
	if strings.Contains(title+"\n"+body, secretToken) {
		t.Fatalf("public metadata leaked the secret: title=%q body=%q", title, body)
	}
	if title != "Ship it [redacted]" {
		t.Fatalf("outbound title = %q; want the scrubbed form", title)
	}
	if !strings.Contains(body, "echo [redacted]") {
		t.Fatalf("command description was not scrubbed: %q", body)
	}
	if !strings.Contains(body, "<!-- octomus:task:"+saved.ID+" -->") ||
		!strings.Contains(body, "Reviewed commit: `"+*saved.OutputCommit+"`") {
		t.Fatalf("public body lost delivery identity: %q", body)
	}
	assertNoOpenClients(t, script)
}

func TestInvalidReviewsNeverPublish(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		answer  string
		reasons []model.BlockedReason
	}{
		{"malformed", "The change looks fine to me.", []model.BlockedReason{model.BlockedInvalidReview, model.BlockedRunnerUnavailable}},
		{"schema-invalid", `{"completed": "yes", "summary": "Looks fine", "findings": []}`, []model.BlockedReason{model.BlockedInvalidReview, model.BlockedRunnerUnavailable}},
		{"incomplete", `{"completed": false, "summary": "Ran out of time", "findings": []}`, []model.BlockedReason{model.BlockedInvalidReview}},
		{"blank-summary", `{"completed": true, "summary": "  ", "findings": []}`, []model.BlockedReason{model.BlockedInvalidReview}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			routes, script := f.routes, f.script
			script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
			script.Answer(routes.Reviewer, test.answer)
			task := executionTask(t, f, f.cfg.DefaultBranch)
			putTask(t, f, task)

			saved := driveTask(t, f, f.newApp(t), task.ID)
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
			assertUnpublished(t, f, saved)
			assertAdmissions(t, f.state, 2, "executor + reviewer")
			assertNoOpenClients(t, script)
		})
	}
}

func TestReviewerEditBlocks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"test -f feature.txt"} })
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Queue(routes.Reviewer, runnertest.Reply{Answer: cleanReview("Looks fine"), Effect: writeFile("stray.txt", "reviewer edit\n")})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedWorkspaceInvalid) {
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
	assertUnpublished(t, f, saved)
	assertAdmissions(t, f.state, 2, "executor + reviewer")
	assertNoOpenClients(t, script)
}

func TestRepairBudgetExhausted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxRepairRounds = 2
		cfg.MaxNoProgressRounds = 5
	})
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("Round one"), cleanReview("Round two"), cleanReview("Round three"))
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "Repair one", Effect: writeFile("feature.txt", "repair one\n")},
		runnertest.Reply{Answer: "Repair two", Effect: writeFile("feature.txt", "repair two\n")},
		runnertest.Reply{Answer: "Repair three", Effect: writeFile("feature.txt", "repair three\n")})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedVerificationFailed) {
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
	assertUnpublished(t, f, saved)
	assertAdmissions(t, f.state, 6, "executor + 3 reviewers + 2 repairs")
	assertNoOpenClients(t, script)
}

func TestNoChangesBlocks(t *testing.T) {
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
			t.Parallel()
			f := newFixture(t)
			routes, script := f.routes, f.script
			script.Queue(routes.Executor, runnertest.Reply{Answer: "Nothing needed changing", Effect: test.effect})
			task := executionTask(t, f, f.cfg.DefaultBranch)
			putTask(t, f, task)

			saved := driveTask(t, f, f.newApp(t), task.ID)
			if !blockedAs(saved, model.BlockedVerificationFailed) {
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
			assertUnpublished(t, f, saved)
			assertAdmissions(t, f.state, 1, "the executor turn only")
			assertNoOpenClients(t, script)
		})
	}
}

func TestTaskTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	routes, script := f.routes, f.script
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Never delivered", Gate: gate})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Config.TaskTimeoutSeconds = 3
	putTask(t, f, task)
	app := f.newApp(t)

	type outcome struct {
		task model.Task
		err  error
	}
	done := make(chan outcome, 1)
	ctx, cancel := fixtureContext(t, taskWaitTimeout)
	defer cancel()
	go func() {
		saved, err := driveTaskResult(ctx, f, app, task.ID)
		done <- outcome{saved, err}
	}()
	select {
	case <-gate.Entered():
	case <-time.After(30 * time.Second):
		t.Fatal("the executor turn was never reached before the deadline")
	}
	var finished outcome
	select {
	case finished = <-done:
	case <-ctx.Done():
		app.cancel()
		t.Fatalf("task timeout test did not finish: %v\n%s", ctx.Err(), fixtureDiagnostics(app))
	}
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	saved := finished.task
	if !blockedAs(saved, model.BlockedTimeout) {
		t.Fatalf("timeout outcome = %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, "time limit") {
		t.Fatalf("timeout error evidence = %s", optionalText(saved.Error))
	}
	executors := sessionByRole(saved, "executor")
	if len(executors) != 1 || executors[0].Status != model.SessionFailed || !strings.Contains(executors[0].Summary, "time limit") {
		t.Fatalf("timed-out executor session = %+v", executors)
	}
	if marked, _ := f.state.Marked("cancel", task.ID); marked {
		t.Fatal("a timeout must not be recorded as an operator cancellation")
	}
	assertUnpublished(t, f, saved)
	assertNoOpenClients(t, script)
}

// The reviewer's turn moves the remote default branch under the task: publication must refuse the stale base.
func TestRemoteConflictBlocks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	remote := filepath.Join(f.root, "remote.git")
	var external string
	advance := func(string) error {
		parent, err := gitOutput(f.root, "--git-dir", remote, "rev-parse", "main")
		if err != nil {
			return err
		}
		tree, err := gitOutput(f.root, "--git-dir", remote, "rev-parse", parent+"^{tree}")
		if err != nil {
			return err
		}
		commit, err := gitOutput(f.root, "--git-dir", remote, "-c", "user.name=External", "-c", "user.email=external@example.com",
			"commit-tree", tree, "-p", parent, "-m", "External work")
		if err != nil {
			return err
		}
		if _, err := gitOutput(f.root, "--git-dir", remote, "update-ref", "refs/heads/main", commit); err != nil {
			return err
		}
		external = commit
		return nil
	}
	f.script.Queue(f.routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	f.script.Queue(f.routes.Reviewer, runnertest.Reply{Answer: cleanReview("Complete"), Effect: advance})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedStaleBase) {
		t.Fatalf("remote conflict outcome = %+v", saved)
	}
	if saved.OutputCommit != nil || len(publications(t, f)) != 0 {
		t.Fatalf("stale context authorized publication: %+v", saved)
	}
	if head := remoteHead(t, f, "main"); external == "" || head != external {
		t.Fatalf("remote main = %s; want the external commit %q", head, external)
	}
}

// dependentTasks are two tasks on the existing PR branch, the second depending on the first.
func dependentTasks(t *testing.T, f *fixture, head string) (model.Task, model.Task) {
	t.Helper()
	number := uint64(42)
	url := "https://github.com/fixture/project/pull/42"
	onPR := func() model.Task {
		task := executionTask(t, f, "octomus/existing")
		task.Branch = "octomus/existing"
		task.SourceRevision = head
		task.PRNumber = &number
		task.PRURL = &url
		return task
	}
	first, second := onPR(), onPR()
	second.Proposal.ID = "d0-followup"
	second.Proposal.Title = "Complete the next fixture feature"
	second.Proposal.Prompt = "Implement the next fixture capability. fixture-file=feature-next.txt"
	second.Proposal.Dependencies = []string{first.ID}
	return first, second
}

func TestDependencyOrderAndRollback(t *testing.T) {
	t.Parallel()
	t.Run("orders onto dependency output", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		first, second := dependentTasks(t, f, existingPRBranch(t, f))
		f.script.Queue(f.routes.Executor,
			runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")},
			runnertest.Reply{Answer: "Created feature-next.txt", Effect: writeFile("feature-next.txt", "fixed\n")})
		f.script.Answer(f.routes.Reviewer, cleanReview("First"), cleanReview("Second"))
		putTask(t, f, first)
		putTask(t, f, second)
		app := f.newApp(t)
		delivered := driveTask(t, f, app, first.ID)
		if delivered.Status != model.StatusPublished {
			t.Fatalf("dependency delivery = %+v", delivered)
		}
		saved := driveTask(t, f, app, second.ID)
		if saved.Status != model.StatusPublished {
			t.Fatalf("dependent delivery = %+v", saved)
		}
		if saved.SourceRevision != *delivered.OutputCommit {
			t.Fatalf("dependent source = %s; want dependency output %s", saved.SourceRevision, *delivered.OutputCommit)
		}
		if _, err := os.Stat(filepath.Join(saved.Workspace, "feature.txt")); err != nil {
			t.Fatalf("dependent workspace lost the dependency output: %v", err)
		}
		entries := publications(t, f)
		if len(entries) != 2 || entries[0]["action"] != "comment" || entries[1]["action"] != "comment" {
			t.Fatalf("publications = %+v; want two follow-up comments", entries)
		}
	})
	t.Run("rewound dependency output blocks", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		head := existingPRBranch(t, f)
		first, second := dependentTasks(t, f, head)
		f.script.Queue(f.routes.Executor,
			runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")},
			runnertest.Reply{Answer: "Created feature-next.txt", Effect: writeFile("feature-next.txt", "fixed\n")})
		f.script.Answer(f.routes.Reviewer, cleanReview("First"), cleanReview("Second"))
		putTask(t, f, first)
		putTask(t, f, second)
		app := f.newApp(t)
		delivered := driveTask(t, f, app, first.ID)
		if delivered.Status != model.StatusPublished {
			t.Fatalf("dependency delivery = %+v", delivered)
		}
		git(t, f.repo, "fetch", filepath.Join(f.root, "remote.git"), "octomus/existing")
		git(t, f.root, "--git-dir", filepath.Join(f.root, "remote.git"), "update-ref", "refs/heads/octomus/existing", head)
		saved := driveTask(t, f, app, second.ID)
		if !blockedAs(saved, model.BlockedDependencyBlocked) {
			t.Fatalf("rollback dependent outcome = %+v; want dependency_blocked", saved)
		}
		if saved.OutputCommit != nil {
			t.Fatalf("rollback authorized publication: %+v", saved)
		}
	})
}
