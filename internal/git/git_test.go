package git_test

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

func initRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit(t, repo, "init", "-b", "main")
	realGit(t, repo, "config", "user.name", "Fixture")
	realGit(t, repo, "config", "user.email", "fixture@example.com")
	writeFile(t, filepath.Join(repo, "README.md"), "fixture\n")
	realGit(t, repo, "add", ".")
	realGit(t, repo, "commit", "-m", "Initial fixture")
	return repo
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

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestGitAncestryIsAPredicateAndCommandErrorsFailClosed(t *testing.T) {
	t.Parallel()
	repository := initRepo(t)
	c := testConfig()
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if _, err := git.Git(ctx, c, repository, args); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com",
		"commit", "--allow-empty", "-m", "one")
	run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.com",
		"commit", "--allow-empty", "-m", "two")
	first, err := git.Git(ctx, c, repository, []string{"rev-parse", "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := git.Git(ctx, c, repository, []string{"rev-parse", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := git.IsAncestor(ctx, c, repository, first, second); err != nil || !ok {
		t.Fatalf("ancestor predicate = %v, %v; want true", ok, err)
	}
	if ok, err := git.IsAncestor(ctx, c, repository, second, first); err != nil || ok {
		t.Fatalf("reversed predicate = %v, %v; want false", ok, err)
	}
	if _, err := git.IsAncestor(ctx, c, repository, strings.Repeat("0", 40), second); err == nil {
		t.Fatal("a missing object must be an error, not a false predicate")
	}
	if _, err := git.Git(ctx, c, repository, []string{"rev-parse", "refs/heads/missing"}); err == nil {
		t.Fatal("a failed rev-parse must be an error")
	}
}

func TestCloneAtCreatesAnIndependentCheckout(t *testing.T) {
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

func TestRemoteValidationAndRevisionLookup(t *testing.T) {
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

func TestValidateRemoteForms(t *testing.T) {
	repo := initRepo(t)
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "gh"),
		"#!/bin/sh\n[ \"$*\" = \"auth status --hostname github.com\" ] || exit 1\n[ -e \"$0.unauthenticated\" ] && exit 1\nexit 0\n")
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	c := testConfig()
	c.Repository = repo
	ctx := context.Background()
	realGit(t, repo, "remote", "add", "origin", "https://github.com/fixture/project.git")
	const (
		transport = "Origin must use github.com via SSH or credential-free HTTPS"
		mismatch  = "Origin does not match configured GitHub repository"
	)
	for _, tc := range []struct {
		origin string
		want   string
	}{
		{"https://github.com/fixture/project.git", ""},
		{"git@github.com:fixture/project.git", ""},
		{"ssh://git@github.com/fixture/project.git", ""},
		{"https://github.com/fixture/project", ""},
		{"https://github.com/Fixture/Project.git", ""},
		{"https://user:token@github.com/fixture/project.git", transport},
		{"https://x-access-token:ghp_abc@github.com/fixture/project.git", transport},
		{"http://github.com/fixture/project.git", transport},
		{"https://gitlab.com/fixture/project.git", transport},
		{"ssh://git@github.com:22/fixture/project.git", transport},
		{"https://github.com/other/project.git", mismatch},
		{"https://github.com/fixture/project/extra.git", mismatch},
		{"git@github.com:fixture/project.git/", mismatch},
	} {
		realGit(t, repo, "remote", "set-url", "origin", tc.origin)
		err := git.ValidateRemote(ctx, c)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("origin %q rejected: %v", tc.origin, err)
		case tc.want != "" && (err == nil || err.Error() != tc.want):
			t.Errorf("origin %q = %v; want %q", tc.origin, err, tc.want)
		}
	}
	realGit(t, repo, "remote", "set-url", "origin", "https://github.com/fixture/project.git")
	writeFile(t, filepath.Join(bin, "gh.unauthenticated"), "")
	if err := git.ValidateRemote(ctx, c); err == nil {
		t.Fatal("an unauthenticated gh must fail remote validation")
	}
}

