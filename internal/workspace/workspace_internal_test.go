// Permission repair stays inside the tree and never follows symlinks.

package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestMakeDirsWritableSymlinks(t *testing.T) {
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
