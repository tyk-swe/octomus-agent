package git_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

// hostileTrap returns a directory whose appearance of any file proves that git executed agent-controlled configuration.
func hostileTrap(t *testing.T) (dir string, touch string) {
	t.Helper()
	dir = t.TempDir()
	script := filepath.Join(t.TempDir(), "trap")
	writeFile(t, script, fmt.Sprintf("#!/bin/sh\ntouch %q/\"$(basename \"$0\")-$$\"\ncat >/dev/null 2>&1 || true\n", dir))
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, script
}

func assertTrapUntouched(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("git executed agent-controlled configuration: %v", entries)
	}
}

// plantHostileGitDir turns path into a repository whose configuration and hooks would run the trap for every
// orchestrator git operation that trusted it.
func plantHostileGitDir(t *testing.T, path, trap string) {
	t.Helper()
	realGit(t, path, "init", "-q")
	hooks := filepath.Join(path, ".git", "hooks")
	for _, hook := range []string{"pre-commit", "post-commit", "pre-push", "reference-transaction", "post-index-change"} {
		if err := os.Symlink(trap, filepath.Join(hooks, hook)); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{
		"core.fsmonitor":                              trap,
		"core.hooksPath":                              hooks,
		"core.sshCommand":                             trap,
		"credential.helper":                           "!" + trap,
		"filter.hostile.clean":                        trap,
		"filter.hostile.smudge":                       trap,
		"filter.hostile.process":                      trap,
		"diff.hostile.textconv":                       trap,
		"url.file:///nonexistent/evil.insteadOf":      "https://github.com/",
		"url.file:///nonexistent/evil2.pushInsteadOf": "https://github.com/",
	} {
		realGit(t, path, "config", key, value)
	}
}

func TestHostileWorkTreeGitEntryIsNeverTrusted(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	c.VerificationCommands = []string{"make test"}
	ctx := context.Background()
	trapDir, trap := hostileTrap(t)
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "data", "tasks", "task-hostile", "workspace")
	if err := git.CloneAt(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(workspace, ".git")); err != nil {
		t.Fatal(err)
	}
	plantHostileGitDir(t, workspace, trap)
	writeFile(t, filepath.Join(workspace, ".gitattributes"), "* filter=hostile diff=hostile\n")
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")

	commit, err := git.Snapshot(ctx, c, workspace, "Deliver the feature")
	if err != nil {
		t.Fatal(err)
	}
	if at, err := git.At(ctx, c, workspace, commit); err != nil || !at {
		t.Fatalf("trusted state after a planted .git directory = %v, %v; want clean at the snapshot", at, err)
	}
	if files := realGit(t, root, "--git-dir", filepath.Join(filepath.Dir(workspace), "repo.git"),
		"ls-tree", "-r", "--name-only", commit); files != ".gitattributes\nREADME.md\nfeature.txt" {
		t.Fatalf("snapshot tree = %q; the work tree's .git entry must never be committed", files)
	}
	if _, err := git.Publish(ctx, publicationTask(c, workspace, commit, source, "task-hostile")); err != nil {
		t.Fatalf("publication with a hostile work-tree .git entry: %v", err)
	}
	if remoteHead := realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"),
		"rev-parse", "refs/heads/octomus/work"); remoteHead != commit {
		t.Fatalf("remote branch = %s; want the reviewed commit pushed to the trusted remote", remoteHead)
	}
	assertTrapUntouched(t, trapDir)
}

func TestWorkTreeGitPointerCannotRedirectTheOrchestrator(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	ctx := context.Background()
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	checkoutHead := realGit(t, c.Repository, "rev-parse", "HEAD")
	for name, redirect := range map[string]func(pointer string){
		"symlink to the trusted checkout": func(pointer string) {
			if err := os.Symlink(filepath.Join(c.Repository, ".git"), pointer); err != nil {
				t.Fatal(err)
			}
		},
		"gitfile to the trusted checkout": func(pointer string) {
			writeFile(t, pointer, "gitdir: "+filepath.Join(c.Repository, ".git")+"\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			workspace := filepath.Join(root, "data", "tasks", name, "workspace")
			if err := git.CloneAt(ctx, c, workspace, source); err != nil {
				t.Fatal(err)
			}
			pointer := filepath.Join(workspace, ".git")
			if err := os.Remove(pointer); err != nil {
				t.Fatal(err)
			}
			redirect(pointer)
			writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
			commit, err := git.Snapshot(ctx, c, workspace, "Deliver the feature")
			if err != nil {
				t.Fatal(err)
			}
			if got := realGit(t, root, "--git-dir", filepath.Join(filepath.Dir(workspace), "repo.git"), "rev-parse", "HEAD"); got != commit {
				t.Fatalf("trusted metadata HEAD = %s; want the snapshot %s", got, commit)
			}
			if got := realGit(t, c.Repository, "rev-parse", "HEAD"); got != checkoutHead {
				t.Fatalf("checkout HEAD moved to %s; a redirected pointer reached the trusted checkout", got)
			}
			if status := realGit(t, c.Repository, "status", "--porcelain"); status != "" {
				t.Fatalf("checkout status = %q; want the trusted checkout untouched", status)
			}
		})
	}
}

