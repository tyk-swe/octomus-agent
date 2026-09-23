package engine

// Execution and publication lifecycle tests against deterministic local
// fixture peers: real local Git, a Python app-server peer, and a Python GitHub
// peer. No network requests or model calls.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// newExecutionFixture mirrors newPlanningFixture but places the data directory
// at .octomus so the fixture's task-workspace fault injection applies.
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
	out, err := exec.Command("/usr/bin/git", "--git-dir", filepath.Join(fixture.root, "remote.git"), "rev-parse", branch).Output()
	if err != nil {
		t.Fatalf("remote head %s: %v", branch, err)
	}
	return strings.TrimSpace(string(out))
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

// driveTask ticks the scheduler until the durable task reaches a terminal or
// blocked status, then returns the final record.
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
	fixture := newExecutionFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.VerificationCommands = []string{"test -f feature.txt", "grep -q fixed feature.txt"}
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	task := executionTask(t, fixture, cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task did not publish: %+v", saved)
	}
	if saved.OutputCommit == nil || *saved.OutputCommit == saved.SourceRevision {
		t.Fatalf("missing output checkpoint: %+v", saved.OutputCommit)
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
	// The reviewed commit, verified commit, output checkpoint and delivered PR
	// head must all agree.
	if saved.Reviews[2].Revision != *saved.OutputCommit {
		t.Fatalf("clean review revision %s != output checkpoint %s", saved.Reviews[2].Revision, *saved.OutputCommit)
	}
	if saved.Error != nil || saved.BlockedReason != nil {
		t.Fatalf("published task retained failure evidence: %+v", saved)
	}
	// Remote branch holds the output commit and the PR is open with the marker.
	remote := remoteHead(t, fixture, saved.Branch)
	if remote != *saved.OutputCommit {
		t.Fatalf("remote branch = %s, want output %s", remote, *saved.OutputCommit)
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prs []map[string]any
	if err := json.Unmarshal(data, &prs); err != nil {
		t.Fatal(err)
	}
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

func TestExecutionMalformedAndIncompleteReviewsNeverPublish(t *testing.T) {
	for _, mode := range []string{"malformed-review", "incomplete-review"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newExecutionFixture(t)
			if err := os.WriteFile(filepath.Join(fixture.root, mode), []byte("1"), 0o600); err != nil {
				t.Fatal(err)
			}
			task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture, task)
			app := New(fixture.state, fixture.dataDir)
			t.Cleanup(app.Shutdown)
			app.runtime.lastRetention = time.Now()
			app.runtime.lastObserve = time.Now()
			if err := app.Resume(); err != nil {
				t.Fatal(err)
			}
			saved := driveTask(t, fixture, app, task.ID)
			if saved.Status != model.StatusBlocked || saved.BlockedReason == nil {
				t.Fatalf("invalid review did not block: %+v", saved)
			}
			if mode == "malformed-review" && *saved.BlockedReason != model.BlockedReasonInvalidReview && *saved.BlockedReason != model.BlockedReasonRunnerUnavailable {
				t.Fatalf("malformed review blocked as %v", *saved.BlockedReason)
			}
			if mode == "incomplete-review" && *saved.BlockedReason != model.BlockedReasonInvalidReview {
				t.Fatalf("incomplete review blocked as %v", *saved.BlockedReason)
			}
			if saved.OutputCommit != nil || saved.PRNumber != nil {
				t.Fatalf("invalid review authorized publication: %+v", saved)
			}
		})
	}
}

