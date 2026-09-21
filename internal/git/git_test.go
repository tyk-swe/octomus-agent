package git_test

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
	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

// realGit runs the host git binary directly: fixture setup must not flow
// through the wrapped fixture command.
func realGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", args...)
	cmd.Dir = cwd
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

func strptr(s string) *string { return &s }

// TestGitAncestryIsAPredicateAndCommandErrorsFailClosed ports the hardening.rs
// case: real commits, true/false ancestry, and command failures that must not
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
	if !strings.Contains(string(prs), "<!-- octomus:task:task-1 -->") {
		t.Fatalf("prs.json = %s; want the task marker in the body", prs)
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
