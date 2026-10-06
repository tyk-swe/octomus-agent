package engine

// The one engine test fixture: a synthetic repository with a bare remote, the gh fixture behind the PATH
// dispatcher, a state database and a scripted runner for every route.

import (
	"context"
	"database/sql"
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
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestMain(m *testing.M) {
	if err := testutil.IsolateGitEnvironment(); err != nil {
		panic(err)
	}
	cleanup, err := testutil.InstallFixtureCommands()
	if err != nil {
		panic(err)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

type fixtureRoutes struct {
	Executor, Reviewer, Repair                config.Route
	Orchestrator, Discovery, ProposalReviewer config.Route
}

func (r fixtureRoutes) all() []config.Route {
	return []config.Route{r.Executor, r.Reviewer, r.Repair, r.Orchestrator, r.Discovery, r.ProposalReviewer}
}

type fixture struct {
	root, dataDir, repo string
	cfg                 config.Config
	state               *store.Store
	script              *runnertest.Script
	routes              fixtureRoutes
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	repo := filepath.Join(root, "repository")
	remote := filepath.Join(root, "remote.git")
	dataDir := filepath.Join(root, "data")
	for _, directory := range []string{bin, repo, dataDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := testutil.MarkFixtureRoot(root); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"git": "git.sh", "gh": "gh.py"} {
		if err := testutil.InstallFixtureScript(filepath.Join(bin, name), script); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "prs.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "--bare", "--initial-branch=main", remote)
	git(t, root, "init", "--initial-branch=main", repo)
	git(t, repo, "config", "user.name", "Fixture")
	git(t, repo, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Fixture\n\nThe feature contract requires fixed output.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "Initial fixture")
	git(t, repo, "remote", "add", "origin", remote)
	git(t, repo, "push", "-u", "origin", "main")

	routes := fixtureRoutes{
		Executor:         config.NewRoute("scripted-executor", "medium"),
		Reviewer:         config.NewRoute("scripted-reviewer", "high"),
		Repair:           config.NewRoute("scripted-repair", "medium"),
		Orchestrator:     config.NewRoute("scripted-orchestrator", "medium"),
		Discovery:        config.NewRoute("scripted-discovery", "medium"),
		ProposalReviewer: config.NewRoute("scripted-proposal-reviewer", "medium"),
	}
	cfg := testConfig(repo)
	cfg.SessionTimeoutSeconds = 15
	cfg.CommandTimeoutSeconds = 5
	cfg.MaxSessionsPerDay = 30
	cfg.CodexBinary = filepath.Join(root, "no-codex-installed")
	cfg.OpencodeBinary = filepath.Join(root, "no-opencode-installed")
	cfg.Roles["orchestrator"] = routes.Orchestrator
	cfg.Roles["discovery"] = routes.Discovery
	cfg.Roles["proposal_reviewer"] = routes.ProposalReviewer
	cfg.Roles["code_reviewer"] = routes.Reviewer
	for _, tier := range config.Tiers() {
		cfg.Tiers[tier] = routes.Executor
	}
	cfg.RepairRoute = routes.Repair
	state := openStore(t, root)
	saveSettings(t, state, cfg, model.DefaultControl())
	return &fixture{root: root, dataDir: dataDir, repo: repo, cfg: cfg, state: state,
		script: runnertest.New(runnertest.CatalogFor(routes.all()...)...), routes: routes}
}

func (f *fixture) configure(t *testing.T, adjust func(*config.Config)) {
	t.Helper()
	cfg := f.cfg.Clone()
	adjust(&cfg)
	if err := f.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
}

// pausedApp is an App on the fixture's scripted runner, with housekeeping deferred.
func (f *fixture) pausedApp(t *testing.T, options ...Option) *App {
	t.Helper()
	app := New(f.state, f.dataDir, append([]Option{WithRunnerConnector(f.script.Connector())}, options...)...)
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	return app
}

// newApp is pausedApp resumed into continuous operation.
func (f *fixture) newApp(t *testing.T, options ...Option) *App {
	t.Helper()
	app := f.pausedApp(t, options...)
	if err := control(app, "resume"); err != nil {
		t.Fatal(err)
	}
	return app
}

func control(app *App, action string) error {
	_, err := app.ControlAction(action)
	return err
}

func deferHousekeeping(app *App) {
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	return openStore(t, t.TempDir())
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	state, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func testConfig(repository string) config.Config {
	_ = os.MkdirAll(filepath.Join(repository, ".git"), 0o755)
	cfg := config.Default()
	cfg.Repository = repository
	cfg.GitHubRepo = "fixture/project"
	cfg.DefaultBranch = "main"
	cfg.BranchPrefix = "octomus/"
	for _, role := range config.Roles() {
		cfg.Roles[role] = config.NewRoute("gpt-6-astra", "medium")
	}
	for _, tier := range config.Tiers() {
		cfg.Tiers[tier] = config.NewRoute("gpt-6-astra", "medium")
	}
	cfg.RepairRoute = config.NewRoute("gpt-6-astra", "medium")
	cfg.VerificationCommands = []string{"true"}
	return cfg
}

func saveSettings(t *testing.T, state *store.Store, cfg config.Config, control model.Control) {
	t.Helper()
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
}

// git runs a fixture git command and fails the test on error.
func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	output, err := gitOutput(directory, args...)
	if err != nil {
		t.Fatalf("git %s in %s: %v", strings.Join(args, " "), directory, err)
	}
	return output
}

// gitOutput runs a fixture git command where a test failure cannot be raised, such as a scripted runner effect.
func gitOutput(directory string, args ...string) (string, error) {
	cmd := gitCommand(directory, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, stderr.String())
	}
	return strings.TrimSpace(string(output)), nil
}

func gitCommand(directory string, args ...string) *exec.Cmd {
	cmd := process.Command("/usr/bin/git", directory)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

func remoteHead(t *testing.T, f *fixture, branch string) string {
	t.Helper()
	return git(t, f.root, "--git-dir", filepath.Join(f.root, "remote.git"), "rev-parse", branch)
}

// Builders.

func proposal(id, target string) model.Proposal {
	return model.Proposal{
		ID: id, Title: "Concrete " + id, Problem: "Missing behavior " + id,
		Evidence: []string{"README.md"}, Benefit: "Useful behavior", Category: "features",
		Target: target, Tier: "M", Scope: "one file", Dependencies: []string{},
		Prompt: "Implement and verify the documented behavior", Decision: model.DecisionAccepted,
		Reason: "Grounded and worthwhile", ProblemKey: "problem-" + id,
		RelevantPaths: []string{"README.md"}, Reconsiders: []string{},
	}
}

func queuedTask(cfg config.Config, id, target, branch string) model.Task {
	p := proposal(id, target)
	return model.Task{
		ID: id, CycleID: "cycle", Proposal: p, Status: model.StatusQueued,
		Route: cfg.Tiers[p.Tier], Config: cfg.Clone(), SourceRevision: "source",
		ComparisonBase: "source", DefaultRevision: "source", Branch: branch,
		Sessions: []model.Session{}, Reviews: []model.ReviewRound{}, Verification: []model.Verification{},
		CreatedAt: model.Now(), UpdatedAt: model.Now(), SupersededBy: []string{}, Supersedes: []string{},
	}
}

func ownedPR(branch string) model.PullRequest {
	return model.PullRequest{
		Number: 7, Title: "Owned work", Branch: branch, Head: "head", Base: "main",
		URL: "https://example.test/pr/7", State: "open", Owned: true,
		HeadRepository: "fixture/project", BaseRepository: "fixture/project",
	}
}

// executionTask is a queued task on the fixture remote's current head, ready for the scripted executor.
func executionTask(t *testing.T, f *fixture, target string) model.Task {
	t.Helper()
	id := model.ID()
	task := queuedTask(f.cfg, id, target, f.cfg.BranchPrefix+id)
	task.SourceRevision = remoteHead(t, f, target)
	task.DefaultRevision = remoteHead(t, f, f.cfg.DefaultBranch)
	task.ComparisonBase = ""
	task.Proposal.Prompt = "Create feature.txt with fixed output and verify its contents. fixture-file=feature.txt"
	task.Proposal.Title = "Complete the fixture feature"
	return task
}

func putTask(t *testing.T, f *fixture, task model.Task) {
	t.Helper()
	if err := f.state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
}

func loadTask(t *testing.T, state *store.Store, id string) model.Task {
	t.Helper()
	task, err := store.Get[model.Task](state, "task", id)
	if err != nil || task == nil {
		t.Fatalf("load task %s: %+v, %v", id, task, err)
	}
	return *task
}

// driveTask ticks the scheduler until the task reaches a terminal status.
func driveTask(t *testing.T, f *fixture, app *App, taskID string) model.Task {
	t.Helper()
	task, err := driveTaskResult(f, app, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func driveTaskResult(f *fixture, app *App, taskID string) (model.Task, error) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := app.tick(); err != nil {
			return model.Task{}, err
		}
		app.wg.Wait()
		task, err := store.Get[model.Task](f.state, "task", taskID)
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
	current, _ := store.Get[model.Task](f.state, "task", taskID)
	return model.Task{}, fmt.Errorf("task %s did not finish: %+v", taskID, current)
}

func waitCycle(t *testing.T, state *store.Store, id string) model.Cycle {
	t.Helper()
	var cycle *model.Cycle
	if !testutil.WaitUntil(30*time.Second, func() bool {
		var err error
		if cycle, err = store.Get[model.Cycle](state, "cycle", id); err != nil {
			t.Fatal(err)
		}
		return cycle != nil && cycle.Status != model.CycleRunning
	}) {
		t.Fatalf("cycle %s did not finish", id)
	}
	return *cycle
}

func waitOnlyCycle(t *testing.T, state *store.Store) model.Cycle {
	t.Helper()
	var cycles []model.Cycle
	if !testutil.WaitUntil(30*time.Second, func() bool {
		var err error
		if cycles, err = store.List[model.Cycle](state, "cycle"); err != nil {
			t.Fatal(err)
		}
		return len(cycles) == 1 && cycles[0].Status != model.CycleRunning
	}) {
		t.Fatal("planning cycle did not finish")
	}
	return cycles[0]
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

func blockedAs(task model.Task, reason model.BlockedReason) bool {
	return task.Status == model.StatusBlocked && task.BlockedReason != nil && *task.BlockedReason == reason
}

func optionalText(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func cleanReview(summary string) string {
	return `{"completed": true, "summary": "` + summary + `", "findings": []}`
}

func writeFile(name, content string) func(string) error {
	return func(cwd string) error {
		return os.WriteFile(filepath.Join(cwd, name), []byte(content), 0o644)
	}
}

// GitHub fixture state.

func publications(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "publications.jsonl"))
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

func prsJSON(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prs []map[string]any
	if err := json.Unmarshal(data, &prs); err != nil {
		t.Fatal(err)
	}
	return prs
}

// existingPRBranch pushes an owned branch the gh fixture reports as open PR 42 and returns its head.
func existingPRBranch(t *testing.T, f *fixture) string {
	t.Helper()
	work := filepath.Join(f.root, "existing-work")
	git(t, f.root, "clone", f.repo, work)
	git(t, work, "config", "user.name", "Fixture")
	git(t, work, "config", "user.email", "fixture@example.com")
	git(t, work, "checkout", "-b", "octomus/existing")
	if err := os.WriteFile(filepath.Join(work, "earlier.txt"), []byte("Preserve the earlier improvement.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "Earlier Octomus work")
	git(t, work, "push", filepath.Join(f.root, "remote.git"), "octomus/existing")
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
	if err := os.WriteFile(filepath.Join(f.root, "prs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return head
}

// checkpointedTask is a task whose executor already committed and whose review and verification passed,
// stopped just before publication.
func checkpointedTask(t *testing.T, f *fixture, target string) model.Task {
	t.Helper()
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"test -f feature.txt"} })
	task := executionTask(t, f, target)
	ctx := context.Background()
	ws := filepath.Join(f.dataDir, "tasks", task.ID, "workspace")
	if err := gitops.CloneAt(ctx, f.cfg, ws, task.SourceRevision); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit, err := gitops.Snapshot(ctx, f.cfg, ws, task.Proposal.Title)
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

// heldUploadPack makes the fixture remote's upload-pack wait on a `hold` marker, so a Git preflight can be
// observed in flight; `fail` makes it exit non-zero.
func heldUploadPack(t *testing.T, f *fixture) {
	t.Helper()
	script := "#!/bin/sh\nfixture_dir=$(dirname \"$0\")\ntouch \"$fixture_dir/entered-$$\"\nwhile [ -e \"$fixture_dir/hold\" ]; do sleep 0.02; done\nif [ -e \"$fixture_dir/fail\" ]; then exit 1; fi\nexec git-upload-pack \"$@\"\n"
	path := filepath.Join(f.root, "held-upload-pack")
	if err := testutil.WriteExecutable(path, []byte(script)); err != nil {
		t.Fatal(err)
	}
	git(t, f.repo, "config", "remote.origin.uploadpack", path)
	if err := os.WriteFile(filepath.Join(f.root, "hold"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForPreflights(t *testing.T, f *fixture, count int) {
	t.Helper()
	entered := func() int {
		entries, err := os.ReadDir(f.root)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "entered-") {
				n++
			}
		}
		return n
	}
	if !testutil.WaitUntil(30*time.Second, func() bool { return entered() >= count }) {
		t.Fatalf("Git preflight did not reach the controlled remote")
	}
}

func releasePreflight(t *testing.T, f *fixture) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.root, "hold")); err != nil {
		t.Fatal(err)
	}
}

// Assertions.

func assertAdmissions(t *testing.T, state *store.Store, want uint64, label string) {
	t.Helper()
	// These fresh-fixture scenarios assert all turns, even across UTC midnight.
	// Check both the ledger and durable daily counters in the same snapshot.
	var admissions, sessions uint64
	err := state.Snapshot(func(conn *sql.Conn) error {
		return conn.QueryRowContext(context.Background(), `SELECT
			(SELECT COUNT(*) FROM admissions),
			(SELECT COALESCE(SUM(sessions), 0) FROM usage)`).Scan(&admissions, &sessions)
	})
	if err != nil || admissions != want || sessions != want {
		t.Fatalf("fixture admissions = %d, usage = %d, %v; want %d (%s)", admissions, sessions, err, want, label)
	}
}

func assertNoOpenClients(t *testing.T, script *runnertest.Script) {
	t.Helper()
	if open := script.OpenClients(); open != 0 {
		t.Fatalf("%d runner clients left open", open)
	}
}

func assertUnpublished(t *testing.T, f *fixture, task model.Task) {
	t.Helper()
	if task.OutputCommit != nil || task.PRNumber != nil || len(publications(t, f)) != 0 {
		t.Fatalf("task authorized publication: %+v", task)
	}
}
