package workspace_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
	"golang.org/x/sys/unix"
)

func TestRemoveOwnedDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(filepath.Join(root, "task-1", "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "task-1", "workspace", "artifact"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RemoveOwnedDir(root, filepath.Join(root, "task-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "task-1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed child still present: %v", err)
	}
	if err := workspace.RemoveOwnedDir(root, filepath.Join(root, "task-2")); err != nil {
		t.Fatal(err)
	}
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
	for _, bad := range []string{".", ".."} {
		if err := workspace.RemoveOwnedDir(root, filepath.Join(root, bad)); err == nil {
			t.Fatalf("cleanup of %q must fail", bad)
		}
	}
	if err := workspace.RemoveOwnedDir("/", "/"); err == nil {
		t.Fatal("cleanup of the filesystem root must fail")
	}
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

func TestMeasure(t *testing.T) {
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
	if err := os.Symlink(filepath.Dir(target), filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	usage, err := workspace.Measure(root)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != 10 || len(usage.Unmeasured) != 0 {
		t.Fatalf("usage = %+v; want 10 bytes (symlink targets excluded), all measured", usage)
	}
	if usage, err := workspace.Measure(filepath.Join(root, "missing")); err != nil || usage.Bytes != 0 {
		t.Fatalf("missing tree = %+v, %v; want zero", usage, err)
	}
}

func TestMeasureReportsTooDeepSubtreesWithoutFailing(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tasks", "t1", "workspace")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "shallow.txt"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sandbox nests with relative mkdir and cd; spelled out as one path, this chain is far beyond PATH_MAX.
	fd, err := unix.Open(tree, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range 2100 {
		if err := unix.Mkdirat(fd, "a", 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, "a", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	_ = unix.Close(fd)

	usage, err := workspace.Measure(root)
	if err != nil {
		t.Fatalf("Measure of a too-deep tree = %v; want the reachable bytes", err)
	}
	if usage.Bytes != 5 || len(usage.Unmeasured) != 1 ||
		!strings.HasPrefix(usage.Unmeasured[0], filepath.Join("tasks", "t1", "workspace", "a", "a")+string(filepath.Separator)) {
		t.Fatalf("usage = %d bytes, unmeasured %d entries; want 5 bytes and the deep chain reported once", usage.Bytes, len(usage.Unmeasured))
	}
	if err := workspace.RemoveOwnedDir(filepath.Join(root, "tasks"), filepath.Join(root, "tasks", "t1")); err != nil {
		t.Fatalf("removing a too-deep tree = %v", err)
	}
}

func TestMeasureReportsUnreadableSubtrees(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	workspaceDir := filepath.Join(root, "tasks", "t1", "workspace")
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(workspaceDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("readable.txt", "1234567")
	write("locked/inner/f.txt", "hidden")
	write("nosearch/f.txt", "hidden too")
	locked := []struct {
		path string
		mode fs.FileMode
	}{
		{filepath.Join(workspaceDir, "locked"), 0o000},
		{filepath.Join(workspaceDir, "nosearch"), 0o644},
	}
	for _, dir := range locked {
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, dir := range locked {
			_ = os.Chmod(dir.path, 0o755)
		}
	})
	usage, err := workspace.Measure(root)
	if err != nil {
		t.Fatalf("Measure with unreadable subtrees = %v; want the readable bytes", err)
	}
	if usage.Bytes != 7 {
		t.Fatalf("size = %d; want 7 (the readable file only)", usage.Bytes)
	}
	want := []string{filepath.Join("tasks", "t1", "workspace", "locked"), filepath.Join("tasks", "t1", "workspace", "nosearch")}
	slices.Sort(usage.Unmeasured)
	if !slices.Equal(usage.Unmeasured, want) {
		t.Fatalf("unmeasured = %q; want %q, whose bytes are unknown", usage.Unmeasured, want)
	}
	for _, dir := range locked {
		info, err := os.Lstat(dir.path)
		if err != nil || info.Mode().Perm() != dir.mode {
			t.Fatalf("measurement changed %s: %v, %v; want mode %v", dir.path, info, err, dir.mode)
		}
	}
	for _, dir := range locked {
		if _, err := workspace.Measure(dir.path); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("unreadable measured root %s = %v; want a permission error", dir.path, err)
		}
	}
}

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
