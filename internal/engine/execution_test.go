package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func newExecutionFixture(t *testing.T) *planningFixture {
	t.Helper()
	fixture := newPlanningFixture(t)
	fixture.dataDir = filepath.Join(fixture.root, ".octomus")
	if err := os.MkdirAll(fixture.dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func remoteHead(t *testing.T, fixture *planningFixture, branch string) string {
	t.Helper()
	return git(t, fixture.root, "--git-dir", filepath.Join(fixture.root, "remote.git"), "rev-parse", branch)
}

func executionTask(t *testing.T, fixture *planningFixture, target string) model.Task {
	t.Helper()
	id := model.ID()
	cfg := fixture.cfg
	task := queuedTask(cfg, id, target, cfg.BranchPrefix+id)
	head := remoteHead(t, fixture, target)
	task.SourceRevision = head
	task.DefaultRevision = remoteHead(t, fixture, cfg.DefaultBranch)
	task.ComparisonBase = ""
	task.Proposal.Prompt = "Create feature.txt with fixed output and verify its contents. fixture-file=feature.txt"
	task.Proposal.Title = "Complete the fixture feature"
	return task
}

func saveExecutionTask(t *testing.T, fixture *planningFixture, task model.Task) {
	t.Helper()
	if err := fixture.state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
}

func driveTask(t *testing.T, fixture *planningFixture, app *App, taskID string) model.Task {
	t.Helper()
	task, err := driveTaskResult(fixture, app, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func driveTaskResult(fixture *planningFixture, app *App, taskID string) (model.Task, error) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := app.Tick(); err != nil {
			return model.Task{}, err
		}
		app.wg.Wait()
		task, err := store.Get[model.Task](fixture.state, "task", taskID)
		if err != nil {
			return model.Task{}, err
		}
		if task == nil {
			return model.Task{}, errors.New("task vanished")
		}
		switch task.Status {
		case model.StatusPublished, model.StatusBlocked, model.StatusFailed, model.StatusCancelled:
			return *task, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	current, _ := store.Get[model.Task](fixture.state, "task", taskID)
	return model.Task{}, fmt.Errorf("task %s did not finish: %+v", taskID, current)
}

func sessionByRole(task model.Task, role string) []model.Session {
	sessions := []model.Session{}
	for _, s := range task.Sessions {
		if s.Role == role {
			sessions = append(sessions, s)
		}
	}
	return sessions
}

func TestExecutionDeliversFullLifecycle(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.VerificationCommands = []string{"test -f feature.txt", "grep -q fixed feature.txt"}
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	task := executionTask(t, fixture, cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task did not publish: %+v", saved)
	}
	if saved.OutputCommit == nil || *saved.OutputCommit == saved.SourceRevision {
		t.Fatalf("missing output checkpoint: %s", optionalText(saved.OutputCommit))
	}
	if saved.PRNumber == nil || saved.PRURL == nil || !strings.Contains(*saved.PRURL, "github.com/fixture/project/pull/") {
		t.Fatalf("missing publication identity: %+v", saved)
	}
	if len(saved.Reviews) != 3 {
		t.Fatalf("expected 3 review rounds, got %+v", saved.Reviews)
	}
	if !saved.Reviews[2].Result.Clean() || saved.Reviews[0].Result.Clean() || saved.Reviews[1].Result.Clean() {
		t.Fatalf("review evidence order wrong: %+v", saved.Reviews)
	}
	reviewThreads := map[string]bool{}
	for _, round := range saved.Reviews {
		if round.Revision == "" || round.ComparisonBase != saved.SourceRevision {
			t.Fatalf("review round lost revision/base: %+v", round)
		}
		if reviewThreads[round.SessionID] {
			t.Fatalf("review session %s reused across rounds", round.SessionID)
		}
		reviewThreads[round.SessionID] = true
	}
	repairSessions := sessionByRole(saved, "repair")
	if len(repairSessions) != 1 || saved.RepairSession == nil || repairSessions[0].ID != *saved.RepairSession {
		t.Fatalf("repair session was not persistent: %+v", repairSessions)
	}
	executors := sessionByRole(saved, "executor")
	if len(executors) != 1 || executors[0].Status != model.SessionCompleted {
		t.Fatalf("executor session missing or not completed: %+v", executors)
	}
	if len(saved.Verification) != 2 {
		t.Fatalf("expected 2 verification commands, got %+v", saved.Verification)
	}
	for _, v := range saved.Verification {
		if !v.Success || v.Revision != *saved.OutputCommit {
			t.Fatalf("verification evidence not bound to output commit: %+v", v)
		}
	}
	if saved.Reviews[2].Revision != *saved.OutputCommit {
		t.Fatalf("clean review revision %s != output checkpoint %s", saved.Reviews[2].Revision, *saved.OutputCommit)
	}
	if saved.Error != nil || saved.BlockedReason != nil {
		t.Fatalf("published task retained failure evidence: %+v", saved)
	}
	remote := remoteHead(t, fixture, saved.Branch)
	if remote != *saved.OutputCommit {
		t.Fatalf("remote branch = %s, want output %s", remote, *saved.OutputCommit)
	}
	prs := prsJSON(t, fixture)
	if len(prs) != 1 {
		t.Fatalf("expected exactly one PR, got %d", len(prs))
	}
	pr := prs[0]
	head, _ := pr["head"].(map[string]any)
	body, _ := pr["body"].(string)
	if head["ref"] != saved.Branch || pr["state"] != "open" || !strings.Contains(body, "<!-- octomus:task:"+saved.ID+" -->") {
		t.Fatalf("publication PR identity wrong: %+v", pr)
	}
	used, err := fixture.state.SessionsToday()
	if err != nil || used != 6 {
		t.Fatalf("admissions = %d, want 6 (executor + 3 reviewers + 2 repairs)", used)
	}
}

func verificationFixture(t *testing.T, commands []string) (*App, model.Task, string) {
	t.Helper()
	fixture := newExecutionFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.VerificationCommands = commands
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	ws := filepath.Join(fixture.root, "verify-workspace")
	git(t, fixture.root, "init", "--initial-branch=main", ws)
	git(t, ws, "config", "user.name", "Fixture")
	git(t, ws, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(ws, "impl.txt"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, ws, "add", "impl.txt")
	git(t, ws, "commit", "-m", "Fixture")
	revision := git(t, ws, "rev-parse", "HEAD")
	task := executionTask(t, fixture, cfg.DefaultBranch)
	task.Status = model.StatusReviewing
	task.Workspace = ws
	task.ComparisonBase = revision
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	return app, task, revision
}

func TestVerificationMutationIsFailedEvidenceAndStopsRun(t *testing.T) {
	t.Parallel()
	for _, commands := range [][]string{
		{"printf 1 > impl.txt", "test \"$(cat impl.txt)\" = 1", "git checkout -- impl.txt"},
		{"git -c user.name=x -c user.email=x@example.com commit --allow-empty -m moved", "true"},
	} {
		app, task, revision := verificationFixture(t, commands)
		_, err := app.verifyRevision(context.Background(), &task, revision)
		if err == nil || model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
			t.Fatalf("commands %v: err = %v, want workspace_invalid", commands, err)
		}
		saved, getErr := store.Get[model.Task](app.Store, "task", task.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if len(saved.Verification) != 1 || saved.Verification[0].Success {
			t.Fatalf("commands %v: verification = %+v, want one failed record", commands, saved.Verification)
		}
		if record := saved.Verification[0]; record.Command != commands[0] || !strings.Contains(record.Output, "changed during this verification command") {
			t.Fatalf("commands %v: record = %+v; want the mutating command with mutation evidence", commands, record)
		}
	}
}

func TestVerificationCancelledMidCommandReturnsTheCancellationSentinel(t *testing.T) {
	t.Parallel()
	started := filepath.Join(t.TempDir(), "started")
	app, task, revision := verificationFixture(t, []string{"touch '" + started + "' && sleep 30", "true"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := app.verifyRevision(ctx, &task, revision)
		done <- err
	}()
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}) {
		t.Fatal("verification command did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, process.ErrCancelled) {
			t.Fatalf("cancelled verification = %v; want process.ErrCancelled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("verification did not stop after cancellation")
	}
	if saved := loadTask(t, app.Store, task.ID); len(saved.Verification) != 0 {
		t.Fatalf("cancelled verification recorded evidence: %+v", saved.Verification)
	}
}

func TestVerificationRecordsCommandThatBreaksTheStateCheck(t *testing.T) {
	t.Parallel()
	breaking := "rm -rf .git && printf 'gitdir: /nonexistent\\n' > .git"
	app, task, revision := verificationFixture(t, []string{breaking, "true"})
	_, err := app.verifyRevision(context.Background(), &task, revision)
	if err == nil || !strings.Contains(err.Error(), "Workspace state check failed during verification") {
		t.Fatalf("err = %v; want the state check failure", err)
	}
	saved, getErr := store.Get[model.Task](app.Store, "task", task.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if len(saved.Verification) != 1 {
		t.Fatalf("verification = %+v; want one record and no further commands", saved.Verification)
	}
	record := saved.Verification[0]
	if record.Command != breaking || record.Success || record.Revision != revision || !strings.Contains(record.Output, "not a git repository") {
		t.Fatalf("state check failure evidence = %+v", record)
	}
}

func TestVerificationRecordsStreamsAndMutationEvidence(t *testing.T) {
	t.Parallel()
	app, task, revision := verificationFixture(t, []string{"printf 1 > impl.txt", "true"})
	_, err := app.verifyRevision(context.Background(), &task, revision)
	if err == nil {
		t.Fatal("mutation must fail verification")
	}
	saved, _ := store.Get[model.Task](app.Store, "task", task.ID)
	if !strings.Contains(saved.Verification[0].Output, "changed during this verification command") {
		t.Fatalf("mutation evidence missing: %q", saved.Verification[0].Output)
	}

	app, task, revision = verificationFixture(t, []string{"echo out; echo warn >&2", "true"})
	failures, err := app.verifyRevision(context.Background(), &task, revision)
	if err != nil || len(failures) != 0 {
		t.Fatalf("intact verification failed: %v %v", failures, err)
	}
	saved, _ = store.Get[model.Task](app.Store, "task", task.ID)
	if len(saved.Verification) != 2 {
		t.Fatalf("verification = %+v, want two records", saved.Verification)
	}
	for _, v := range saved.Verification {
		if !v.Success || v.Revision != revision {
			t.Fatalf("verification record not bound to the reviewed revision: %+v", v)
		}
	}
	if saved.Verification[0].Output != "out\n[stderr]\nwarn" {
		t.Fatalf("verification lost an output stream: %q", saved.Verification[0].Output)
	}
}

func TestVerificationEvidenceKeepsStderrAndMarksTruncation(t *testing.T) {
	t.Parallel()
	long := "head -c 20000 /dev/zero | tr '\\0' a; echo; echo TAIL-OF'-STDOUT'; "
	for _, test := range []struct {
		name    string
		command string
		success bool
		want    []string
		absent  []string
		suffix  string
	}{
		{name: "long failure", command: long + "echo FAIL'URE-DETAIL' >&2; exit 1", want: []string{"TAIL-OF-STDOUT", "\n[stderr]\nFAILURE-DETAIL", outputTruncatedMarker}, suffix: "\nexit status: 1"},
		{name: "long success", command: long + "echo PASS'-WARNING' >&2", success: true, want: []string{"TAIL-OF-STDOUT", "\n[stderr]\nPASS-WARNING", outputTruncatedMarker}},
		{name: "long stderr", command: "{ head -c 20000 /dev/zero | tr '\\0' e; echo; echo STDERR'-TAIL'; } >&2; echo STDOUT'-KEPT'; exit 3", want: []string{"STDOUT-KEPT\n[stderr]\n" + outputTruncatedMarker, "STDERR-TAIL"}, suffix: "\nexit status: 3"},
		{name: "stderr within the room stdout leaves", command: "{ echo STDERR'-HEAD'; head -c 12000 /dev/zero | tr '\\0' e; echo; echo STDERR'-TAIL'; } >&2; echo out; exit 1", want: []string{"out\n[stderr]\nSTDERR-HEAD", "STDERR-TAIL"}, absent: []string{outputTruncatedMarker}, suffix: "\nexit status: 1"},
		{name: "short failure", command: "echo out; echo err >&2; exit 3", want: []string{"out\n[stderr]\nerr\nexit status: 3"}},
		{name: "secret", command: "echo token=ghp_abcdefghij0123456789; exit 2", want: []string{"token=[redacted]"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, task, revision := verificationFixture(t, []string{test.command})
			failures, err := app.verifyRevision(context.Background(), &task, revision)
			if err != nil {
				t.Fatal(err)
			}
			saved := loadTask(t, app.Store, task.ID)
			if len(saved.Verification) != 1 {
				t.Fatalf("verification = %+v; want one record", saved.Verification)
			}
			record := saved.Verification[0]
			if record.Success != test.success || len(record.Output) > verificationOutputLimit || !utf8.ValidString(record.Output) {
				t.Fatalf("record success=%t, %d bytes; want success=%t within %d bytes", record.Success, len(record.Output), test.success, verificationOutputLimit)
			}
			for _, want := range test.want {
				if !strings.Contains(record.Output, want) {
					t.Fatalf("evidence lacks %q:\n%s", want, record.Output)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(record.Output, absent) {
					t.Fatalf("evidence has %q:\n%s", absent, record.Output)
				}
			}
			if !strings.HasSuffix(record.Output, test.suffix) || strings.Contains(record.Output, "ghp_") {
				t.Fatalf("evidence does not end with %q or kept a secret:\n%s", test.suffix, record.Output)
			}
			if test.success != (len(failures) == 0) || !test.success && failures[0] != test.command+": "+record.Output {
				t.Fatalf("repair prompt failures = %q; want the saved evidence", failures)
			}
		})
	}

	mutating := "head -c 20000 /dev/zero | tr '\\0' a; printf 1 > impl.txt"
	app, task, revision := verificationFixture(t, []string{mutating})
	if _, err := app.verifyRevision(context.Background(), &task, revision); model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
		t.Fatalf("mutation err = %v; want workspace_invalid", err)
	}
	record := loadTask(t, app.Store, task.ID).Verification[0]
	if len(record.Output) > verificationOutputLimit || !strings.HasPrefix(record.Output, outputTruncatedMarker+"\n") || !strings.HasSuffix(record.Output, "\nWorkspace or HEAD changed during this verification command") {
		t.Fatalf("long mutation evidence (%d bytes) lost its marker or note:\n%s", len(record.Output), record.Output)
	}
}

func TestVerificationEvidenceKeepsTheRealEndOfLongOutput(t *testing.T) {
	t.Parallel()
	const (
		progress = "seq -f 'progress line %g of the long verification run' 12000; "
		long     = progress + "echo 'FINAL FAILURE LINE'"
		end      = "\nprogress line 12000 of the long verification run\nFINAL FAILURE LINE\nexit status: 1"
	)
	for _, test := range []struct {
		name    string
		command string
		prefix  string
		suffix  string
	}{
		{name: "stdout", command: long + "; exit 1", prefix: outputTruncatedMarker + "\n", suffix: end},
		{name: "stderr", command: "{ " + long + "; } >&2; echo out; exit 1", prefix: "out\n[stderr]\n" + outputTruncatedMarker + "\n", suffix: end},
		{name: "end inside a long line", command: progress + "head -c 70000 /dev/zero | tr '\\0' A; echo; echo 'FINAL FAILURE LINE'; exit 1", prefix: outputTruncatedMarker + "\n",
			suffix: " of the long verification run\n" + outputTruncatedMarker + "\nFINAL FAILURE LINE\nexit status: 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, task, revision := verificationFixture(t, []string{test.command})
			failures, err := app.verifyRevision(context.Background(), &task, revision)
			if err != nil {
				t.Fatal(err)
			}
			saved := loadTask(t, app.Store, task.ID)
			if len(saved.Verification) != 1 {
				t.Fatalf("verification = %+v; want one record", saved.Verification)
			}
			output := saved.Verification[0].Output
			if !strings.HasPrefix(output, test.prefix) || !strings.HasSuffix(output, test.suffix) || strings.Contains(output, "AAAA") {
				t.Fatalf("evidence lost its marker or the command's real end: %.80q ... %q", output, output[max(len(output)-120, 0):])
			}
			if len(output) > verificationOutputLimit || !utf8.ValidString(output) {
				t.Fatalf("evidence has %d bytes; want valid UTF-8 within %d", len(output), verificationOutputLimit)
			}
			if len(failures) != 1 || failures[0] != test.command+": "+output {
				t.Fatalf("repair prompt failures = %q; want the saved evidence", failures)
			}
			prompt, err := repairPrompt(&saved, saved.ExecutionConfig(), model.Review{}, failures)
			if err != nil || !strings.Contains(prompt, "FINAL FAILURE LINE") {
				t.Fatalf("repair prompt lacks the command's real end (%v)", err)
			}
		})
	}
}

func TestVerificationEvidenceNeverShowsASecretTheCaptureLimitCut(t *testing.T) {
	t.Parallel()
	const (
		credential = "'https://bot:s3cr3tpassword0123@github.com/x'"
		leak       = "s3cr3tpass"
	)
	cutLine := func(before int) string {
		return fmt.Sprintf("head -c %d /dev/zero | tr '\\0' A; printf '%%s\\n' %s", process.DiagnosticLimit-before-22, credential)
	}
	keptLine := "echo KEPT-LINE; " + cutLine(len("KEPT-LINE\n"))
	for _, test := range []struct {
		name    string
		command string
		want    string
	}{
		{name: "stdout", command: keptLine + "; exit 1", want: "KEPT-LINE\n" + outputTruncatedMarker + "\nexit status: 1"},
		{name: "stderr", command: "{ " + keptLine + "; } >&2; echo out; exit 1", want: "out\n[stderr]\nKEPT-LINE\n" + outputTruncatedMarker + "\nexit status: 1"},
		{name: "one unbroken line", command: cutLine(0) + "; exit 1", want: "\n" + outputTruncatedMarker + "\nexit status: 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, task, revision := verificationFixture(t, []string{test.command})
			failures, err := app.verifyRevision(context.Background(), &task, revision)
			if err != nil {
				t.Fatal(err)
			}
			saved := loadTask(t, app.Store, task.ID)
			if len(saved.Verification) != 1 {
				t.Fatalf("verification = %+v; want one record", saved.Verification)
			}
			output := saved.Verification[0].Output
			if strings.Contains(output, leak) || output != test.want {
				t.Fatalf("evidence = ...%q; want %q without the cut credential", output[max(len(output)-80, 0):], test.want)
			}
			if len(failures) != 1 || failures[0] != test.command+": "+output {
				t.Fatalf("repair prompt failures = %q; want the saved evidence", failures)
			}
		})
	}
}

func TestBoundedTailKeepsTheEndWithinTheLimit(t *testing.T) {
	t.Parallel()
	marker := outputTruncatedMarker
	for _, test := range []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{name: "fits exactly", text: strings.Repeat("x", 32), limit: 32, want: strings.Repeat("x", 32)},
		{name: "over the limit", text: "head-" + strings.Repeat("x", 40) + "-tail", limit: 32, want: marker + "\n" + strings.Repeat("x", 8) + "-tail"},
		{name: "rune boundary", text: strings.Repeat("é", 20), limit: 24, want: marker + "\n" + strings.Repeat("é", 2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := boundedTail(test.text, test.limit)
			if got != test.want || len(got) > test.limit || !utf8.ValidString(got) {
				t.Fatalf("boundedTail = %q (%d bytes); want %q within %d", got, len(got), test.want, test.limit)
			}
		})
	}
}

func TestVerificationArtifactMustBeGitIgnored(t *testing.T) {
	t.Parallel()
	commands := []string{"printf report > coverage.out", "true"}
	app, task, revision := verificationFixture(t, commands)
	_, err := app.verifyRevision(context.Background(), &task, revision)
	if err == nil || model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
		t.Fatalf("untracked artifact err = %v; want workspace_invalid", err)
	}
	saved := loadTask(t, app.Store, task.ID)
	if len(saved.Verification) != 1 || saved.Verification[0].Success ||
		!strings.Contains(saved.Verification[0].Output, "changed during this verification command") {
		t.Fatalf("untracked artifact evidence = %+v; want one failed mutation record", saved.Verification)
	}

	app, task, revision = verificationFixture(t, commands)
	exclude := filepath.Join(task.Workspace, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte("coverage.out\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failures, err := app.verifyRevision(context.Background(), &task, revision)
	if err != nil || len(failures) != 0 {
		t.Fatalf("ignored artifact failed verification: %v, %v", failures, err)
	}
	saved = loadTask(t, app.Store, task.ID)
	if len(saved.Verification) != 2 || !saved.Verification[0].Success || !saved.Verification[1].Success {
		t.Fatalf("ignored artifact evidence = %+v; want two passing records", saved.Verification)
	}
}

func writeFixtureMode(t *testing.T, fixture *planningFixture, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.root, name), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func publications(t *testing.T, fixture *planningFixture) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.root, "publications.jsonl"))
	if err != nil {
		return nil
	}
	entries := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func prsJSON(t *testing.T, fixture *planningFixture) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.root, "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prs []map[string]any
	if err := json.Unmarshal(data, &prs); err != nil {
		t.Fatal(err)
	}
	return prs
}

func newExecutionApp(t *testing.T, fixture *planningFixture) *App {
	t.Helper()
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestExecutionRemoteConflictBlocksStaleBase(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	writeFixtureMode(t, fixture, "remote-conflict")
	if err := os.WriteFile(filepath.Join(fixture.root, "target"), []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonStaleBase {
		t.Fatalf("remote conflict outcome = %+v", saved)
	}
	if saved.OutputCommit != nil || len(publications(t, fixture)) != 0 {
		t.Fatalf("stale context authorized publication: %+v", saved)
	}
	external, err := os.ReadFile(filepath.Join(fixture.root, "external-revision"))
	if err != nil {
		t.Fatal(err)
	}
	if remote := remoteHead(t, fixture, "main"); remote != strings.TrimSpace(string(external)) {
		t.Fatalf("remote main = %s; want external %s", remote, external)
	}
}

func heldUploadPack(t *testing.T, fixture *planningFixture) {
	t.Helper()
	script := "#!/bin/sh\nfixture_dir=$(dirname \"$0\")\ntouch \"$fixture_dir/entered-$$\"\nwhile [ -e \"$fixture_dir/hold\" ]; do sleep 0.02; done\nif [ -e \"$fixture_dir/fail\" ]; then exit 1; fi\nexec git-upload-pack \"$@\"\n"
	path := filepath.Join(fixture.root, "held-upload-pack")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, fixture.repo, "config", "remote.origin.uploadpack", path)
	writeFixtureMode(t, fixture, "hold")
}

func enteredPreflights(t *testing.T, fixture *planningFixture) int {
	t.Helper()
	entries, err := os.ReadDir(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "entered-") {
			count++
		}
	}
	return count
}

func waitForPreflights(t *testing.T, fixture *planningFixture, count int) {
	t.Helper()
	if !testutil.WaitUntil(30*time.Second, func() bool { return enteredPreflights(t, fixture) >= count }) {
		t.Fatalf("Git preflight did not reach the controlled remote")
	}
}

func releasePreflight(t *testing.T, fixture *planningFixture) {
	t.Helper()
	if err := os.Remove(filepath.Join(fixture.root, "hold")); err != nil {
		t.Fatal(err)
	}
}

func checkpointedTask(t *testing.T, fixture *planningFixture, target string) model.Task {
	t.Helper()
	cfg := fixture.cfg.Clone()
	cfg.VerificationCommands = []string{"test -f feature.txt"}
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	task := executionTask(t, fixture, target)
	ctx := context.Background()
	ws := filepath.Join(fixture.dataDir, "tasks", task.ID, "workspace")
	if err := gitops.CloneAt(ctx, cfg, ws, task.SourceRevision); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, err := gitops.Snapshot(ctx, cfg, ws, task.Proposal.Title)
	if err != nil {
		t.Fatal(err)
	}
	task.Workspace = ws
	task.ComparisonBase = task.SourceRevision
	thread := "exec-" + task.ID
	task.ExecutionSession = &thread
	task.Sessions = []model.Session{{ID: thread, Role: "executor", Status: model.SessionCompleted, Summary: "Implemented the feature"}}
	task.Reviews = []model.ReviewRound{{SessionID: "r1", Revision: commit, ComparisonBase: task.SourceRevision, Result: model.Review{Completed: true, Summary: "clean", Findings: []model.Finding{}}, CreatedAt: model.Now()}}
	task.Verification = []model.Verification{{Command: "test -f feature.txt", Success: true, Revision: commit, CreatedAt: model.Now()}}
	task.OutputCommit = &commit
	return task
}

func seedFixturePR(t *testing.T, fixture *planningFixture, task model.Task, state string) {
	t.Helper()
	git(t, task.Workspace, "push", filepath.Join(fixture.root, "remote.git"), *task.OutputCommit+":refs/heads/"+task.Branch)
	pr := []map[string]any{{
		"number": 1, "title": "x", "body": "<!-- octomus:task:" + task.ID + " -->",
		"head":     map[string]any{"ref": task.Branch, "sha": "", "repo": map[string]any{"full_name": "fixture/project"}},
		"base":     map[string]any{"ref": "main"},
		"html_url": "https://github.com/fixture/project/pull/1", "state": state,
		"merged_at": nil, "additions": 1, "deletions": 0, "created_at": "2026-09-07T00:00:00Z",
	}}
	data, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionRestartReconcilesPublicationCheckpoint(t *testing.T) {
	t.Parallel()
	t.Run("push landed without PR", func(t *testing.T) {
		fixture := newExecutionFixture(t)
		task := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
		task.Status = model.StatusPublishing
		saveExecutionTask(t, fixture, task)
		app := New(fixture.state, fixture.dataDir)
		t.Cleanup(app.Shutdown)
		if err := app.Recover(); err != nil {
			t.Fatal(err)
		}
		queued, err := store.Get[model.Task](fixture.state, "task", task.ID)
		if err != nil || queued == nil || queued.Status != model.StatusQueued {
			t.Fatalf("checkpoint recovery = %+v, %v", queued, err)
		}
		if err := app.Resume(); err != nil {
			t.Fatal(err)
		}
		saved := driveTask(t, fixture, app, task.ID)
		if saved.Status != model.StatusPublished || saved.PRNumber == nil {
			t.Fatalf("recovered publication = %+v", saved)
		}
		if entries := publications(t, fixture); len(entries) != 1 || entries[0]["action"] != "create" {
			t.Fatalf("publications = %+v; want exactly one create", entries)
		}
		if remote := remoteHead(t, fixture, saved.Branch); remote != *saved.OutputCommit {
			t.Fatalf("remote = %s; want output %s", remote, *saved.OutputCommit)
		}
	})
	t.Run("PR created but acknowledgement lost", func(t *testing.T) {
		fixture := newExecutionFixture(t)
		task := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
		task.Status = model.StatusPublishing
		seedFixturePR(t, fixture, task, "open")
		saveExecutionTask(t, fixture, task)
		app := New(fixture.state, fixture.dataDir)
		t.Cleanup(app.Shutdown)
		if err := app.Recover(); err != nil {
			t.Fatal(err)
		}
		if err := app.Resume(); err != nil {
			t.Fatal(err)
		}
		saved := driveTask(t, fixture, app, task.ID)
		if saved.Status != model.StatusPublished || saved.PRNumber == nil || *saved.PRNumber != 1 {
			t.Fatalf("reconciled publication = %+v", saved)
		}
		if entries := publications(t, fixture); len(entries) != 0 {
			t.Fatalf("republication wrote actions: %+v", entries)
		}
	})
	t.Run("PR closed after checkpoint", func(t *testing.T) {
		fixture := newExecutionFixture(t)
		task := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
		task.Status = model.StatusPublishing
		seedFixturePR(t, fixture, task, "closed")
		saveExecutionTask(t, fixture, task)
		app := New(fixture.state, fixture.dataDir)
		t.Cleanup(app.Shutdown)
		if err := app.Recover(); err != nil {
			t.Fatal(err)
		}
		if err := app.Resume(); err != nil {
			t.Fatal(err)
		}
		saved := driveTask(t, fixture, app, task.ID)
		if saved.Status != model.StatusPublished || saved.PRNumber == nil || *saved.PRNumber != 1 {
			t.Fatalf("closed-PR reconcile = %+v", saved)
		}
		if entries := publications(t, fixture); len(entries) != 0 {
			t.Fatalf("closed-PR reconcile wrote actions: %+v", entries)
		}
	})
}

func TestExecutionShutdownDuringPublicationRequeuesCheckpoint(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	task := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusExecuting
	saveExecutionTask(t, fixture, task)
	heldUploadPack(t, fixture)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	app.runTask(task)
	waitForPreflights(t, fixture, 1)

	app.Shutdown()
	stopped := loadTask(t, fixture.state, task.ID)
	if stopped.Status != model.StatusPublishing || stopped.OutputCommit == nil || *stopped.OutputCommit != *task.OutputCommit ||
		stopped.Error != nil || stopped.BlockedReason != nil {
		t.Fatalf("graceful stop recorded a publication outcome: %+v", stopped)
	}
	if entries := publications(t, fixture); len(entries) != 0 {
		t.Fatalf("held publication wrote actions: %+v", entries)
	}
	releasePreflight(t, fixture)

	restarted := New(fixture.state, fixture.dataDir)
	t.Cleanup(restarted.Shutdown)
	deferHousekeeping(restarted)
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	if recovered := loadTask(t, fixture.state, task.ID); recovered.Status != model.StatusQueued || recovered.Attempts != 1 {
		t.Fatalf("interrupted publication recovery = %+v", recovered)
	}
	if err := restarted.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture, restarted, task.ID)
	if saved.Status != model.StatusPublished || saved.PRNumber == nil || *saved.OutputCommit != *task.OutputCommit {
		t.Fatalf("recovered publication = %+v", saved)
	}
	if entries := publications(t, fixture); len(entries) != 1 || entries[0]["action"] != "create" {
		t.Fatalf("publications = %+v; want exactly one create", entries)
	}
	if len(saved.Sessions) != 1 {
		t.Fatalf("recovered publication ran another model turn: %+v", saved.Sessions)
	}
	if remote := remoteHead(t, fixture, saved.Branch); remote != *saved.OutputCommit {
		t.Fatalf("remote = %s; want output %s", remote, *saved.OutputCommit)
	}
}

func existingPrBranch(t *testing.T, fixture *planningFixture) string {
	t.Helper()
	work := filepath.Join(fixture.root, "existing-work")
	git(t, fixture.root, "clone", fixture.repo, work)
	git(t, work, "config", "user.name", "Fixture")
	git(t, work, "config", "user.email", "fixture@example.com")
	git(t, work, "checkout", "-b", "octomus/existing")
	if err := os.WriteFile(filepath.Join(work, "earlier.txt"), []byte("Preserve the earlier improvement.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "Earlier Octomus work")
	git(t, work, "push", filepath.Join(fixture.root, "remote.git"), "octomus/existing")
	head := git(t, work, "rev-parse", "HEAD")
	pr := []map[string]any{{
		"number": 42, "title": "An existing improvement", "body": "Existing context.\n<!-- octomus:task:earlier -->",
		"head":     map[string]any{"ref": "octomus/existing", "sha": head, "repo": map[string]any{"full_name": "fixture/project"}},
		"base":     map[string]any{"ref": "main"},
		"html_url": "https://github.com/fixture/project/pull/42", "state": "open",
		"merged_at": nil, "additions": 2000, "deletions": 0, "created_at": "2026-08-01T00:00:00Z",
	}}
	data, err := json.Marshal(pr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return head
}

func TestExecutionExistingPrAppendsComment(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	head := existingPrBranch(t, fixture)
	task := executionTask(t, fixture, "octomus/existing")
	task.Branch = "octomus/existing"
	task.SourceRevision = head
	number := uint64(42)
	task.PRNumber = &number
	url := "https://github.com/fixture/project/pull/42"
	task.PRURL = &url
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusPublished || saved.PRNumber == nil || *saved.PRNumber != 42 {
		t.Fatalf("existing-PR delivery = %+v", saved)
	}
	entries := publications(t, fixture)
	if len(entries) != 1 || entries[0]["action"] != "comment" || entries[0]["number"] != float64(42) {
		t.Fatalf("publications = %+v; want exactly one comment on #42", entries)
	}
	prs := prsJSON(t, fixture)
	if len(prs) != 1 {
		t.Fatalf("a second PR was created: %+v", prs)
	}
	body, _ := prs[0]["body"].(string)
	if !strings.Contains(body, "Existing context.") || !strings.Contains(body, "<!-- octomus:task:earlier -->") {
		t.Fatalf("maintainer description rewritten: %q", body)
	}
	if _, err := os.Stat(filepath.Join(saved.Workspace, "earlier.txt")); err != nil {
		t.Fatalf("earlier work missing from the workspace: %v", err)
	}
	if base := git(t, saved.Workspace, "merge-base", saved.DefaultRevision, head); saved.ComparisonBase != base {
		t.Fatalf("comparison base = %q; want merge base %s", saved.ComparisonBase, base)
	}
	for _, round := range saved.Reviews {
		if round.ComparisonBase != saved.ComparisonBase {
			t.Fatalf("review round lost the comparison base: %+v", round)
		}
	}
	if remote := remoteHead(t, fixture, "octomus/existing"); remote != *saved.OutputCommit {
		t.Fatalf("remote = %s; want output %s", remote, *saved.OutputCommit)
	}
}

func TestExecutionDependenciesOrderAndRollback(t *testing.T) {
	t.Parallel()
	t.Run("orders onto dependency output", func(t *testing.T) {
		fixture := newExecutionFixture(t)
		head := existingPrBranch(t, fixture)
		first := executionTask(t, fixture, "octomus/existing")
		first.Branch = "octomus/existing"
		first.SourceRevision = head
		number := uint64(42)
		first.PRNumber = &number
		url := "https://github.com/fixture/project/pull/42"
		first.PRURL = &url
		second := executionTask(t, fixture, "octomus/existing")
		second.Branch = "octomus/existing"
		second.SourceRevision = head
		second.PRNumber = &number
		second.PRURL = &url
		second.Proposal.ID = "d0-followup"
		second.Proposal.Title = "Complete the next fixture feature"
		second.Proposal.Prompt = "Implement the next fixture capability. fixture-file=feature-next.txt"
		second.Proposal.Dependencies = []string{first.ID}
		saveExecutionTask(t, fixture, first)
		saveExecutionTask(t, fixture, second)
		app := newExecutionApp(t, fixture)
		delivered := driveTask(t, fixture, app, first.ID)
		if delivered.Status != model.StatusPublished {
			t.Fatalf("dependency delivery = %+v", delivered)
		}
		saved := driveTask(t, fixture, app, second.ID)
		if saved.Status != model.StatusPublished {
			t.Fatalf("dependent delivery = %+v", saved)
		}
		if saved.SourceRevision != *delivered.OutputCommit {
			t.Fatalf("dependent source = %s; want dependency output %s", saved.SourceRevision, *delivered.OutputCommit)
		}
		if _, err := os.Stat(filepath.Join(saved.Workspace, "feature.txt")); err != nil {
			t.Fatalf("dependent workspace lost the dependency output: %v", err)
		}
		entries := publications(t, fixture)
		if len(entries) != 2 || entries[0]["action"] != "comment" || entries[1]["action"] != "comment" {
			t.Fatalf("publications = %+v; want two follow-up comments", entries)
		}
	})
	t.Run("rewound dependency output blocks", func(t *testing.T) {
		fixture := newExecutionFixture(t)
		head := existingPrBranch(t, fixture)
		first := executionTask(t, fixture, "octomus/existing")
		first.Branch = "octomus/existing"
		first.SourceRevision = head
		number := uint64(42)
		first.PRNumber = &number
		url := "https://github.com/fixture/project/pull/42"
		first.PRURL = &url
		second := executionTask(t, fixture, "octomus/existing")
		second.Branch = "octomus/existing"
		second.SourceRevision = head
		second.PRNumber = &number
		second.PRURL = &url
		second.Proposal.ID = "d0-followup"
		second.Proposal.Dependencies = []string{first.ID}
		saveExecutionTask(t, fixture, first)
		saveExecutionTask(t, fixture, second)
		app := newExecutionApp(t, fixture)
		delivered := driveTask(t, fixture, app, first.ID)
		if delivered.Status != model.StatusPublished {
			t.Fatalf("dependency delivery = %+v", delivered)
		}
		git(t, fixture.repo, "fetch", filepath.Join(fixture.root, "remote.git"), "octomus/existing")
		git(t, fixture.root, "--git-dir", filepath.Join(fixture.root, "remote.git"), "update-ref", "refs/heads/octomus/existing", head)
		saved := driveTask(t, fixture, app, second.ID)
		if !blockedAs(saved, model.BlockedReasonDependencyBlocked) {
			t.Fatalf("rollback dependent outcome = %+v; want dependency_blocked", saved)
		}
		if saved.OutputCommit != nil {
			t.Fatalf("rollback authorized publication: %+v", saved)
		}
	})
}

func TestExecutionWorkerPanicBlocks(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir, WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
		panic("worker exploded")
	})))
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil || !strings.Contains(*saved.Error, "panicked") {
		t.Fatalf("panic outcome = %+v", saved)
	}
}

func TestRunJoinedReturnsTheCallbacksOwnResult(t *testing.T) {
	t.Parallel()
	for _, want := range []error{nil, errors.New("late failure")} {
		ctx, cancel := context.WithCancel(context.Background())
		result, err := runJoined(ctx, cancel, 100*time.Millisecond, "Callback panicked", func() error {
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			return want
		})
		cancel()
		if !result.Expired || result.AlreadyCancelled {
			t.Fatalf("deadline result = %+v; want a genuine expiry", result)
		}
		if err != want {
			t.Fatalf("late callback result = %v; want %v", err, want)
		}
	}
	result, err := runJoined(context.Background(), func() {}, time.Minute, "Callback panicked", func() error {
		panic("boom")
	})
	if result.Expired || err == nil || err.Error() != "Callback panicked: boom" {
		t.Fatalf("panicking callback = %+v, %v", result, err)
	}
}

func TestSupervisionNeverDemotesRecordedPublication(t *testing.T) {
	t.Parallel()
	supervise := func(t *testing.T, execute func(*App) func(context.Context, *model.Task) error) (*App, model.Task, error) {
		t.Helper()
		state := testStore(t)
		cfg := testConfig(t.TempDir())
		task := queuedTask(cfg, model.ID(), cfg.DefaultBranch, "octomus/delivered")
		task.Status = model.StatusPublishing
		task.Config.TaskTimeoutSeconds = 1
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
		app := New(state, t.TempDir())
		t.Cleanup(app.Shutdown)
		err := app.superviseExecution(context.Background(), task, execute(app))
		return app, loadTask(t, state, task.ID), err
	}
	errorEvents := func(t *testing.T, app *App, id string) []string {
		t.Helper()
		events, err := app.Store.Events(&id)
		if err != nil {
			t.Fatal(err)
		}
		messages := []string{}
		for _, event := range events {
			if event.Kind == "error" {
				messages = append(messages, event.Message)
			}
		}
		return messages
	}

	t.Run("published after the deadline fired", func(t *testing.T) {
		app, saved, err := supervise(t, func(app *App) func(context.Context, *model.Task) error {
			return func(_ context.Context, task *model.Task) error {
				time.Sleep(1500 * time.Millisecond)
				return app.transition(task, model.StatusPublished)
			}
		})
		if err != nil {
			t.Fatalf("a recorded delivery was reported as a failure: %v", err)
		}
		if saved.Status != model.StatusPublished || saved.BlockedReason != nil || saved.Error != nil {
			t.Fatalf("late deadline rewrote the delivery: %+v", saved)
		}
		if messages := errorEvents(t, app, saved.ID); len(messages) != 0 {
			t.Fatalf("late deadline recorded errors: %v", messages)
		}
	})

	t.Run("bookkeeping failed after the published write", func(t *testing.T) {
		app, saved, err := supervise(t, func(app *App) func(context.Context, *model.Task) error {
			return func(_ context.Context, task *model.Task) error {
				if err := app.transition(task, model.StatusPublished); err != nil {
					return err
				}
				return errors.New("PR observation write failed")
			}
		})
		if err == nil || !strings.Contains(err.Error(), "PR observation write failed") {
			t.Fatalf("bookkeeping failure was not returned: %v", err)
		}
		if saved.Status != model.StatusPublished || saved.BlockedReason != nil || saved.Error != nil {
			t.Fatalf("bookkeeping failure demoted the delivery: %+v", saved)
		}
		if messages := errorEvents(t, app, saved.ID); len(messages) != 1 || !strings.Contains(messages[0], "PR observation write failed") {
			t.Fatalf("bookkeeping failure evidence = %v", messages)
		}
	})
}

func TestSupervisionReportsOperatorCancelOverLateDeadline(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	task := queuedTask(cfg, model.ID(), cfg.DefaultBranch, "octomus/cancelled")
	task.Status = model.StatusExecuting
	task.Sessions = []model.Session{{ID: "executor-thread", Role: "executor", Status: model.SessionRunning}}
	task.Config.TaskTimeoutSeconds = 1
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkCancel(task.ID); err != nil {
		t.Fatal(err)
	}
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	err := app.superviseExecution(cancelled, task, func(ctx context.Context, _ *model.Task) error {
		time.Sleep(3 * time.Second)
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := loadTask(t, state, task.ID)
	if saved.Status != model.StatusCancelled || saved.BlockedReason != nil || saved.Error == nil || *saved.Error != "Cancelled by the operator" {
		t.Fatalf("cancelled task = status %s, reason %v, error %q", saved.Status, saved.BlockedReason, optionalText(saved.Error))
	}
	if len(saved.Sessions) != 1 || saved.Sessions[0].Status != model.SessionFailed || saved.Sessions[0].Summary != "Cancelled by the operator" {
		t.Fatalf("cancelled session = %+v", saved.Sessions)
	}
	if !hasEvent(t, state, task.ID, "error", "Task time limit exceeded") {
		t.Fatal("the late deadline was not recorded as an error event")
	}
}

func TestExecutionDeliversFullLifecycleViaOpenCode(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	opencode := filepath.Join(fixture.root, "opencode")
	pythonFixtureShim(t, opencode, fixture.root, "opencode.py")
	cfg := fixture.cfg.Clone()
	cfg.OpencodeBinary = opencode
	cfg.CodexBinary = "/codex-is-not-installed"
	provider := "fixture"
	variant := "high"
	executor := config.Route{Backend: config.BackendOpencode, Provider: &provider, Model: "fixture-model", Variant: &variant}
	planning := config.Route{Backend: config.BackendOpencode, Provider: &provider, Model: "plain-model"}
	alternate := "alternate"
	repair := config.Route{Backend: config.BackendOpencode, Provider: &alternate, Model: "fixture-model"}
	for name := range cfg.Tiers {
		cfg.Tiers[name] = executor
	}
	for name := range cfg.Roles {
		cfg.Roles[name] = executor
	}
	cfg.Roles["discovery"] = planning
	cfg.Roles["orchestrator"] = planning
	cfg.Roles["proposal_reviewer"] = planning
	cfg.RepairRoute = repair
	cfg.VerificationCommands = []string{`test "$(cat feature.txt)" = fixed`}
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	task := executionTask(t, fixture, cfg.DefaultBranch)
	task.Route = executor
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("opencode task did not publish: %+v", saved)
	}
	if saved.OutputCommit == nil || remoteHead(t, fixture, saved.Branch) != *saved.OutputCommit {
		t.Fatalf("opencode delivery head mismatch: %+v", saved)
	}
	if len(saved.Reviews) != 3 || !saved.Reviews[2].Result.Clean() {
		t.Fatalf("opencode reviews = %+v", saved.Reviews)
	}
	if saved.Reviews[2].Revision != *saved.OutputCommit {
		t.Fatalf("reviewed commit %s != output %s", saved.Reviews[2].Revision, *saved.OutputCommit)
	}
	repairs := sessionByRole(saved, "repair")
	if len(repairs) != 1 || repairs[0].Status != model.SessionCompleted {
		t.Fatalf("opencode repair session = %+v", repairs)
	}
	for _, v := range saved.Verification {
		if !v.Success || v.Revision != *saved.OutputCommit {
			t.Fatalf("opencode verification not bound to output: %+v", v)
		}
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "protocol.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	backendCount := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["backend"] != "opencode" {
			t.Fatalf("unexpected backend in protocol log: %v", entry)
		}
		backendCount++
	}
	if backendCount == 0 {
		t.Fatal("no opencode protocol traffic recorded")
	}
	used, err := fixture.state.SessionsToday()
	if err != nil || used != 6 {
		t.Fatalf("opencode admissions = %d, want 6 (executor + 3 reviewers + 2 repairs)", used)
	}
}