// verificationFixture builds a real workspace at one commit with a tracked
// impl.txt, then runs verifyRevision against it — the F1/F6 review-findings
// regression setup.
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
	command(t, fixture.root, "/usr/bin/git", "init", "--initial-branch=main", ws)
	command(t, ws, "/usr/bin/git", "config", "user.name", "Fixture")
	command(t, ws, "/usr/bin/git", "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(ws, "impl.txt"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command(t, ws, "/usr/bin/git", "add", "impl.txt")
	command(t, ws, "/usr/bin/git", "commit", "-m", "Fixture")
	out, err := exec.Command("/usr/bin/git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.TrimSpace(string(out))
	task := executionTask(t, fixture, cfg.DefaultBranch)
	task.Status = model.StatusReviewing
	task.Workspace = ws
	task.ComparisonBase = revision
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	return app, task, revision
}

// F1: a verification command that mutates tracked state is recorded as failed
// evidence and stops the remaining commands.
func TestVerificationMutationIsFailedEvidenceAndStopsRun(t *testing.T) {
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
	}
}

// F1: worktree mutation evidence names the mutation; F6: successful commands
// keep both output streams.
func TestVerificationRecordsStreamsAndMutationEvidence(t *testing.T) {
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
	if !strings.Contains(saved.Verification[0].Output, "out") || !strings.Contains(saved.Verification[0].Output, "warn") {
		t.Fatalf("verification lost an output stream: %q", saved.Verification[0].Output)
	}
}

func waitForFile(t *testing.T, path string, label string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", label)
}

// writeFixtureMode arms a deterministic fixture-peer mode file.
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
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	return app
}

