package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMeasureReopenChecksAncestorIdentity(t *testing.T) {
	for _, change := range []string{"unchanged", "directory", "symlink", "file", "removed"} {
		t.Run(change, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "root")
			if err := os.MkdirAll(filepath.Join(root, "owner", "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			dir, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			path := []measuredDir{{name: "owner"}, {name: "sub"}}
			for i, name := range []string{"owner", filepath.Join("owner", "sub")} {
				if err := unix.Stat(filepath.Join(root, name), &path[i].meta); err != nil {
					t.Fatal(err)
				}
			}
			if change != "unchanged" {
				// The open root must never lead the walk into the original tree after it moves outside root,
				// nor into a different directory inserted at the same path while a descendant was being read.
				moved := filepath.Join(base, "moved")
				if err := os.Rename(filepath.Join(root, "owner"), moved); err != nil {
					t.Fatal(err)
				}
				switch change {
				case "directory":
					err = os.MkdirAll(filepath.Join(root, "owner", "sub"), 0o755)
				case "symlink":
					err = os.Symlink(moved, filepath.Join(root, "owner"))
				case "file":
					err = os.WriteFile(filepath.Join(root, "owner"), []byte("replacement"), 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			w := walker{root: dir, unmeasured: map[string]struct{}{}}
			reopened, err := w.reopen(path, "owner")
			if err != nil {
				t.Fatal(err)
			}
			if reopened != nil {
				defer reopened.Close()
			}
			if change == "unchanged" {
				if reopened == nil || len(w.unmeasured) != 0 {
					t.Fatalf("unchanged ancestor = %v, unmeasured %v; want the original directory", reopened, w.unmeasured)
				}
				var meta unix.Stat_t
				if err := unix.Fstat(int(reopened.Fd()), &meta); err != nil {
					t.Fatal(err)
				}
				if meta.Dev != path[1].meta.Dev || meta.Ino != path[1].meta.Ino {
					t.Fatal("reopened the wrong directory")
				}
				return
			}
			if reopened != nil {
				t.Fatal("reopened a changed ancestor")
			}
			_, reported := w.unmeasured["owner"]
			if reported != (change != "removed") {
				t.Fatalf("unmeasured after %s = %v; only a removed subtree may be ignored", change, w.unmeasured)
			}
		})
	}
}

func TestMeasureKeepsParentOpenForLeafSiblings(t *testing.T) {
	for _, leaf := range []bool{true, false} {
		t.Run(map[bool]string{true: "leaf", false: "non-leaf"}[leaf], func(t *testing.T) {
			root := t.TempDir()
			child := filepath.Join(root, "child")
			if err := os.Mkdir(child, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(child, "counted.txt"), []byte("123"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !leaf {
				if err := os.Mkdir(filepath.Join(child, "grandchild"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			dir, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			childDir, err := os.Open(child)
			if err != nil {
				t.Fatal(err)
			}
			w := walker{root: dir, unmeasured: map[string]struct{}{}}
			releases := 0
			if err := w.walk(childDir, "child", []measuredDir{{name: "child"}}, func() { releases++ }); err != nil {
				t.Fatal(err)
			}
			wantReleases := 1
			if leaf {
				wantReleases = 0
			}
			if releases != wantReleases || w.bytes != 3 {
				t.Fatalf("parent releases = %d, bytes = %d; want %d releases and 3 bytes", releases, w.bytes, wantReleases)
			}
		})
	}
}

func TestMeasureBoundsRepeatedTraversalPerOwner(t *testing.T) {
	root := t.TempDir()
	offender := filepath.Join("tasks", "expensive")
	deep := filepath.Join(root, offender, "workspace", strings.Repeat("d/", 10))
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		leaf := filepath.Join(deep, name, "child")
		if err := os.MkdirAll(leaf, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(leaf, "counted.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Two healthy owners each have a deep, branching tree. Their budgets must be independent both of the
	// expensive owner's exhausted budget and of each other's work, including reopens of their shared tasks parent.
	for _, owner := range []string{"healthy-a", "healthy-b"} {
		for _, branch := range []string{"left", "right"} {
			leaf := filepath.Join(root, "tasks", owner, "workspace", "deep", branch, "child")
			if err := os.MkdirAll(leaf, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(leaf, "counted.txt"), []byte("12345"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	w := walker{root: dir, group: 2, unmeasured: map[string]struct{}{}, reopenLimit: 16}
	if err := w.walk(dir, ".", nil, nil); err != nil {
		t.Fatal(err)
	}
	var unmeasured []string
	for prefix := range w.unmeasured {
		unmeasured = append(unmeasured, prefix)
	}
	if !slices.Equal(unmeasured, []string{offender}) || w.bytes != 22 {
		t.Fatalf("usage = %d bytes, unmeasured %q; want 20 healthy bytes, 2 reached expensive bytes and only %q unmeasured", w.bytes, unmeasured, offender)
	}
	if w.reopened[offender] != -1 {
		t.Fatal("expensive owner did not exhaust its own work budget")
	}
	for _, owner := range []string{"healthy-a", "healthy-b"} {
		if used := w.reopened[filepath.Join("tasks", owner)]; used <= 0 || used > w.reopenLimit {
			t.Fatalf("healthy owner %q reopen work = %d; want independent work within its budget", owner, used)
		}
	}
}

func TestMeasureStopsDescendingAfterBudgetExhaustion(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child", "uncounted.txt"), []byte("unvisited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "counted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	// A group-zero walk retains its measured root, so it must stop new descents even without another reopen.
	w := walker{root: dir, unmeasured: map[string]struct{}{".": {}}, reopened: map[string]int{".": -1}}
	if err := w.walk(dir, ".", nil, nil); err != nil {
		t.Fatal(err)
	}
	if w.bytes != 1 {
		t.Fatalf("bytes = %d; an exhausted owner must not descend into another child", w.bytes)
	}
}

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
