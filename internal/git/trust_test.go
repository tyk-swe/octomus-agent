// Orchestrator git never executes configuration a work tree controls.

package git_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// hostileTrap returns a directory whose appearance of any file proves that git executed agent-controlled configuration.
func hostileTrap(t *testing.T) (dir string, touch string) {
	t.Helper()
	dir = t.TempDir()
	script := filepath.Join(t.TempDir(), "trap")
	// A trap that fails to start would hide the very execution the test looks for.
	if err := testutil.WriteExecutable(script, fmt.Appendf(nil, "#!/bin/sh\ntouch %q/\"$(basename \"$0\")-$$\"\ncat >/dev/null 2>&1 || true\n", dir)); err != nil {
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

func TestHostileGitEntry(t *testing.T) {
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
