// Git and GitHub publication against real local Git and the gh fixture.

package git_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func realGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := process.Command("/usr/bin/git", cwd)
	cmd.Args = append(cmd.Args, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, cwd, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testConfig() config.Config {
	c := config.Default()
	c.GitHubRepo = "fixture/project"
	c.CommandTimeoutSeconds = 30
	return c
}

func fixtureRoot(t *testing.T) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	if err := testutil.MarkFixtureRoot(root); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, fixture := range map[string]string{"git": "git.sh", "gh": "gh.py"} {
		if err := testutil.InstallFixtureScript(filepath.Join(bin, name), fixture); err != nil {
			t.Fatal(err)
		}
	}
	checkout := filepath.Join(root, "checkout")
	if err := os.Mkdir(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "remote.git")
	realGit(t, root, "init", "--bare", remote)
	realGit(t, checkout, "init", "-b", "main")
	realGit(t, checkout, "config", "user.name", "Fixture")
	realGit(t, checkout, "config", "user.email", "fixture@example.com")
	writeFile(t, filepath.Join(checkout, "README.md"), "Feature contract: feature.txt must contain fixed.\n")
	realGit(t, checkout, "add", ".")
	realGit(t, checkout, "commit", "-m", "Initial fixture")
	realGit(t, checkout, "remote", "add", "origin", remote)
	realGit(t, checkout, "push", "-u", "origin", "main")
	realGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	c := testConfig()
	c.Repository = checkout
	c.GitHubRepo = "fixture/project"
	return c, root
}

const (
	envSecretName  = "OCTOMUS_FIXTURE_API_KEY"
	envSecretValue = "fixture-env-secret-0123456789"
)

