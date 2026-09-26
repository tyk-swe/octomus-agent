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
)

// realGit runs the host git binary directly: fixture setup must not flow
// through the wrapped fixture command. It still gets the service's child
// environment, so Git variables a hook exports to the test run (GIT_DIR,
// GIT_INDEX_FILE) cannot redirect fixture setup into another repository.
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

func processGone(pid string) bool {
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] == "Z"
}

func waitUntil(timeout time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ready() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// initRepo creates a real repository with an initial commit on main.
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

// fixtureRoot replicates tests/e2e.py setup(): fixture bin wrappers on PATH, a
// real checkout whose origin is the local bare remote, and OCTOMUS_FIXTURE.
func fixtureRoot(t *testing.T) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "gh"} {
		src := filepath.Join("..", "..", "tests", "fixtures", name+".py")
		dst := filepath.Join(bin, name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
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
	t.Setenv("OCTOMUS_FIXTURE", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := testConfig()
	c.Repository = checkout
	c.GitHubRepo = "fixture/project"
	return c, root
}

// envSecretName/Value are a clearly synthetic secret-bearing environment
// pair: the name matches the scrubber's API_KEY name rule and the value is
// long enough to be collected, while matching no token pattern on its own.
const (
	envSecretName  = "OCTOMUS_FIXTURE_API_KEY"
	envSecretValue = "fixture-env-secret-0123456789"
)

// TestMain installs the env-secret pair at process start. The store's
// scrubber freezes os.Environ on its first call, and any failing-command
// error path can trigger that freeze long before a publishing test runs —
// so the pair must be present from the start, exactly as operator secrets
// are in the service's real environment.
func TestMain(m *testing.M) {
	if err := os.Setenv(envSecretName, envSecretValue); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func strptr(s string) *string { return &s }

// deref renders an optional revision for failure messages.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// Real commits establish true/false ancestry; command failures must not
// read as a false predicate.
func TestGitAncestryIsAPredicateAndCommandErrorsFailClosed(t *testing.T) {
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
	// Exit status 1 is Git's documented "not an ancestor" answer: false, not error.
	if ok, err := git.IsAncestor(ctx, c, repository, second, first); err != nil || ok {
		t.Fatalf("reversed predicate = %v, %v; want false", ok, err)
	}
	// A missing object is a real command failure (exit 128) and must not read
	// as a false ancestry predicate.
	if _, err := git.IsAncestor(ctx, c, repository, strings.Repeat("0", 40), second); err == nil {
		t.Fatal("a missing object must be an error, not a false predicate")
	}
	// A failed rev-parse cannot be an empty revision either.
	if _, err := git.Git(ctx, c, repository, []string{"rev-parse", "refs/heads/missing"}); err == nil {
		t.Fatal("a failed rev-parse must be an error")
	}
}

// TestCloneAtCreatesAnIndependentCheckout covers clone identity: the workspace
// lands detached at the requested revision with the trusted origin, the
// deterministic committer identity, and the .octomus exclude rule.
func TestCloneAtCreatesAnIndependentCheckout(t *testing.T) {
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
	// The clone's origin must carry the trusted URL of the configured checkout,
	// not a local path that would make the workspace itself a remote.
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
	if exclude, err := os.ReadFile(filepath.Join(workspace, ".git/info/exclude")); err != nil ||
		!strings.Contains(string(exclude), "/.octomus/") {
		t.Fatalf("exclude = %q, %v", exclude, err)
	}
	// An existing destination is never clobbered; recovery must inspect it.
	if err := git.CloneAt(ctx, c, workspace, revision); err == nil {
		t.Fatal("cloning over an existing workspace must fail")
	}
}

// TestRemoteValidationAndRevisionLookup covers the configured-remote checks
// and ls-remote revision reads against the fixture peer.
func TestRemoteValidationAndRevisionLookup(t *testing.T) {
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

// TestValidateRemoteForms pins which origin URLs name the configured GitHub
// repository. Task clones copy the origin into their own configuration and
// publication pushes to it, so a URL carrying credentials, another host or
// transport, or another path must never pass. It runs real Git with no global
// or system configuration, so no url.insteadOf rewrite changes what origin
// reports, and a stub gh that answers only the auth check.
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
		want   string // "" accepts
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
	// A valid origin still needs gh authenticated against github.com.
	realGit(t, repo, "remote", "set-url", "origin", "https://github.com/fixture/project.git")
	writeFile(t, filepath.Join(bin, "gh.unauthenticated"), "")
	if err := git.ValidateRemote(ctx, c); err == nil {
		t.Fatal("an unauthenticated gh must fail remote validation")
	}
}

// TestRemoteRevisionIgnoresTailMatchingRefs: ls-remote patterns also match the
// tail of longer ref names, so a branch named `a/refs/heads/main` answers the
// query for `refs/heads/main` too, and sorts first. Only the exact ref may
// resolve: a decoy must neither replace the real head nor make an absent
// branch look present.
func TestRemoteRevisionIgnoresTailMatchingRefs(t *testing.T) {
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

// TestCleanlinessAndSnapshot covers `git status --porcelain` cleanliness, HEAD
// comparison, and staged snapshot commits in a real workspace.
func TestCleanlinessAndSnapshot(t *testing.T) {
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
	// A second snapshot with nothing staged keeps HEAD unchanged.
	if again, err := git.Snapshot(ctx, c, workspace, "No change"); err != nil || again != commit {
		t.Fatalf("unchanged snapshot = %s, %v; want %s", again, err, commit)
	}
}

// TestParseInventory exercises the paginated inventory contract: sorting,
// identical duplicates tolerated, and hard failures for conflicting entries,
// missing identity, foreign repositories, unknown states and empty replies.
func TestParseInventory(t *testing.T) {
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

// publicationTask builds the recorded task state publication expects: a clean
// review and passing verification at the output commit.
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

// publishableTask clones a real workspace at the fixture main head, lands a
// reviewed change and returns the checkpointed publication task plus its
// output commit.
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

// TestFixturePublishCreatesPullRequest drives the whole publication sequence
// through the fixture peers: remote validation, ancestry, the leased push, and
// the `gh pr create` whose URL is validated rather than trusted.
func TestFixturePublishCreatesPullRequest(t *testing.T) {
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
	// The pushed head on the fixture remote is the reviewed commit.
	remoteHead := realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"),
		"rev-parse", "refs/heads/octomus/work")
	if remoteHead != commit {
		t.Fatalf("remote branch = %s; want the reviewed commit %s", remoteHead, commit)
	}
	// The default branch never moved.
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
	// The peer recorded the exact --title argument and --body-file payload.
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
	// A second publish of the same task reconciles as already delivered.
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

// TestFixtureFollowUpAppendsComment covers the owned-PR follow-up path: the
// evidence lands as an append-only comment and the description is untouched.
func TestFixtureFollowUpAppendsComment(t *testing.T) {
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	checkout := c.Repository
	// Earlier Octomus work on the owned branch, with its own marker.
	realGit(t, checkout, "checkout", "-b", "octomus/existing")
	writeFile(t, filepath.Join(checkout, "earlier.txt"), "Preserve the earlier improvement.\n")
	realGit(t, checkout, "add", ".")
	realGit(t, checkout, "commit", "-m", "Earlier Octomus work")
	realGit(t, checkout, "push", "origin", "octomus/existing")
	earlier := realGit(t, checkout, "rev-parse", "HEAD")
	realGit(t, checkout, "checkout", "main")
	writeFile(t, filepath.Join(root, "prs.json"), fmt.Sprintf(`[{"number":42,"title":"An existing improvement","body":"Existing context.\n<!-- octomus:task:earlier -->","head":{"ref":"octomus/existing","sha":%q,"repo":{"full_name":"fixture/project"}},"base":{"ref":"main","repo":{"full_name":"fixture/project"}},"html_url":"https://github.com/fixture/project/pull/42","state":"open","merged_at":null,"additions":2000,"deletions":0,"created_at":"2026-08-01T00:00:00Z"}]`, earlier))
	// The follow-up task builds on the delivered head of the existing branch.
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
	// The maintainer-editable description is never rewritten by a follow-up.
	if body, _ := saved[0]["body"].(string); !strings.HasPrefix(body, "Existing context.") {
		t.Fatalf("body = %q; want the original description preserved", body)
	}
}

// TestFixturePublishScrubsSecretsForPublicDelivery: token-patterned and
// environment-secret values in the proposal, the verification command and the
// executor summary are scrubbed from the exact title argument and body
// payload the peer receives, while harmless text and the delivery identity
// survive intact.
func TestFixturePublishScrubsSecretsForPublicDelivery(t *testing.T) {
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
	// Replaying the delivery adds nothing even though the task's canonical
	// text still carries the secret-shaped values.
	again, err := git.Publish(ctx, task)
	if err != nil || again.Number != 1 {
		t.Fatalf("re-publication = %+v, %v; want the existing PR", again, err)
	}
	entries, _ := os.ReadFile(filepath.Join(root, "publications.jsonl"))
	if count := strings.Count(string(entries), `"action"`); count != 1 {
		t.Fatalf("re-publication wrote %d actions; want idempotent delivery", count)
	}
}

// TestFixtureFollowUpScrubsCommentMetadata: an owned-PR follow-up posts the
// scrubbed record as an append-only comment; the maintainer-visible
// description is preserved byte-for-byte.
func TestFixtureFollowUpScrubsCommentMetadata(t *testing.T) {
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	checkout := c.Repository
	// Earlier Octomus work on the owned branch, with its own marker.
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
	// Replaying the follow-up appends no second comment.
	again, err := git.Publish(ctx, task)
	if err != nil || again.Number != 42 {
		t.Fatalf("replayed follow-up = %+v, %v; want PR #42", again, err)
	}
	entries, _ := os.ReadFile(filepath.Join(root, "publications.jsonl"))
	if count := strings.Count(string(entries), `"action"`); count != 1 {
		t.Fatalf("replayed follow-up wrote %d actions; want one", count)
	}
}

// TestFixturePublishLongBodyKeepsDeliveryIdentity: a body far beyond the
// bounded operator-message formatter's 16,384-character cap is delivered
// whole — publication never routes through that truncating formatter.
func TestFixturePublishLongBodyKeepsDeliveryIdentity(t *testing.T) {
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

// TestPublishRefusesUnsafeMetadataBeforeAnyWrite: metadata that cannot be
// represented safely is refused before the first outbound write — no push, no
// pull request, no comment — and the operator-facing refusal never echoes the
// rejected private text.
func TestPublishRefusesUnsafeMetadataBeforeAnyWrite(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		adjust func(*model.Task)
		want   string
		echo   string // planted private text that must never surface in the error
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
			// A task id colliding with the token policy destroys the marker
			// under scrubbing: delivery is refused rather than published
			// without its identity.
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
			// Nothing reached the remote: no pushed branch, no PR, no comment.
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

// TestPublishRejectsStaleBase pins the remote-movement guard: when the default
// branch advanced past the recorded source, publication refuses with
// StaleBase rather than pushing.
func TestPublishRejectsStaleBase(t *testing.T) {
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
	// The remote default branch moves after the task recorded its base.
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
	// Nothing reached the remote.
	if rev, err := git.RemoteRevision(ctx, c, "octomus/work"); err != nil || rev != nil {
		t.Fatalf("remote branch = %v, %v; want nothing pushed", rev, err)
	}
}

// TestFixturePublishListsLatestVerificationOnly: a command that failed and was
// later re-run successfully at the same reviewed commit is described by its
// latest result only — the one that gated publication — so the public text
// never shows both outcomes for one command.
func TestFixturePublishListsLatestVerificationOnly(t *testing.T) {
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

// TestPublishGatesOnTheLatestVerificationPerCommand: only the most recent
// record of each configured command gates publication, and it must be a
// success at the reviewed commit.
func TestPublishGatesOnTheLatestVerificationPerCommand(t *testing.T) {
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

// TestPublishGatesRefuseBeforeAnyWrite: each safety gate in front of the push
// refuses on its own, with its typed reason, and leaves the remote exactly as it
// was: no ref moved, no pull request created, no comment posted. Unreviewed,
// moved or foreign work must never reach the remote. The verification gate has
// its own table in TestPublishGatesOnTheLatestVerificationPerCommand.
func TestPublishGatesRefuseBeforeAnyWrite(t *testing.T) {
	// seedPRs writes pull requests the gh peer serves for the task's branch.
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
			// An earlier clean review of the output does not cover a later
			// round that reviewed something else.
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
			// Even a prefix that admits every branch never admits the default
			// branch itself.
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
			// Two pull requests from the branch cannot be told apart; the
			// foreign head repository keeps the peer from resolving their heads.
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
			// An owned, open pull request on the branch carries another task's
			// marker: this task must not publish over it.
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
			// The reviewed output descends from the default head but not from
			// the recorded source revision.
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
			// Nothing reached the remote: no ref moved, no PR, no comment.
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

// TestFixturePublishNeverRecursesIntoSubmodules: publication pushes only the
// owned branch even when the operator's global Git configuration enables
// submodule recursion and the reviewed commit moves a submodule to a commit
// its own remote has never seen.
func TestFixturePublishNeverRecursesIntoSubmodules(t *testing.T) {
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	// A submodule remote with one commit on main, added to the fixture's main.
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
	// The reviewed change also moves the submodule to a local, unpushed commit.
	realGit(t, task.Workspace, "-c", "protocol.file.allow=always", "submodule", "update", "--init")
	writeFile(t, filepath.Join(task.Workspace, "sub", "lib.txt"), "changed locally\n")
	realGit(t, filepath.Join(task.Workspace, "sub"), "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com",
		"commit", "-am", "Unpushed library change")
	commit, err := git.Snapshot(ctx, c, task.Workspace, "Move the submodule")
	if err != nil {
		t.Fatal(err)
	}
	task.OutputCommit = strptr(commit)
	task.Reviews[0].Revision = commit
	task.Verification[0].Revision = commit
	// Ambient operator configuration that would otherwise recurse on push.
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

// TestPublishUncertainWrapsCauseOnce: an untyped publication failure is
// reported as PublicationUncertain with the reason sentence stated once,
// followed by the underlying cause.
func TestPublishUncertainWrapsCauseOnce(t *testing.T) {
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	task, _ := publishableTask(t, c, root, "task-uncertain")
	// The fixture still answers `remote get-url` and `gh auth status`, so the
	// first failure is ls-remote in a checkout that is not a repository.
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

// TestPublicationChecksEveryIdentityFieldAndClosedReconciliation: every
// identity field (repositories, ownership, branch, base, reviewed head, marker)
// must match, and closed/merged states pass only explicit reconciliation.
func TestPublicationChecksEveryIdentityFieldAndClosedReconciliation(t *testing.T) {
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
	// A missing task marker fails publication even when every field matches.
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

// TestFixtureGhChildCleanup exercises fixture-peer cleanup: the gh fixture can
// spawn a delayed child, and cancelling mid-call must terminate the peer's own
// descendant within bounded time rather than leaking it.
func TestFixtureGhChildCleanup(t *testing.T) {
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
	// The file appears before its line lands, so wait for a decodable record.
	if !waitUntil(5*time.Second, func() bool {
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
	if !waitUntil(5*time.Second, func() bool { return processGone(child) }) {
		t.Fatal("the fixture peer's descendant survived cancellation")
	}
	// The peer's leader was waited for: its proc entry is gone entirely.
	if !waitUntil(5*time.Second, func() bool {
		_, err := os.Stat("/proc/" + fmt.Sprint(entry.Pid))
		return errors.Is(err, os.ErrNotExist)
	}) {
		t.Fatal("the fixture peer leader was not reaped")
	}
}
