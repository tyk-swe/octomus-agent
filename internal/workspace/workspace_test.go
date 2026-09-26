package workspace_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// TestRemoveOwnedDir pins the housekeeping cleanup contract: only plainly
// named direct children of the owned root may be deleted, and no component —
// including the target itself — may be a symlink.
func TestRemoveOwnedDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(filepath.Join(root, "task-1", "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "task-1", "workspace", "artifact"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An owned direct child is removed entirely.
	if err := workspace.RemoveOwnedDir(root, filepath.Join(root, "task-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "task-1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed child still present: %v", err)
	}
	// A missing child is already gone, not an error.
	if err := workspace.RemoveOwnedDir(root, filepath.Join(root, "task-2")); err != nil {
		t.Fatal(err)
	}
	// A symlink in place of the child refuses cleanup and leaves the target intact.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "task-3")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(root, link); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink cleanup = %v; want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("symlink cleanup must never touch the target")
	}
	// A symlinked ancestor refuses cleanup too, even when the child is real.
	realRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realRoot, "task-4"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkedRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, symlinkedRoot); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(symlinkedRoot, filepath.Join(symlinkedRoot, "task-4")); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("cleanup under a symlinked ancestor = %v; want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(realRoot, "task-4")); err != nil {
		t.Fatal("refused cleanup must leave the real child intact")
	}
	// Names that are not plain children are invalid cleanup paths.
	for _, bad := range []string{".", ".."} {
		if err := workspace.RemoveOwnedDir(root, filepath.Join(root, bad)); err == nil {
			t.Fatalf("cleanup of %q must fail", bad)
		}
	}
	// The filesystem root has no name and no parent; a degenerate "/" root
	// must never reach removal.
	if err := workspace.RemoveOwnedDir("/", "/"); err == nil {
		t.Fatal("cleanup of the filesystem root must fail")
	}
	// A path that is not a direct child of the root is refused.
	deeper := filepath.Join(root, "task-5", "workspace")
	if err := os.MkdirAll(deeper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(root, deeper); err == nil {
		t.Fatal("cleanup of a nested descendant must fail")
	}
	if _, err := os.Stat(deeper); err != nil {
		t.Fatal("refused cleanup must leave the path intact")
	}
}

// TestRemoveOwnedDirRemovesReadOnlyTrees pins that toolchain output such as a
// Go module cache inside a workspace — directories without write, read or
// search permission — never makes housekeeping fail on every pass. Root ignores
// those modes, so the test needs an unprivileged user.
func TestRemoveOwnedDirRemovesReadOnlyTrees(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := filepath.Join(t.TempDir(), "tasks")
	tree := filepath.Join(root, "task-1")
	for _, dir := range []string{filepath.Join(tree, "mod", "pkg"), filepath.Join(tree, "locked", "inner")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "mod", "pkg", "f.go"), []byte("package pkg\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "locked", "inner", "f.go"), []byte("package inner\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	// Children before parents, so every mode can be applied.
	modes := []struct {
		path string
		mode fs.FileMode
	}{
		{filepath.Join(tree, "mod", "pkg"), 0o555},
		{filepath.Join(tree, "mod"), 0o555},
		{filepath.Join(tree, "locked", "inner"), 0o555},
		{filepath.Join(tree, "locked"), 0o000},
		{tree, 0o555},
	}
	for _, m := range modes {
		if err := os.Chmod(m.path, m.mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for i := len(modes) - 1; i >= 0; i-- {
			_ = os.Chmod(modes[i].path, 0o755)
		}
	})
	if err := workspace.RemoveOwnedDir(root, tree); err != nil {
		t.Fatalf("read-only owned tree cleanup = %v; want removal", err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only owned tree still present: %v", err)
	}
}

func TestRemoveOwnedDirRejectsNoncanonicalPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, dir := range []string{filepath.Join(outside, "child"), filepath.Join(outside, "victim"), filepath.Join(root, "victim")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "child"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/link/../victim", "/./victim", "//victim", "/victim/"} {
		t.Run(suffix, func(t *testing.T) {
			// Do not use Join: it would clean away the traversal under test.
			if err := workspace.RemoveOwnedDir(root, root+suffix); err == nil {
				t.Fatal("noncanonical cleanup path must be refused")
			}
			for _, dir := range []string{root, outside} {
				if _, err := os.Stat(filepath.Join(dir, "victim")); err != nil {
					t.Fatalf("refused cleanup must preserve victim in %s: %v", dir, err)
				}
			}
		})
	}
}

// TestDirectorySize pins housekeeping's accounting: regular file sizes sum,
// symlinks contribute nothing, and a missing tree measures as zero.
func TestDirectorySize(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "1234")
	write("sub/b.txt", "123456")
	target := filepath.Join(t.TempDir(), "target.txt")
	if err := os.WriteFile(target, []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	size, err := workspace.DirectorySize(root)
	if err != nil {
		t.Fatal(err)
	}
	if size != 10 {
		t.Fatalf("size = %d; want 10 (symlink target excluded)", size)
	}
	if size, err := workspace.DirectorySize(filepath.Join(root, "missing")); err != nil || size != 0 {
		t.Fatalf("missing tree = %d, %v; want zero", size, err)
	}
}

// TestInitialized pins the resumable-workspace predicate: a recorded session,
// a comparison base and a real checkout must all be present.
func TestInitialized(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	session := "session-1"
	task := model.Task{Workspace: checkout, ComparisonBase: "abc", ExecutionSession: &session}
	if !workspace.Initialized(task) {
		t.Fatal("a task with session, base and checkout must be initialized")
	}
	noSession := task
	noSession.ExecutionSession = nil
	if workspace.Initialized(noSession) {
		t.Fatal("a task without a recorded session is not initialized")
	}
	noBase := task
	noBase.ComparisonBase = ""
	if workspace.Initialized(noBase) {
		t.Fatal("a task without a comparison base is not initialized")
	}
	noCheckout := task
	noCheckout.Workspace = filepath.Join(root, "missing")
	if workspace.Initialized(noCheckout) {
		t.Fatal("a task without a real checkout is not initialized")
	}
}