func TestMain(m *testing.M) {
	if err := testutil.IsolateGitEnvironment(); err != nil {
		panic(err)
	}
	if err := os.Setenv(envSecretName, envSecretValue); err != nil {
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

func strptr(s string) *string { return &s }

func TestCloneAt(t *testing.T) {
	t.Parallel()
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	revision, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "tasks", "task-1", "workspace")
	if err := git.CloneAt(ctx, c, workspace, revision); err != nil {
		t.Fatal(err)
	}
	head, err := git.Git(ctx, c, workspace, []string{"rev-parse", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if head != revision {
		t.Fatalf("workspace HEAD = %s; want %s", head, revision)
	}
	origin, err := git.Git(ctx, c, workspace, []string{"remote", "get-url", "origin"})
	if err != nil {
		t.Fatal(err)
	}
	if origin != "https://github.com/fixture/project.git" {
		t.Fatalf("workspace origin = %q; want the trusted fixture URL", origin)
	}
	if name, err := git.Git(ctx, c, workspace, []string{"config", "user.name"}); err != nil || name != "Octomus Agent" {
		t.Fatalf("user.name = %q, %v", name, err)
	}
	if exclude, err := os.ReadFile(filepath.Join(filepath.Dir(workspace), "repo.git", "info", "exclude")); err != nil ||
		!strings.Contains(string(exclude), "/.octomus/") {
		t.Fatalf("exclude = %q, %v", exclude, err)
	}
	if err := git.CloneAt(ctx, c, workspace, revision); err == nil {
		t.Fatal("cloning over an existing workspace must fail")
	}
}

func TestRemoteValidation(t *testing.T) {
	t.Parallel()
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	if err := git.ValidateRemote(ctx, c); err != nil {
		t.Fatalf("valid fixture remote rejected: %v", err)
	}
	wrong := c
	wrong.GitHubRepo = "other/project"
	if err := git.ValidateRemote(ctx, wrong); err == nil ||
		!strings.Contains(err.Error(), "Origin does not match configured GitHub repository") {
		t.Fatalf("mismatched repository = %v; want an origin match failure", err)
	}
	main, err := git.RemoteRevision(ctx, c, "main")
	if err != nil {
		t.Fatal(err)
	}
	head, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	if main == nil || *main != head {
		t.Fatalf("remote main = %v; want %s", main, head)
	}
	if missing, err := git.RemoteRevision(ctx, c, "octomus/missing"); err != nil || missing != nil {
		t.Fatalf("missing branch = %v, %v; want no revision", missing, err)
	}
	if _, err := git.RemoteRevision(ctx, c, ".."); err == nil {
		t.Fatal("an invalid branch name must be rejected before ls-remote")
	}
	if err := git.Fetch(ctx, c); err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

func TestCleanlinessAndSnapshot(t *testing.T) {
	t.Parallel()
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	base, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := git.CloneAt(ctx, c, workspace, base); err != nil {
		t.Fatal(err)
	}
	if ok, err := git.At(ctx, c, workspace, base); err != nil || !ok {
		t.Fatalf("clean checkout at %s = %v, %v; want at", base, ok, err)
	}
	if ok, err := git.At(ctx, c, workspace, strings.Repeat("0", 40)); err != nil || ok {
		t.Fatalf("clean checkout at a foreign revision = %v, %v; want not at", ok, err)
	}
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
	if ok, err := git.At(ctx, c, workspace, base); err != nil || ok {
		t.Fatalf("dirty checkout = %v, %v; want not at", ok, err)
	}
	commit, err := git.Snapshot(ctx, c, workspace, "Deliver the feature")
	if err != nil {
		t.Fatal(err)
	}
	if commit == base {
		t.Fatal("a snapshot with staged changes must produce a new commit")
	}
	if ok, err := git.IsAncestor(ctx, c, workspace, base, commit); err != nil || !ok {
		t.Fatalf("snapshot ancestry = %v, %v; want the base preserved", ok, err)
	}
	if ok, err := git.At(ctx, c, workspace, commit); err != nil || !ok {
		t.Fatalf("post-snapshot workspace = %v, %v; want clean at the new head", ok, err)
	}
	if again, err := git.Snapshot(ctx, c, workspace, "No change"); err != nil || again != commit {
		t.Fatalf("unchanged snapshot = %s, %v; want %s", again, err, commit)
	}
}

func publicationTask(c config.Config, workspace, commit, source, id string) model.Task {
	return model.Task{
		ID:              id,
		CycleID:         "cycle",
		Proposal:        model.Proposal{Title: "Concrete improvement", Problem: "Missing behavior", Benefit: "Useful behavior", Scope: "one file"},
		Status:          model.StatusPublishing,
		Config:          c,
		SourceRevision:  source,
		ComparisonBase:  source,
		DefaultRevision: source,
		Branch:          "octomus/work",
		Workspace:       workspace,
		Sessions:        []model.Session{{ID: "s1", Role: "executor", Status: model.SessionCompleted, Summary: "Implemented the feature"}},
		Reviews:         []model.ReviewRound{{SessionID: "r1", Revision: commit, Result: model.Review{Completed: true, Summary: "clean"}, CreatedAt: model.Now()}},
		Verification:    []model.Verification{{Command: "make test", Success: true, Revision: commit, CreatedAt: model.Now()}},
		OutputCommit:    strptr(commit),
		CreatedAt:       model.Now(),
		UpdatedAt:       model.Now(),
	}
}

func publishableTask(t *testing.T, c config.Config, root, id string) (model.Task, string) {
	t.Helper()
	ctx := context.Background()
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "data", "tasks", id, "workspace")
	if err := git.CloneAt(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
	commit, err := git.Snapshot(ctx, c, workspace, "Deliver the feature")
	if err != nil {
		t.Fatal(err)
	}
	return publicationTask(c, workspace, commit, source, id), commit
}

func TestPublishCreatesPR(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "data", "tasks", "task-1", "workspace")
	if err := git.CloneAt(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
	commit, err := git.Snapshot(ctx, c, workspace, "Deliver the feature")
	if err != nil {
		t.Fatal(err)
	}
	task := publicationTask(c, workspace, commit, source, "task-1")
	pr, err := git.Publish(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 1 || pr.URL != "https://github.com/fixture/project/pull/1" {
		t.Fatalf("published = %+v; want PR #1 at the fixture URL", pr)
	}
	remoteHead := realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"),
		"rev-parse", "refs/heads/octomus/work")
	if remoteHead != commit {
		t.Fatalf("remote branch = %s; want the reviewed commit %s", remoteHead, commit)
	}
	if remoteMain := realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"),
		"rev-parse", "main"); remoteMain != source {
		t.Fatalf("remote main = %s; want untouched %s", remoteMain, source)
	}
	prs, err := os.ReadFile(filepath.Join(root, "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var created []map[string]any
	if err := json.Unmarshal(prs, &created); err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("prs.json = %s; want exactly one PR", prs)
	}
	if title, _ := created[0]["title"].(string); title != "Concrete improvement" {
		t.Fatalf("outbound title = %q; want the proposal title", title)
	}
	body, _ := created[0]["body"].(string)
	if !strings.Contains(body, "<!-- octomus:task:task-1 -->") ||
		!strings.Contains(body, "Reviewed commit: `"+commit+"`") {
		t.Fatalf("outbound body lost delivery identity: %q", body)
	}
	publications, err := os.ReadFile(filepath.Join(root, "publications.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(publications), `"action": "create"`) {
		t.Fatalf("publications = %s; want a create action", publications)
	}
	again, err := git.Publish(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if again.Number != 1 {
		t.Fatalf("re-publication = %+v; want the existing PR #1", again)
	}
	entries, _ := os.ReadFile(filepath.Join(root, "publications.jsonl"))
	if count := strings.Count(string(entries), `"action"`); count != 1 {
		t.Fatalf("re-publication wrote %d actions; want idempotent delivery", count)
	}
}

func TestPublishRedaction(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	ctx := context.Background()
	token := "ghp_fixtureToken0123456789"
	secretCommand := "curl https://fixture:fixtureSecret99@example.com/health"
	c.VerificationCommands = []string{"make test", secretCommand}
	task, commit := publishableTask(t, c, root, "task-secret")
	task.Proposal.Title = "Fix the leak " + token
	task.Proposal.Problem = "Missing behavior. Call Bearer fixtureBearerToken123 then " + envSecretValue + "."
	task.Proposal.Benefit = "Harmless benefit text stays verbatim."
	task.Sessions[0].Summary = "Implemented using sk-fixtureSummarySecret99"
	task.Verification = []model.Verification{
		{Command: "make test", Success: true, Revision: commit, CreatedAt: model.Now()},
		{Command: secretCommand, Success: true, Revision: commit, CreatedAt: model.Now()},
	}
	pr, err := git.Publish(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prs []map[string]any
	if err := json.Unmarshal(data, &prs); err != nil || len(prs) != 1 {
		t.Fatalf("prs.json = %s, %v; want one PR", data, err)
	}
	title, _ := prs[0]["title"].(string)
	body, _ := prs[0]["body"].(string)
	sent := title + "\n" + body
	for _, secret := range []string{token, envSecretValue, "fixtureBearerToken123", "sk-fixtureSummarySecret99", "fixtureSecret99"} {
		if strings.Contains(sent, secret) {
			t.Fatalf("public metadata leaked %q: %q", secret, sent)
		}
	}
	if title != "Fix the leak [redacted]" {
		t.Fatalf("outbound title = %q; want the scrubbed form", title)
	}
	for _, want := range []string{
		"Harmless benefit text stays verbatim.",
		"[redacted]example.com/health",
		"Implemented using [redacted]",
		"<!-- octomus:task:task-secret -->",
		"Reviewed commit: `" + commit + "`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("outbound body missing %q: %q", want, body)
		}
	}
	if pr.Number != 1 {
		t.Fatalf("published = %+v; want PR #1", pr)
	}
	again, err := git.Publish(ctx, task)
	if err != nil || again.Number != 1 {
		t.Fatalf("re-publication = %+v, %v; want the existing PR", again, err)
	}
	entries, _ := os.ReadFile(filepath.Join(root, "publications.jsonl"))
	if count := strings.Count(string(entries), `"action"`); count != 1 {
		t.Fatalf("re-publication wrote %d actions; want idempotent delivery", count)
	}
}

func TestPublishRejectsStaleBase(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	ctx := context.Background()
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "data", "tasks", "task-3", "workspace")
	if err := git.CloneAt(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
	commit, err := git.Snapshot(ctx, c, workspace, "Deliver the feature")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(c.Repository, "other.txt"), "moved\n")
	realGit(t, c.Repository, "add", ".")
	realGit(t, c.Repository, "commit", "-m", "External movement")
	realGit(t, c.Repository, "push", "origin", "main")
	task := publicationTask(c, workspace, commit, source, "task-3")
	_, err = git.Publish(ctx, task)
	if err == nil {
		t.Fatal("publication over a moved base must fail")
	}
	if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonStaleBase {
		t.Fatalf("reason = %v; want stale_base", reason)
	}
	if rev, err := git.RemoteRevision(ctx, c, "octomus/work"); err != nil || rev != nil {
		t.Fatalf("remote branch = %v, %v; want nothing pushed", rev, err)
	}
}
