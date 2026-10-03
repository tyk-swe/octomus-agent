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

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
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
	assertAdmissions(t, fixture.state, 6, "executor + 3 reviewers + 2 repairs")
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
	ws := filepath.Join(fixture.root, "verify", "workspace")
	git(t, fixture.root, "init", "--initial-branch=main", "--separate-git-dir", filepath.Join(fixture.root, "verify", "repo.git"), ws)
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

func TestVerificationSeesOnlyTheReviewedCommit(t *testing.T) {
	t.Parallel()
	app, task, revision := verificationFixture(t, []string{
		"test \"$(cat impl.txt)\" = 0", "test ! -e planted.txt", "test ! -d node_modules", "echo built > coverage.out"})
	if err := os.WriteFile(filepath.Join(filepath.Dir(task.Workspace), "repo.git", "info", "exclude"), []byte("planted.txt\nnode_modules/\ncoverage.out\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"planted.txt": "left by a session\n", "node_modules/fake/index.js": "exit(0)\n"} {
		path := filepath.Join(task.Workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	failures, err := app.verifyRevision(context.Background(), &task, revision)
	if err != nil || len(failures) != 0 {
		t.Fatalf("verification = %v, %v; ignored files in the task work tree must not reach the checkout", failures, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(task.Workspace), verificationDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification checkout was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(task.Workspace, "planted.txt")); err != nil {
		t.Fatal("the task work tree itself must be left as it was")
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
	assertAdmissions(t, fixture.state, 6, "OpenCode executor + 3 reviewers + 2 repairs")
}