func TestNestedRepositoryIsRefusedWithoutRunningItsConfiguration(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	ctx := context.Background()
	trapDir, trap := hostileTrap(t)
	source, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "data", "tasks", "task-nested", "workspace")
	if err := git.CloneAt(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(workspace, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	plantHostileGitDir(t, nested, trap)
	writeFile(t, filepath.Join(nested, "lib.txt"), "library\n")
	realGit(t, nested, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "add", "lib.txt")
	realGit(t, nested, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "library")

	if clean, err := git.At(ctx, c, workspace, source); err != nil || clean {
		t.Fatalf("state with an untracked nested repository = %v, %v; want dirty", clean, err)
	}
	_, err = git.Snapshot(ctx, c, workspace, "Vendor a library")
	if model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
		t.Fatalf("snapshot with a nested repository = %v; want workspace_invalid", err)
	}
	assertTrapUntouched(t, trapDir)
}

func TestRegisteredSubmoduleConfigurationNeverRuns(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	ctx := context.Background()
	trapDir, trap := hostileTrap(t)
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
	source := realGit(t, c.Repository, "rev-parse", "HEAD")
	workspace := filepath.Join(root, "data", "tasks", "task-sub", "workspace")
	if err := git.CloneAt(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(workspace, "sub")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatal(err)
	}
	plantHostileGitDir(t, planted, trap)
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")

	if _, err := git.At(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	commit, err := git.Snapshot(ctx, c, workspace, "Deliver the feature")
	if err != nil && !errors.Is(err, model.BlockedReasonWorkspaceInvalid) {
		t.Fatal(err)
	}
	if err == nil {
		if at, err := git.At(ctx, c, workspace, commit); err != nil || !at {
			t.Fatalf("state after snapshot = %v, %v", at, err)
		}
	}
	assertTrapUntouched(t, trapDir)
}

func TestLegacyInWorkTreeMetadataStillResolves(t *testing.T) {
	t.Parallel()
	c := testConfig()
	ctx := context.Background()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	realGit(t, root, "init", "-q", "-b", "main", workspace)
	writeFile(t, filepath.Join(workspace, "README.md"), "legacy\n")
	realGit(t, workspace, "add", ".")
	realGit(t, workspace, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "commit", "-qm", "legacy")
	head := realGit(t, workspace, "rev-parse", "HEAD")
	if at, err := git.At(ctx, c, workspace, head); err != nil || !at {
		t.Fatalf("legacy clone state = %v, %v; want clean at %s", at, err, head)
	}
	if err := os.WriteFile(filepath.Join(root, "repo.git"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.At(ctx, c, workspace, head); model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
		t.Fatalf("a non-directory repo.git = %v; want workspace_invalid rather than a fallback", err)
	}
}

func TestForkHeadsReachPlanningClonesWithoutNetwork(t *testing.T) {
	t.Parallel()
	c, root := fixtureRoot(t)
	ctx := context.Background()
	remote := filepath.Join(root, "remote.git")
	tree := realGit(t, root, "--git-dir", remote, "rev-parse", "main^{tree}")
	forkHead := realGit(t, root, "--git-dir", remote, "-c", "user.name=Fork", "-c", "user.email=fork@example.com",
		"commit-tree", tree, "-p", "main", "-m", "Fork change")
	realGit(t, root, "--git-dir", remote, "update-ref", "refs/pull/7/head", forkHead)

	missing, err := git.FetchForkHeads(ctx, c, []uint64{7, 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != 8 {
		t.Fatalf("missing = %v; want only the head the remote does not serve", missing)
	}
	source := realGit(t, c.Repository, "rev-parse", "main")
	workspace := filepath.Join(root, "data", "cycles", "c1", "discovery-0", "workspace")
	if err := git.CloneAt(ctx, c, workspace, source); err != nil {
		t.Fatal(err)
	}
	if kind := realGit(t, workspace, "cat-file", "-t", forkHead); kind != "commit" {
		t.Fatalf("fork head in a planning clone = %q; want the commit available by SHA", kind)
	}
	if _, err := git.FetchForkHeads(ctx, c, nil); err != nil {
		t.Fatal(err)
	}
	if refs := realGit(t, c.Repository, "for-each-ref", "refs/octomus/pr/"); refs != "" {
		t.Fatalf("stale fork refs = %q; want them pruned when no longer grounded", refs)
	}
}