func TestRemoteRevisionIgnoresTailMatchingRefs(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	ctx := context.Background()
	main := realGit(t, c.Repository, "rev-parse", "main")
	realGit(t, c.Repository, "commit", "--allow-empty", "-m", "decoy")
	decoy := realGit(t, c.Repository, "rev-parse", "HEAD")
	remote := filepath.Join(root, "remote.git")
	realGit(t, c.Repository, "push", remote, "HEAD:refs/heads/a/refs/heads/main")
	realGit(t, c.Repository, "push", remote, "HEAD:refs/heads/z/refs/heads/octomus/missing")
	got, err := git.RemoteRevision(ctx, c, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != main {
		t.Fatalf("remote main = %v; want %s, not the decoy %s", deref(got), main, decoy)
	}
	if missing, err := git.RemoteRevision(ctx, c, "octomus/missing"); err != nil || missing != nil {
		t.Fatalf("missing branch = %v, %v; want no revision despite the tail-matching decoy", deref(missing), err)
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

func TestSnapshotScrubsCommitMessage(t *testing.T) {
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
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
	commit, err := git.Snapshot(ctx, c, workspace, "Rotate ghp_fixtureToken0123456789 and "+envSecretValue)
	if err != nil {
		t.Fatal(err)
	}
	message, err := git.Git(ctx, c, workspace, []string{"log", "-1", "--format=%B", commit})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(message) != "Rotate [redacted] and [redacted]" {
		t.Fatalf("snapshot commit message = %q; want both secrets scrubbed", message)
	}
}

func TestParseInventory(t *testing.T) {
	t.Parallel()
	c := testConfig()
	entry := func(number int, state, branch, head, base, baseRepo, body string) string {
		return fmt.Sprintf(`{"number":%d,"title":"t","head":{"ref":%q,"sha":%q,"repo":{"full_name":"fixture/project"}},"base":{"ref":%q,"repo":{"full_name":%q}},"html_url":"https://github.com/fixture/project/pull/%d","state":%q,"merged_at":null,"body":%q,"additions":1,"deletions":0,"created_at":"2026-09-07T00:00:00Z"}`,
			number, branch, head, base, baseRepo, number, state, body)
	}
	owned := entry(2, "open", "octomus/a", "abc", "main", "fixture/project", "x <!-- octomus:task:a -->")
	external := entry(5, "open", "contrib", "def", "main", "fixture/project", "no marker")
	inventory, err := git.ParseInventory("["+external+","+owned+"]\n["+owned+"]", c)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.PRs) != 2 || inventory.PRs[0].Number != 2 || inventory.PRs[1].Number != 5 {
		t.Fatalf("inventory = %+v; want entries 2 and 5 sorted", inventory.PRs)
	}
	if !inventory.PRs[0].Owned || inventory.PRs[1].Owned {
		t.Fatalf("ownership = %v/%v; want only the prefixed marker PR owned",
			inventory.PRs[0].Owned, inventory.PRs[1].Owned)
	}
	closed := entry(9, "closed", "octomus/b", "ghi", "main", "fixture/project", "<!-- octomus:task:b -->")
	if inv, err := git.ParseInventory("["+closed+"]", c); err != nil || len(inv.PRs) != 0 {
		t.Fatalf("closed entries = %+v, %v; want filtered out", inv.PRs, err)
	}
	conflict := entry(2, "open", "octomus/a", "different", "main", "fixture/project", "x <!-- octomus:task:a -->")
	if _, err := git.ParseInventory("["+owned+"]\n["+conflict+"]", c); err == nil {
		t.Fatal("conflicting duplicates must fail")
	}
	if _, err := git.ParseInventory("[]", c); err != nil {
		t.Fatalf("an empty page is a valid empty inventory: %v", err)
	}
	for _, response := range []string{"null", "[]\nnull", "null\n[]", "[" + owned + "]\nnull"} {
		if _, err := git.ParseInventory(response, c); err == nil {
			t.Errorf("null pagination page must fail: %s", response)
		}
	}
	if _, err := git.ParseInventory("", c); err == nil {
		t.Fatal("an empty response must fail")
	}
	missingHead := `{"number":3,"title":"t","head":{"ref":"","sha":""},"base":{"ref":"main","repo":{"full_name":"fixture/project"}},"html_url":"u","state":"open","merged_at":null,"body":""}`
	if _, err := git.ParseInventory("["+missingHead+"]", c); err == nil {
		t.Fatal("an entry missing required identity must fail")
	}
	foreign := entry(4, "open", "octomus/c", "jkl", "main", "other/project", "<!-- octomus:task:c -->")
	if _, err := git.ParseInventory("["+foreign+"]", c); err == nil {
		t.Fatal("a foreign base repository must fail")
	}
	weird := entry(6, "closing", "octomus/d", "mno", "main", "fixture/project", "<!-- octomus:task:d -->")
	if _, err := git.ParseInventory("["+weird+"]", c); err == nil {
		t.Fatal("an unrecognized state must fail")
	}
	if _, err := git.ParseInventory("{not json}", c); err == nil {
		t.Fatal("malformed output must fail")
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

func TestFixturePublishCreatesPullRequest(t *testing.T) {
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

func TestFixtureFollowUpAppendsComment(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	checkout := c.Repository
	realGit(t, checkout, "checkout", "-b", "octomus/existing")
	writeFile(t, filepath.Join(checkout, "earlier.txt"), "Preserve the earlier improvement.\n")
	realGit(t, checkout, "add", ".")
	realGit(t, checkout, "commit", "-m", "Earlier Octomus work")
	realGit(t, checkout, "push", "origin", "octomus/existing")
	earlier := realGit(t, checkout, "rev-parse", "HEAD")
	realGit(t, checkout, "checkout", "main")
	writeFile(t, filepath.Join(root, "prs.json"), fmt.Sprintf(`[{"number":42,"title":"An existing improvement","body":"Existing context.\n<!-- octomus:task:earlier -->","head":{"ref":"octomus/existing","sha":%q,"repo":{"full_name":"fixture/project"}},"base":{"ref":"main","repo":{"full_name":"fixture/project"}},"html_url":"https://github.com/fixture/project/pull/42","state":"open","merged_at":null,"additions":2000,"deletions":0,"created_at":"2026-08-01T00:00:00Z"}]`, earlier))
	workspace := filepath.Join(root, "data", "tasks", "task-2", "workspace")
	if err := git.CloneAt(ctx, c, workspace, earlier); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "followup.txt"), "more\n")
	commit, err := git.Snapshot(ctx, c, workspace, "Follow-up work")
	if err != nil {
		t.Fatal(err)
	}
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	task := publicationTask(c, workspace, commit, earlier, "task-2")
	task.DefaultRevision = source
	task.Branch = "octomus/existing"
	task.PRNumber = new(uint64)
	*task.PRNumber = 42
	pr, err := git.Publish(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 42 {
		t.Fatalf("follow-up = %+v; want PR #42", pr)
	}
	publications, err := os.ReadFile(filepath.Join(root, "publications.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(publications), `"action": "comment"`) {
		t.Fatalf("publications = %s; want a comment action", publications)
	}
	if strings.Contains(string(publications), `"action": "create"`) {
		t.Fatalf("publications = %s; a follow-up must not create a PR", publications)
	}
	prs, err := os.ReadFile(filepath.Join(root, "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []map[string]any
	if err := json.Unmarshal(prs, &saved); err != nil {
		t.Fatal(err)
	}
	comments, _ := saved[0]["comments"].([]any)
	if len(comments) != 1 || !strings.Contains(fmt.Sprint(comments[0]), "<!-- octomus:task:task-2 -->") {
		t.Fatalf("comments = %v; want the follow-up marker appended", comments)
	}
	if body, _ := saved[0]["body"].(string); !strings.HasPrefix(body, "Existing context.") {
		t.Fatalf("body = %q; want the original description preserved", body)
	}
}

func TestFixturePublishScrubsSecretsForPublicDelivery(t *testing.T) {
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

func TestFixtureFollowUpScrubsCommentMetadata(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	checkout := c.Repository
	realGit(t, checkout, "checkout", "-b", "octomus/existing")
	writeFile(t, filepath.Join(checkout, "earlier.txt"), "Preserve the earlier improvement.\n")
	realGit(t, checkout, "add", ".")
	realGit(t, checkout, "commit", "-m", "Earlier Octomus work")
	realGit(t, checkout, "push", "origin", "octomus/existing")
	earlier := realGit(t, checkout, "rev-parse", "HEAD")
	realGit(t, checkout, "checkout", "main")
	description := "Existing context.\n<!-- octomus:task:earlier -->"
	writeFile(t, filepath.Join(root, "prs.json"), fmt.Sprintf(`[{"number":42,"title":"An existing improvement","body":%q,"head":{"ref":"octomus/existing","sha":%q,"repo":{"full_name":"fixture/project"}},"base":{"ref":"main","repo":{"full_name":"fixture/project"}},"html_url":"https://github.com/fixture/project/pull/42","state":"open","merged_at":null,"additions":2000,"deletions":0,"created_at":"2026-08-01T00:00:00Z"}]`, description, earlier))
	workspace := filepath.Join(root, "data", "tasks", "task-followup", "workspace")
	if err := git.CloneAt(ctx, c, workspace, earlier); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "followup.txt"), "more\n")
	commit, err := git.Snapshot(ctx, c, workspace, "Follow-up work")
	if err != nil {
		t.Fatal(err)
	}
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	token := "ghp_followupSecret0123456789"
	task := publicationTask(c, workspace, commit, earlier, "task-followup")
	task.DefaultRevision = source
	task.Branch = "octomus/existing"
	task.PRNumber = new(uint64)
	*task.PRNumber = 42
	task.Proposal.Title = "Follow-up carrying " + token
	task.Proposal.Problem = "Additional evidence " + envSecretValue
	task.Sessions[0].Summary = "Repaired using Bearer followupBearer777"
	pr, err := git.Publish(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 42 {
		t.Fatalf("follow-up = %+v; want PR #42", pr)
	}
	prs, err := os.ReadFile(filepath.Join(root, "prs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []map[string]any
	if err := json.Unmarshal(prs, &saved); err != nil || len(saved) != 1 {
		t.Fatalf("prs.json = %s, %v; want the existing PR only", prs, err)
	}
	if body, _ := saved[0]["body"].(string); body != description {
		t.Fatalf("description = %q; want it preserved byte-for-byte", body)
	}
	comments, _ := saved[0]["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("comments = %v; want exactly one appended comment", comments)
	}
	comment, _ := comments[0].(map[string]any)["body"].(string)
	for _, secret := range []string{token, envSecretValue, "followupBearer777"} {
		if strings.Contains(comment, secret) {
			t.Fatalf("follow-up comment leaked %q: %q", secret, comment)
		}
	}
	for _, want := range []string{
		"Octomus follow-up: Follow-up carrying [redacted]",
		"<!-- octomus:task:task-followup -->",
		"Reviewed commit: `" + commit + "`",
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("follow-up comment missing %q: %q", want, comment)
		}
	}
	again, err := git.Publish(ctx, task)
	if err != nil || again.Number != 42 {
		t.Fatalf("replayed follow-up = %+v, %v; want PR #42", again, err)
	}
	entries, _ := os.ReadFile(filepath.Join(root, "publications.jsonl"))
	if count := strings.Count(string(entries), `"action"`); count != 1 {
		t.Fatalf("replayed follow-up wrote %d actions; want one", count)
	}
}

func TestFixturePublishLongBodyKeepsDeliveryIdentity(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	task, commit := publishableTask(t, c, root, "task-long")
	task.Proposal.Problem = strings.Repeat("Long harmless problem context. ", 800)
	if _, err := git.Publish(ctx, task); err != nil {
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
	body, _ := prs[0]["body"].(string)
	if len(body) <= 16384 {
		t.Fatalf("outbound body length = %d; want it beyond the operator-message cap", len(body))
	}
	if !strings.Contains(body, task.Proposal.Problem) {
		t.Fatal("long harmless problem text was shortened")
	}
	if !strings.HasSuffix(body, "<!-- octomus:task:task-long -->") ||
		!strings.Contains(body, "Reviewed commit: `"+commit+"`") {
		t.Fatal("outbound body lost its delivery identity")
	}
}

func TestPublishRefusesUnsafeMetadataBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		id     string
		adjust func(*model.Task)
		want   string
		echo   string
	}{
		{
			name: "oversized body",
			adjust: func(task *model.Task) {
				task.Proposal.Problem = "Leaked ghp_oversizeSecret777\n" + strings.Repeat("x", 70000)
			},
			want: "size limit",
			echo: "ghp_oversizeSecret777",
		},
		{
			name: "oversized title",
			adjust: func(task *model.Task) {
				task.Proposal.Title = strings.Repeat("t", 300)
			},
			want: "title limit",
		},
		{
			name: "empty title",
			adjust: func(task *model.Task) {
				task.Proposal.Title = " \t\n"
			},
			want: "empty",
		},
		{
			name: "NUL in title",
			adjust: func(task *model.Task) {
				task.Proposal.Title = "valid\x00title"
			},
			want: "unsupported character",
		},
		{
			name: "NUL in body",
			adjust: func(task *model.Task) {
				task.Proposal.Problem = "valid\x00body"
			},
			want: "unsupported character",
		},
		{
			name:   "marker collision",
			id:     "task-ghp_collisionSecret777",
			adjust: func(task *model.Task) {},
			want:   "marker",
			echo:   "ghp_collisionSecret777",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, root := fixtureRoot(t)
			c.VerificationCommands = []string{"make test"}
			ctx := context.Background()
			id := tc.id
			if id == "" {
				id = "task-refused"
			}
			task, _ := publishableTask(t, c, root, id)
			tc.adjust(&task)
			_, err := git.Publish(ctx, task)
			if err == nil {
				t.Fatal("unsafe publication metadata must be refused")
			}
			if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonWorkspaceInvalid {
				t.Fatalf("reason = %v; want workspace_invalid", reason)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal = %q; want %q", err, tc.want)
			}
			if tc.echo != "" && strings.Contains(err.Error(), tc.echo) {
				t.Fatalf("refusal echoed the rejected private text: %q", err)
			}
			if rev, err := git.RemoteRevision(ctx, c, task.Branch); err != nil || rev != nil {
				t.Fatalf("remote branch = %v, %v; want nothing pushed", rev, err)
			}
			if data, err := os.ReadFile(filepath.Join(root, "publications.jsonl")); err == nil &&
				strings.Contains(string(data), `"action"`) {
				t.Fatalf("publications = %s; want no writes", data)
			}
			if data, err := os.ReadFile(filepath.Join(root, "prs.json")); err == nil {
				if content := strings.TrimSpace(string(data)); content != "" && content != "[]" {
					t.Fatalf("prs.json = %s; want no pull requests", data)
				}
			}
		})
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

func TestFixturePublishListsLatestVerificationOnly(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test", "make lint"}
	task, commit := publishableTask(t, c, root, "task-latest")
	task.Verification = []model.Verification{
		{Command: "make test", Success: false, Revision: commit, CreatedAt: model.Now()},
		{Command: "make lint", Success: true, Revision: commit, CreatedAt: model.Now()},
		{Command: "make test", Success: true, Revision: commit, CreatedAt: model.Now()},
	}
	if _, err := git.Publish(context.Background(), task); err != nil {
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
	body, _ := prs[0]["body"].(string)
	if !strings.Contains(body, "Verification\n- `make test`: passed\n- `make lint`: passed\n") {
		t.Fatalf("outbound body = %q; want one latest line per configured command", body)
	}
	if strings.Contains(body, "failed") {
		t.Fatalf("outbound body = %q; a superseded failure must not be listed", body)
	}
}

func TestPublishGatesOnTheLatestVerificationPerCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		record func(commit string) []model.Verification
	}{
		{
			name: "latest failed after an earlier pass",
			record: func(commit string) []model.Verification {
				return []model.Verification{
					{Command: "make test", Success: true, Revision: commit},
					{Command: "make lint", Success: true, Revision: commit},
					{Command: "make test", Success: false, Revision: commit},
				}
			},
		},
		{
			name: "latest passed at another revision",
			record: func(commit string) []model.Verification {
				return []model.Verification{
					{Command: "make test", Success: true, Revision: commit},
					{Command: "make lint", Success: true, Revision: commit},
					{Command: "make lint", Success: true, Revision: strings.Repeat("0", 40)},
				}
			},
		},
		{
			name: "configured command never recorded",
			record: func(commit string) []model.Verification {
				return []model.Verification{{Command: "make test", Success: true, Revision: commit}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, root := fixtureRoot(t)
			c.VerificationCommands = []string{"make test", "make lint"}
			ctx := context.Background()
			task, commit := publishableTask(t, c, root, "task-gate")
			task.Verification = tc.record(commit)
			_, err := git.Publish(ctx, task)
			if err == nil {
				t.Fatal("publication without a latest passing verification must be refused")
			}
			if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonWorkspaceInvalid {
				t.Fatalf("reason = %v; want workspace_invalid", reason)
			}
			if !strings.Contains(err.Error(), "Publication requires successful verification at the reviewed revision") {
				t.Fatalf("refusal = %q; want the verification gate", err)
			}
			if rev, err := git.RemoteRevision(ctx, c, task.Branch); err != nil || rev != nil {
				t.Fatalf("remote branch = %v, %v; want nothing pushed", deref(rev), err)
			}
		})
	}
}

func TestPublishGatesRefuseBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	seedPRs := func(t *testing.T, root string, prs ...map[string]any) {
		t.Helper()
		data, err := json.Marshal(prs)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "prs.json"), string(data))
	}
	pullRequest := func(number int, headRepo, body string) map[string]any {
		return map[string]any{
			"number": number, "title": "Earlier work", "body": body,
			"head":     map[string]any{"ref": "octomus/work", "sha": "", "repo": map[string]any{"full_name": headRepo}},
			"base":     map[string]any{"ref": "main", "repo": map[string]any{"full_name": "fixture/project"}},
			"html_url": fmt.Sprintf("https://github.com/fixture/project/pull/%d", number),
			"state":    "open", "merged_at": nil, "additions": 1, "deletions": 0,
			"created_at": "2026-09-07T00:00:00Z",
		}
	}
	cases := []struct {
		name   string
		adjust func(t *testing.T, root string, task *model.Task, commit string)
		reason model.BlockedReason
		want   string
	}{
		{
			name:   "no reviewed commit",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) { task.OutputCommit = nil },
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "No reviewed commit",
		},
		{
			name:   "no review",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) { task.Reviews = nil },
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Publication requires a clean review at the output revision",
		},
		{
			name: "latest review at another revision",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				later := task.Reviews[0]
				later.Revision = strings.Repeat("0", 40)
				task.Reviews = append(task.Reviews, later)
			},
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Publication requires a clean review at the output revision",
		},
		{
			name: "latest review incomplete",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				task.Reviews[0].Result = model.Review{Completed: false, Summary: "interrupted"}
			},
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Publication requires a clean review at the output revision",
		},
		{
			name: "latest review has findings",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				task.Reviews[0].Result.Findings = []model.Finding{{Title: "Bug", File: "feature.txt", Detail: "wrong", Priority: "high"}}
			},
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Publication requires a clean review at the output revision",
		},
		{
			name:   "branch outside the owned prefix",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) { task.Branch = "feature/x" },
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Cannot publish outside the owned branch namespace",
		},
		{
			name: "default branch",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				task.Config.BranchPrefix = ""
				task.Branch = task.Config.DefaultBranch
			},
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Cannot publish outside the owned branch namespace",
		},
		{
			name: "workspace changed after review",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				writeFile(t, filepath.Join(task.Workspace, "late.txt"), "unreviewed\n")
			},
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Workspace changed after review",
		},
		{
			name: "workspace HEAD changed after review",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				realGit(t, task.Workspace, "commit", "--allow-empty", "-m", "late")
			},
			reason: model.BlockedReasonWorkspaceInvalid,
			want:   "Workspace HEAD changed after review",
		},
		{
			name: "ambiguous PR association",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				seedPRs(t, root,
					pullRequest(1, "external/project", "First"),
					pullRequest(2, "external/project", "Second"))
			},
			reason: model.BlockedReasonRemoteConflict,
			want:   "Ambiguous PR association; reconcile before publication",
		},
		{
			name: "branch owned by another task",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"),
					"update-ref", "refs/heads/octomus/work", task.SourceRevision)
				seedPRs(t, root, pullRequest(1, "fixture/project", "Earlier work.\n<!-- octomus:task:other-task -->"))
			},
			reason: model.BlockedReasonRemoteConflict,
			want:   "Branch is already associated with another task",
		},
		{
			name: "output does not contain the recorded source",
			adjust: func(t *testing.T, root string, task *model.Task, commit string) {
				realGit(t, task.Workspace, "checkout", "--detach", task.SourceRevision)
				realGit(t, task.Workspace, "commit", "--allow-empty", "-m", "side")
				task.SourceRevision = realGit(t, task.Workspace, "rev-parse", "HEAD")
				realGit(t, task.Workspace, "checkout", "--detach", commit)
			},
			reason: model.BlockedReasonRemoteConflict,
			want:   "Reviewed output does not contain the recorded source; reconcile the branch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, root := fixtureRoot(t)
			c.VerificationCommands = []string{"make test"}
			ctx := context.Background()
			task, commit := publishableTask(t, c, root, "task-gated")
			tc.adjust(t, root, &task, commit)
			remote := filepath.Join(root, "remote.git")
			refs := realGit(t, root, "--git-dir", remote, "for-each-ref")
			_, err := git.Publish(ctx, task)
			if err == nil {
				t.Fatal("publication past a failed gate must be refused")
			}
			if reason := model.BlockedReasonFromError(err); reason != tc.reason {
				t.Fatalf("reason = %v; want %v (%q)", reason, tc.reason, err)
			}
			if !strings.HasPrefix(err.Error(), tc.want+": ") {
				t.Fatalf("refusal = %q; want %q", err, tc.want)
			}
			if after := realGit(t, root, "--git-dir", remote, "for-each-ref"); after != refs {
				t.Fatalf("remote refs = %q; want them unchanged from %q", after, refs)
			}
			if data, err := os.ReadFile(filepath.Join(root, "publications.jsonl")); err == nil &&
				strings.Contains(string(data), `"action"`) {
				t.Fatalf("publications = %s; want no writes", data)
			}
		})
	}
}