// TestExecutionFailedVerificationExhaustsRepairBudget: verification evidence
// fails on every clean review; the repair budget, not the reviewer, decides
// the outcome.
func TestExecutionFailedVerificationExhaustsRepairBudget(t *testing.T) {
	fixture := newExecutionFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.VerificationCommands = []string{"false"}
	cfg.MaxRepairRounds = 2
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	task := executionTask(t, fixture, cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonVerificationFailed {
		t.Fatalf("failed verification outcome = %+v", saved)
	}
	if len(saved.Reviews) != 3 || len(saved.Verification) != 1 || saved.Verification[0].Success {
		t.Fatalf("unexpected evidence: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
	}
	if len(sessionByRole(saved, "repair")) != 1 || saved.RepairSession == nil {
		t.Fatalf("repair session not persistent: %+v", saved.Sessions)
	}
	if saved.OutputCommit != nil || len(publications(t, fixture)) != 0 {
		t.Fatalf("failed verification authorized publication: %+v", saved)
	}
}

// TestExecutionRemoteConflictBlocksStaleBase: the target head moved under the
// reviewed work; publication refuses the stale context.
func TestExecutionRemoteConflictBlocksStaleBase(t *testing.T) {
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

// driveUntilFile ticks the scheduler until path appears or the deadline hits.
func driveUntilFile(t *testing.T, fixture *planningFixture, app *App, path, label string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := app.Tick(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s did not appear", label)
}

// heldUploadPack points the fixture checkout's upload-pack at a script that
// records entry and holds while fixture.root/hold exists — the deterministic
// remote-preflight gate under test.
func heldUploadPack(t *testing.T, fixture *planningFixture) {
	t.Helper()
	script := "#!/bin/sh\nfixture_dir=$(dirname \"$0\")\ntouch \"$fixture_dir/entered-$$\"\nwhile [ -e \"$fixture_dir/hold\" ]; do sleep 0.02; done\nif [ -e \"$fixture_dir/fail\" ]; then exit 1; fi\nexec git-upload-pack \"$@\"\n"
	path := filepath.Join(fixture.root, "held-upload-pack")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command(t, fixture.repo, "/usr/bin/git", "config", "remote.origin.uploadpack", path)
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
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if enteredPreflights(t, fixture) >= count {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Git preflight did not reach the controlled remote")
}

func releasePreflight(t *testing.T, fixture *planningFixture) {
	t.Helper()
	if err := os.Remove(filepath.Join(fixture.root, "hold")); err != nil {
		t.Fatal(err)
	}
}

// TestExecutionCancellationDuringTurn: the operator cancel marker reaches a
// running turn; the durable outcome is cancelled, not failed.
func TestExecutionCancellationDuringTurn(t *testing.T) {
	fixture := newExecutionFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.root, "codex-mode"), []byte("hold"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	driveUntilFile(t, fixture, app, filepath.Join(fixture.root, "codex-entered"), "held executor turn")
	if err := app.TaskAction(context.Background(), task.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusCancelled {
		t.Fatalf("cancelled turn outcome = %+v", saved)
	}
	if saved.OutputCommit != nil || len(publications(t, fixture)) != 0 {
		t.Fatalf("cancelled task published: %+v", saved)
	}
	marked, err := fixture.state.MarkerSet("cancel", task.ID)
	if err != nil || !marked {
		t.Fatalf("cancel marker = %v, %v", marked, err)
	}
	if executors := sessionByRole(saved, "executor"); len(executors) != 1 || executors[0].Status == model.SessionCompleted {
		t.Fatalf("cancelled executor session = %+v", executors)
	}
	// The best-effort turn/interrupt itself is covered deterministically by the
	// runner tests; the fixture child may be reaped before it records it here.
}

// TestExecutionTaskTimeout: the task deadline fires while remote work is
// still in flight; the durable outcome is a timed-out block, not a runner
// failure or a cancellation.
func TestExecutionTaskTimeout(t *testing.T) {
	fixture := newExecutionFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.SessionTimeoutSeconds = 10
	cfg.TaskTimeoutSeconds = 10
	// The per-command bound stays generous so the task deadline, not a command
	// timeout, fires first.
	cfg.CommandTimeoutSeconds = 120
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	heldUploadPack(t, fixture)
	task := executionTask(t, fixture, cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	type outcome struct {
		task model.Task
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		task, err := driveTaskResult(fixture, app, task.ID)
		done <- outcome{task, err}
	}()
	waitForPreflights(t, fixture, 1)
	finished := <-done
	releasePreflight(t, fixture)
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	saved := finished.task
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonTimeout {
		t.Fatalf("timeout outcome = %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, "time limit") {
		t.Fatalf("timeout error evidence = %+v", saved.Error)
	}
}

// TestExecutionFailedExecutorStartRetries: a runner start failure preserves the
// reserved admission, blocks without a session, and an operator retry redelivers
// with exactly one new executor admission.
func TestExecutionFailedExecutorStartRetries(t *testing.T) {
	fixture := newExecutionFixture(t)
	writeFixtureMode(t, fixture, "failed-executor-start")
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonRunnerUnavailable {
		t.Fatalf("failed start outcome = %+v", saved)
	}
	if saved.ExecutionSession != nil || saved.Error == nil || !strings.Contains(*saved.Error, "Fixture failed start") {
		t.Fatalf("failed start evidence = %+v", saved)
	}
	if used, err := fixture.state.SessionsToday(); err != nil || used != 1 {
		t.Fatalf("admissions after failed start = %d, %v; want the reserved 1", used, err)
	}
	marker := filepath.Join(fixture.root, "failed-executor-start")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	saved = driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("retried delivery = %+v", saved)
	}
	if used, err := fixture.state.SessionsToday(); err != nil || used != 7 {
		t.Fatalf("admissions = %d, %v; want 7 (1 failed + 6 delivered)", used, err)
	}
	if entries := publications(t, fixture); len(entries) != 1 || entries[0]["action"] != "create" {
		t.Fatalf("publications = %+v; want one create", entries)
	}
}

// checkpointedTask builds the durable publication-checkpoint state: a real
// workspace whose HEAD is the recorded output commit with clean review and
// successful verification evidence.
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

// TestExecutionRestartReconcilesPublicationCheckpoint: a durable publishing
// checkpoint plus an intact workspace is requeued and delivered without
// duplicating remote writes — covering both the lost-acknowledgement and the
// never-created variants.
func TestExecutionRestartReconcilesPublicationCheckpoint(t *testing.T) {
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
		// The checkpoint is requeued rather than restarted from scratch.
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
		// The remote already has the pushed branch and the created PR; only the
		// client's acknowledgement was lost.
		command(t, task.Workspace, "/usr/bin/git", "push", filepath.Join(fixture.root, "remote.git"), *task.OutputCommit+":refs/heads/"+task.Branch)
		pr := []map[string]any{{
			"number": 1, "title": "x", "body": "<!-- octomus:task:" + task.ID + " -->",
			"head":     map[string]any{"ref": task.Branch, "sha": "", "repo": map[string]any{"full_name": "fixture/project"}},
			"base":     map[string]any{"ref": "main"},
			"html_url": "https://github.com/fixture/project/pull/1", "state": "open",
			"merged_at": nil, "additions": 1, "deletions": 0, "created_at": "2026-09-07T00:00:00Z",
		}}
		data, err := json.Marshal(pr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
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
		command(t, task.Workspace, "/usr/bin/git", "push", filepath.Join(fixture.root, "remote.git"), *task.OutputCommit+":refs/heads/"+task.Branch)
		// The delivered PR was closed before the checkpoint reconciled: the
		// closed match is explicit reconcile evidence, not a new write.
		pr := []map[string]any{{
			"number": 1, "title": "x", "body": "<!-- octomus:task:" + task.ID + " -->",
			"head":     map[string]any{"ref": task.Branch, "sha": "", "repo": map[string]any{"full_name": "fixture/project"}},
			"base":     map[string]any{"ref": "main"},
			"html_url": "https://github.com/fixture/project/pull/1", "state": "closed",
			"merged_at": nil, "additions": 1, "deletions": 0, "created_at": "2026-09-07T00:00:00Z",
		}}
		data, err := json.Marshal(pr)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
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

// TestExecutionRestartRequeuesInitializedTask: a task that died mid-execution
// with an intact workspace resumes its executor session instead of restarting.
func TestExecutionRestartRequeuesInitializedTask(t *testing.T) {
	fixture := newExecutionFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.VerificationCommands = []string{"test -f feature.txt"}
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	task := executionTask(t, fixture, cfg.DefaultBranch)
	ctx := context.Background()
	ws := filepath.Join(fixture.dataDir, "tasks", task.ID, "workspace")
	if err := gitops.CloneAt(ctx, cfg, ws, task.SourceRevision); err != nil {
		t.Fatal(err)
	}
	thread := model.ID()
	task.Workspace = ws
	task.ComparisonBase = task.SourceRevision
	task.ExecutionSession = &thread
	task.Status = model.StatusExecuting
	task.Sessions = []model.Session{{ID: thread, Role: "executor", Status: model.SessionRunning}}
	// The fixture server must still know the thread for resume to succeed.
	if err := os.MkdirAll(filepath.Join(fixture.root, "threads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "threads", thread+".json"), []byte(`{"turn_started":true,"repairs":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	if err := app.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Get[model.Task](fixture.state, "task", task.ID)
	if err != nil || recovered == nil {
		t.Fatal(err)
	}
	if recovered.Status != model.StatusQueued || recovered.Attempts != 1 {
		t.Fatalf("interrupted task recovery = %+v", recovered)
	}
	if executors := sessionByRole(*recovered, "executor"); len(executors) != 1 || executors[0].Status != model.SessionInterrupted {
		t.Fatalf("running session not interrupted: %+v", executors)
	}
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("requeued delivery = %+v", saved)
	}
	if executors := sessionByRole(saved, "executor"); len(executors) != 1 || executors[0].Status != model.SessionCompleted {
		t.Fatalf("resumed executor session = %+v", executors)
	}
}

// existingPrBranch builds the existing-owned-PR remote state: the octomus/
// branch with earlier delivered work plus its open fixture PR.
func existingPrBranch(t *testing.T, fixture *planningFixture) string {
	t.Helper()
	work := filepath.Join(fixture.root, "existing-work")
	command(t, fixture.root, "/usr/bin/git", "clone", fixture.repo, work)
	command(t, work, "/usr/bin/git", "config", "user.name", "Fixture")
	command(t, work, "/usr/bin/git", "config", "user.email", "fixture@example.com")
	command(t, work, "/usr/bin/git", "checkout", "-b", "octomus/existing")
	if err := os.WriteFile(filepath.Join(work, "earlier.txt"), []byte("Preserve the earlier improvement.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command(t, work, "/usr/bin/git", "add", ".")
	command(t, work, "/usr/bin/git", "commit", "-m", "Earlier Octomus work")
	command(t, work, "/usr/bin/git", "push", filepath.Join(fixture.root, "remote.git"), "octomus/existing")
	out, err := exec.Command("/usr/bin/git", "-C", work, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))
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

// TestExecutionExistingPrAppendsComment: delivering onto an owned open PR
// appends a follow-up comment; the maintainer-visible description is never
// rewritten and no second PR is created.
func TestExecutionExistingPrAppendsComment(t *testing.T) {
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
	if remote := remoteHead(t, fixture, "octomus/existing"); remote != *saved.OutputCommit {
		t.Fatalf("remote = %s; want output %s", remote, *saved.OutputCommit)
	}
}

// TestExecutionDependenciesOrderAndRollback: the dependent task rebases onto
// the published dependency output; when the remote rewinds the delivered head,
// the dependent blocks instead of building on vanished work.
func TestExecutionDependenciesOrderAndRollback(t *testing.T) {
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
		// The remote rewinds the delivered head; the dependent must not build
		// on vanished work. The delivered commit is fetched into the trusted
		// checkout first — the same guarantee the fixture's lazy rollback makes
		// (the ancestry check requires the object locally).
		command(t, fixture.repo, "/usr/bin/git", "fetch", filepath.Join(fixture.root, "remote.git"), "octomus/existing")
		command(t, fixture.root, "/usr/bin/git", "--git-dir", filepath.Join(fixture.root, "remote.git"), "update-ref", "refs/heads/octomus/existing", head)
		saved := driveTask(t, fixture, app, second.ID)
		if saved.Status != model.StatusBlocked || saved.BlockedReason == nil {
			t.Fatalf("rollback dependent outcome = %+v", saved)
		}
		if *saved.BlockedReason != model.BlockedReasonDependencyBlocked && *saved.BlockedReason != model.BlockedReasonStaleBase {
			t.Fatalf("rollback blocked as %v", *saved.BlockedReason)
		}
		if saved.OutputCommit != nil {
			t.Fatalf("rollback authorized publication: %+v", saved)
		}
	})
}

// TestExecutionWorkerPanicBlocks: a dying worker cannot leave a durable active
// task; the task-guard writes the terminal record.
func TestExecutionWorkerPanicBlocks(t *testing.T) {
	fixture := newExecutionFixture(t)
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir, WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
		panic("worker exploded")
	})))
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil || !strings.Contains(*saved.Error, "panicked") {
		t.Fatalf("panic outcome = %+v", saved)
	}
}

func TestExecutionTimeoutJoinsCallbackBeforeFinalizing(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.TaskTimeoutSeconds = 0
	task := queuedTask(cfg, "slow-executor", "main", "tyk/slow-executor")
	task.Status = model.StatusExecuting
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	app := New(state, t.TempDir())
	release := make(chan struct{})
	cancelled := make(chan struct{})
	app.taskRunner = TaskRunnerFunc(func(ctx context.Context, task model.Task) error {
		return app.superviseExecution(ctx, task, func(ctx context.Context, task *model.Task) error {
			<-ctx.Done()
			close(cancelled)
			<-release // Model non-cancellable work followed by a durable write.
			task.Sessions = append(task.Sessions, model.Session{ID: "late", Status: model.SessionRunning})
			return app.saveTask(task)
		})
	})
	t.Cleanup(app.Shutdown)
	t.Cleanup(func() { close(release) })
	app.runTask(task)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("executor was not cancelled")
	}
	finished := make(chan struct{})
	go func() { app.wg.Wait(); close(finished) }()
	// Exceed WithDeadline's eight-second grace: ownership must still be held.
	select {
	case <-finished:
		t.Fatal("worker completed while its callback could still write")
	case <-time.After(9 * time.Second):
	}
	app.runtimeMu.Lock()
	_, active := app.runtime.tasks[task.ID]
	app.runtimeMu.Unlock()
	if !active {
		t.Fatal("executor lost runtime ownership before callback exit")
	}
	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Status != model.StatusExecuting {
		t.Fatalf("task finalized before callback exit: %+v, %v", saved, err)
	}
	release <- struct{}{}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish after callback exit")
	}
	saved, err = store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil || saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonTimeout {
		t.Fatalf("timeout evidence lost: %+v, %v", saved, err)
	}
	if len(saved.Sessions) != 1 || saved.Sessions[0].Status != model.SessionFailed {
		t.Fatalf("late callback state was not finalized: %+v", saved.Sessions)
	}
}

func TestExecutionDeadlineCallbackPanicBlocks(t *testing.T) {
	fixture := newExecutionFixture(t)
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	// Substitute only execution, preserving the production supervisor and its
	// separate deadline goroutine. The outer worker recovery cannot catch this.
	app.taskRunner = TaskRunnerFunc(func(ctx context.Context, task model.Task) error {
		return app.superviseExecution(ctx, task, func(_ context.Context, task *model.Task) error {
			task.Sessions = append(task.Sessions, model.Session{ID: "panicking-executor", Role: "executor", Status: model.SessionRunning})
			panic("executor exploded")
		})
	})
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil || !strings.Contains(*saved.Error, "Task worker panicked: executor exploded") {
		t.Fatalf("panic outcome = %+v", saved)
	}
	if len(saved.Sessions) != 1 || saved.Sessions[0].Status != model.SessionFailed {
		t.Fatalf("panic did not fail the running session: %+v", saved.Sessions)
	}
	events, err := fixture.state.Events(&task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "error" && strings.Contains(event.Message, "executor exploded") {
			return
		}
	}
	t.Fatal("panic did not record a supervisor error event")
}

// TestExecutionDeliversFullLifecycleViaOpenCode runs the same
// executor → fresh reviews → persistent repair → verification → publication
// lifecycle through the OpenCode HTTP/SSE fixture peer
// requires the lifecycle on both runners.
func TestExecutionDeliversFullLifecycleViaOpenCode(t *testing.T) {
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

// TestExecutionNoProgressLimitStopsIdenticalRepairs: a repair that reproduces
// the same tree yields the identical snapshot revision; the no-progress
// budget, not the repair cap, ends the task.
func TestExecutionNoProgressLimitStopsIdenticalRepairs(t *testing.T) {
	fixture := newExecutionFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.VerificationCommands = []string{"false"}
	cfg.MaxRepairRounds = 4
	cfg.MaxNoProgressRounds = 1
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	fixture.cfg = cfg
	task := executionTask(t, fixture, cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonVerificationFailed {
		t.Fatalf("no-progress outcome = %+v", saved)
	}
	// Reviews 3 and 4 cover the identical revision the no-op repair produced:
	// executor 'needs repair' → repair 'partial' → repair 'fixed' → clean
	// review + failed verification → repair rewriting 'fixed' changes nothing.
	if len(saved.Reviews) != 4 {
		t.Fatalf("reviews = %+v; want 4 rounds ending on the repeated revision", saved.Reviews)
	}
	if saved.Reviews[2].Revision != saved.Reviews[3].Revision {
		t.Fatalf("no-progress signature missing: %+v", saved.Reviews)
	}
	if saved.OutputCommit != nil || len(publications(t, fixture)) != 0 {
		t.Fatalf("no-progress task authorized publication: %+v", saved)
	}
}
