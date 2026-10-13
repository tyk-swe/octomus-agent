// A gitlink a runner committed itself: the staged-only refusal inside Snapshot never sees it, so the change set
// the revision carries has to be refused against the comparison base instead.

package git_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func cloneFixtureWorkspace(t *testing.T, c config.Config, revision string) string {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := git.CloneAt(context.Background(), c, workspace, revision); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// commitNestedRepository commits a nested repository at name and commits the work tree, as a runner that owns its
// own commits does: nothing is left staged for the snapshot to inspect.
func commitNestedRepository(t *testing.T, c config.Config, workspace, name string) {
	t.Helper()
	nested := filepath.Join(workspace, filepath.FromSlash(name))
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit(t, nested, "init", "-b", "main")
	realGit(t, nested, "config", "user.name", "Fixture")
	realGit(t, nested, "config", "user.email", "fixture@example.com")
	writeFile(t, filepath.Join(nested, "nested.txt"), "nested\n")
	realGit(t, nested, "add", ".")
	realGit(t, nested, "commit", "-m", "Nested repository")
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
	commitAll(t, c, workspace, "Runner commit")
}

func commitAll(t *testing.T, c config.Config, workspace, message string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := git.WorkGit(ctx, c, workspace, []string{"add", "--all"}); err != nil {
		t.Fatal(err)
	}
	if _, err := git.WorkGit(ctx, c, workspace, []string{"commit", "-m", message}); err != nil {
		t.Fatal(err)
	}
	head, err := git.WorkGit(ctx, c, workspace, []string{"rev-parse", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	return head
}

// A nested repository a runner committed itself leaves nothing staged, so the snapshot would hand its gitlink to
// review, verification and publication. The range refusal must reject it and name the path.
func TestCommittedNestedRepositoryIsRefused(t *testing.T) {
	t.Parallel()
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	base, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := cloneFixtureWorkspace(t, c, base)
	commitNestedRepository(t, c, workspace, "vendor/module")
	revision, err := git.WorkGit(ctx, c, workspace, []string{"rev-parse", "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if revision == base {
		t.Fatal("the runner's commit did not advance the workspace")
	}
	if _, err := git.Snapshot(ctx, c, workspace, "Deliver the feature"); err != nil {
		t.Fatalf("the snapshot had nothing staged to refuse: %v", err)
	}
	refusal := git.RefuseCommittedGitlinks(ctx, c, workspace, base, revision)
	if refusal == nil {
		t.Fatal("a nested repository committed by the runner was not refused")
	}
	if !errors.Is(refusal, model.BlockedWorkspaceInvalid) {
		t.Fatalf("refusal reason = %v; want workspace_invalid", refusal)
	}
	if reported := refusedPath(t, refusal); reported != "vendor/module" {
		t.Fatalf("reported path = %q; want the committed gitlink's path", reported)
	}
}

// A submodule the comparison base already carries stays allowed while the reviewed range leaves it alone, and a
// commit that retargets it is refused like a staged entry.
func TestCommittedGitlinkRangeFollowsTheBase(t *testing.T) {
	t.Parallel()
	c, _ := fixtureRoot(t)
	ctx := context.Background()
	// The base itself carries a submodule: the trusted checkout publishes one.
	nested := filepath.Join(c.Repository, "vendor", "module")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	realGit(t, nested, "init", "-b", "main")
	realGit(t, nested, "config", "user.name", "Fixture")
	realGit(t, nested, "config", "user.email", "fixture@example.com")
	writeFile(t, filepath.Join(nested, "nested.txt"), "nested\n")
	realGit(t, nested, "add", ".")
	realGit(t, nested, "commit", "-m", "Nested repository")
	realGit(t, c.Repository, "add", "--all")
	realGit(t, c.Repository, "commit", "-m", "Add the vendor submodule")
	realGit(t, c.Repository, "push", "origin", "main")
	base, err := git.Git(ctx, c, c.Repository, []string{"rev-parse", "main"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := cloneFixtureWorkspace(t, c, base)
	writeFile(t, filepath.Join(workspace, "feature.txt"), "fixed\n")
	untouched := commitAll(t, c, workspace, "Unrelated change")
	if err := git.RefuseCommittedGitlinks(ctx, c, workspace, base, untouched); err != nil {
		t.Fatalf("an untouched submodule in the base was refused: %v", err)
	}
	other, err := git.WorkGit(ctx, c, workspace, []string{"rev-parse", base})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := git.WorkGit(ctx, c, workspace, []string{
		"update-index", "--add", "--cacheinfo", "160000," + other + ",vendor/module",
	}); err != nil {
		t.Fatal(err)
	}
	retargeted := commitAll(t, c, workspace, "Retarget the submodule")
	if err := git.RefuseCommittedGitlinks(ctx, c, workspace, base, retargeted); err == nil {
		t.Fatal("a retargeted submodule in the reviewed range was not refused")
	}
	if err := git.RefuseCommittedGitlinks(ctx, c, workspace, base, base); err != nil {
		t.Fatalf("an empty range must be accepted: %v", err)
	}
	if err := git.RefuseCommittedGitlinks(ctx, c, workspace, "", retargeted); err != nil {
		t.Fatalf("a base-less check must accept the range: %v", err)
	}
}