func TestFixturePublishNeverRecursesIntoSubmodules(t *testing.T) {
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	sub := filepath.Join(root, "sub.git")
	realGit(t, root, "init", "--bare", "-b", "main", sub)
	seed := filepath.Join(root, "sub-seed")
	realGit(t, root, "init", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "lib.txt"), "library\n")
	realGit(t, seed, "add", ".")
	realGit(t, seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-m", "Library")
	realGit(t, seed, "push", sub, "main")
	realGit(t, c.Repository, "-c", "protocol.file.allow=always", "submodule", "add", sub, "sub")
	realGit(t, c.Repository, "commit", "-m", "Add submodule")
	realGit(t, c.Repository, "push", "origin", "main")
	task, _ := publishableTask(t, c, root, "task-submodule")
	realGit(t, task.Workspace, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
	writeFile(t, filepath.Join(task.Workspace, "sub", "lib.txt"), "changed locally\n")
	realGit(t, filepath.Join(task.Workspace, "sub"), "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com",
		"commit", "-am", "Unpushed library change")
	if _, err := git.Snapshot(ctx, c, task.Workspace, "Move the submodule"); model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
		t.Fatalf("snapshot of a moved submodule = %v; want it refused as unreviewed content", err)
	}
	realGit(t, task.Workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-m", "Move the submodule")
	commit := realGit(t, task.Workspace, "rev-parse", "HEAD")
	task.OutputCommit = strptr(commit)
	task.Reviews[0].Revision = commit
	task.Verification[0].Revision = commit
	global := filepath.Join(root, "global.gitconfig")
	writeFile(t, global, "[submodule]\n\trecurse = true\n[push]\n\trecurseSubmodules = on-demand\n[protocol \"file\"]\n\tallow = always\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	if _, err := git.Publish(ctx, task); err != nil {
		t.Fatalf("publication with ambient submodule recursion: %v", err)
	}
	if remoteHead := realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"),
		"rev-parse", "refs/heads/octomus/work"); remoteHead != commit {
		t.Fatalf("remote branch = %s; want the reviewed commit %s", remoteHead, commit)
	}
	if refs := realGit(t, root, "--git-dir", sub, "for-each-ref", "--format=%(refname) %(objectname)"); refs != "refs/heads/main "+realGit(t, seed, "rev-parse", "HEAD") {
		t.Fatalf("submodule remote refs = %q; publication must not push to it", refs)
	}
}

