package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestMakeDirsWritableSkipsSymlinkTargets pins the permission repair that lets
// cleanup remove read-only trees: every real directory in the owned tree,
// including ones that could not be listed or searched, gains owner access,
// while symlinked directories — outside the root or beside the tree inside
// it — keep their modes. It runs as any user, unlike the unprivileged
// removal test, because root ignores the modes it changes.
func TestMakeDirsWritableSkipsSymlinkTargets(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tasks")
	tree := filepath.Join(root, "task-1")
	sibling := filepath.Join(root, "task-2")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{
		filepath.Join(tree, "mod", "pkg"),
		filepath.Join(tree, "locked", "inner"),
		sibling,
		outside,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "mod", "pkg", "f.go"), []byte("package pkg\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "mod", "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "task-2"), filepath.Join(tree, "mod", "sibling-link")); err != nil {
		t.Fatal(err)
	}
	// Children before parents, so every mode can be applied.
	modes := []struct {
		path string
		mode fs.FileMode
	}{
		{filepath.Join(tree, "mod", "pkg"), 0o555},
		{filepath.Join(tree, "mod"), 0o500},
		{filepath.Join(tree, "locked", "inner"), 0o555},
		{filepath.Join(tree, "locked"), 0o000},
		{tree, 0o555},
		{sibling, 0o555},
		{outside, 0o555},
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

	makeDirsWritable(root, "task-1")

	for _, dir := range []string{
		tree,
		filepath.Join(tree, "mod"),
		filepath.Join(tree, "mod", "pkg"),
		filepath.Join(tree, "locked"),
		filepath.Join(tree, "locked", "inner"),
	} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o700 != 0o700 {
			t.Fatalf("%s mode = %v; want owner rwx", dir, info.Mode().Perm())
		}
	}
	for _, dir := range []string{outside, sibling} {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o555 {
			t.Fatalf("symlink target %s mode = %v; want it unchanged at 0555", dir, info.Mode().Perm())
		}
	}

	if err := RemoveOwnedDir(root, tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("owned tree still present: %v", err)
	}
	for _, dir := range []string{outside, sibling} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("removal must never touch symlink target %s: %v", dir, err)
		}
	}
}

// TestMakeDirsWritableReachesAnyDirectoryName pins that the repair reaches
// directories below names that are legal on Linux but not valid io/fs paths —
// bytes that are not UTF-8, backslashes, colons — so a read-only tree under
// such a name cannot keep blocking cleanup.
func TestMakeDirsWritableReachesAnyDirectoryName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tasks")
	tree := filepath.Join(root, "task-1")
	var locked []string
	for _, name := range []string{"latin-\xe9", `back\slash`, "co:lon"} {
		parent := filepath.Join(tree, name)
		child := filepath.Join(parent, "deep")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		// Children before parents, so every mode can be applied.
		locked = append(locked, child, parent)
	}
	for _, dir := range locked {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for i := len(locked) - 1; i >= 0; i-- {
			_ = os.Chmod(locked[i], 0o755)
		}
	})

	makeDirsWritable(root, "task-1")

	for _, dir := range locked {
		info, err := os.Lstat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o700 != 0o700 {
			t.Fatalf("%q mode = %v; want owner rwx", dir, info.Mode().Perm())
		}
	}
}