func TestPublishUncertainWrapsCauseOnce(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	task, _ := publishableTask(t, c, root, "task-uncertain")
	task.Config.Repository = t.TempDir()
	_, err := git.Publish(context.Background(), task)
	if err == nil {
		t.Fatal("publication from a broken checkout must fail")
	}
	if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonPublicationUncertain {
		t.Fatalf("reason = %v; want publication_uncertain", reason)
	}
	sentence := model.BlockedReasonPublicationUncertain.Error()
	if count := strings.Count(err.Error(), sentence); count != 1 {
		t.Fatalf("error states the reason %d times; want once: %q", count, err)
	}
	if !strings.HasPrefix(err.Error(), sentence+": ") || !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("error = %q; want the reason followed by the git failure", err)
	}
}

func TestPublicationChecksEveryIdentityFieldAndClosedReconciliation(t *testing.T) {
	t.Parallel()
	c := testConfig()
	commit := strings.Repeat("a", 40)
	task := publicationTask(c, "ws", commit, commit, "task-identity")
	pr := model.PullRequest{
		Number: 1, Title: "x", Branch: task.Branch, Head: commit, Base: "main",
		URL:  "https://github.com/fixture/project/pull/1",
		Body: "<!-- octomus:task:" + task.ID + " -->", State: "open",
		Owned: true, HeadRepository: "fixture/project", BaseRepository: "fixture/project",
	}
	if err := git.ValidatePublication(task, pr, true, false); err != nil {
		t.Fatalf("matching publication rejected: %v", err)
	}
	if err := git.ValidatePublication(task, pr, false, false); err == nil {
		t.Fatal("publication without the task marker must fail")
	}
	mismatch := func(mutate func(*model.PullRequest)) {
		p := pr
		mutate(&p)
		if err := git.ValidatePublication(task, p, true, false); err == nil {
			t.Fatalf("mismatched publication accepted: %+v", p)
		}
	}
	mismatch(func(p *model.PullRequest) { p.Head = "mismatch" })
	mismatch(func(p *model.PullRequest) { p.Branch = "mismatch" })
	mismatch(func(p *model.PullRequest) { p.Base = "mismatch" })
	mismatch(func(p *model.PullRequest) { p.HeadRepository = "mismatch" })
	mismatch(func(p *model.PullRequest) { p.BaseRepository = "mismatch" })
	mismatch(func(p *model.PullRequest) { p.Owned = false })
	for _, state := range []string{"closed", "merged"} {
		p := pr
		p.State = state
		if err := git.ValidatePublication(task, p, true, false); err == nil {
			t.Fatalf("%s publication accepted outside reconciliation", state)
		}
		if err := git.ValidatePublication(task, p, true, true); err != nil {
			t.Fatalf("%s publication rejected under reconciliation: %v", state, err)
		}
	}
}

func TestFixtureGhChildCleanup(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("requires /proc")
	}
	writeFile(t, filepath.Join(root, "reconcile-delay"), "30")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := git.OpenPrInventory(ctx, c)
		done <- err
	}()
	logPath := filepath.Join(root, "reconcile-processes.jsonl")
	var entry struct {
		Pid      int `json:"pid"`
		ChildPid int `json:"child_pid"`
	}
	if !testutil.WaitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return false
		}
		line, _, _ := strings.Cut(string(data), "\n")
		return json.Unmarshal([]byte(strings.TrimSpace(line)), &entry) == nil
	}) {
		t.Fatal("the fixture peer never spawned its delayed child")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled inventory call must fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fixture peer call did not return after cancellation")
	}
	child := fmt.Sprint(entry.ChildPid)
	if !testutil.WaitUntil(5*time.Second, func() bool { return testutil.ProcessGone(child) }) {
		t.Fatal("the fixture peer's descendant survived cancellation")
	}
	if !testutil.WaitUntil(5*time.Second, func() bool {
		_, err := os.Stat("/proc/" + fmt.Sprint(entry.Pid))
		return errors.Is(err, os.ErrNotExist)
	}) {
		t.Fatal("the fixture peer leader was not reaped")
	}
}
