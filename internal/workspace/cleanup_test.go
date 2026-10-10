package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRemoveOwnedDirBudgetsAndRetry(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		limits cleanupLimits
		want   string
	}{
		{"entries", cleanupLimits{entries: 3, reopens: 100, depth: 100}, "entry limit"},
		{"depth", cleanupLimits{entries: 100, reopens: 100, depth: 2}, "depth limit"},
		{"reopens", cleanupLimits{entries: 100, reopens: 1, depth: 100}, "ancestor traversal limit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			tree := filepath.Join(root, "task")
			if err := os.MkdirAll(filepath.Join(tree, "a", "b", "c"), 0o700); err != nil {
				t.Fatal(err)
			}
			for i := range 10 {
				if err := os.WriteFile(filepath.Join(tree, fmt.Sprintf("file-%d", i)), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			keep := filepath.Join(root, "sibling")
			if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := removeOwnedDir(context.Background(), root, tree, scenario.limits)
			if err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("bounded cleanup = %v; want %s", err, scenario.want)
			}
			if _, err := os.Stat(tree); err != nil {
				t.Fatalf("incomplete cleanup must leave its remaining root for retry: %v", err)
			}
			if err := RemoveOwnedDir(root, tree); err != nil {
				t.Fatalf("retry did not remove the remaining tree: %v", err)
			}
			if _, err := os.Stat(tree); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("successful retry left its root: %v", err)
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
				t.Fatalf("cleanup affected its sibling: %q, %v", data, err)
			}
		})
	}
}

// This context cancels after cleanup has removed its first file, so the check does not rely on wall-clock timing
// or a particular traversal's number of context checks.
type cancelAfterCleanupProgress struct {
	context.Context
	cancel context.CancelFunc
	path   string
	count  int
}

func (c *cancelAfterCleanupProgress) Err() error {
	if names, err := os.ReadDir(c.path); err == nil && len(names) < c.count {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRemoveOwnedDirCancellation(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "task")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if err := os.WriteFile(filepath.Join(tree, fmt.Sprint(i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RemoveOwnedDirContext(ctx, root, tree); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup = %v", err)
	}
	if entries, err := os.ReadDir(tree); err != nil || len(entries) != 20 {
		t.Fatalf("already cancelled cleanup changed its tree: %d entries, %v", len(entries), err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	progress := &cancelAfterCleanupProgress{Context: ctx, cancel: cancel, path: tree, count: 20}
	if err := removeOwnedDir(progress, root, tree, cleanupLimits{100, 100, 100}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during removal = %v", err)
	}
	if entries, err := os.ReadDir(tree); err != nil || len(entries) != 19 {
		t.Fatalf("removal continued after cancellation: %d entries, %v", len(entries), err)
	}
	if err := RemoveOwnedDir(root, tree); err != nil {
		t.Fatalf("cancelled cleanup could not be retried: %v", err)
	}
}

func TestCleanupPermissionRepairRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := writableDirectory(context.Background(), int(parent.Fd()), "locked"); err != nil {
		t.Fatalf("repairing a mode-000 directory: %v", err)
	}
	if meta, err := os.Stat(locked); err != nil || meta.Mode().Perm() != 0o700 {
		t.Fatalf("mode-000 repair = %v, %v", meta, err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("locked", filepath.Join(root, "replaced")); err != nil {
		t.Fatal(err)
	}
	if err := writableDirectory(context.Background(), int(parent.Fd()), "replaced"); err == nil {
		t.Fatal("permission repair followed a replaced directory's symlink")
	}
	if meta, err := os.Stat(locked); err != nil || meta.Mode().Perm() != 0o555 {
		t.Fatalf("symlink target changed mode during repair: %v, %v", meta, err)
	}
}

type mutateDuringCleanup struct {
	context.Context
	mutate func()
}

func (c *mutateDuringCleanup) Err() error {
	c.mutate()
	return c.Context.Err()
}

func TestRemoveOwnedDirRefusesReplacedAncestor(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "task")
	ancestor := filepath.Join(tree, "ancestor")
	leaf := filepath.Join(ancestor, "leaf")
	outside := filepath.Join(root, "sibling")
	for _, path := range []string{leaf, outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	trigger := filepath.Join(leaf, "remove")
	keep := filepath.Join(outside, "keep")
	for _, path := range []string{trigger, keep} {
		if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	changed := false
	ctx := &mutateDuringCleanup{Context: context.Background(), mutate: func() {
		if changed {
			return
		}
		if _, err := os.Stat(trigger); !errors.Is(err, fs.ErrNotExist) {
			return
		}
		changed = true
		if err := os.Rename(ancestor, filepath.Join(tree, "renamed")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, ancestor); err != nil {
			t.Fatal(err)
		}
	}}
	if err := removeOwnedDir(ctx, root, tree, cleanupLimits{100, 100, 100}); err == nil || !changed {
		t.Fatalf("cleanup must refuse an ancestor replaced while its child was open: changed %v, %v", changed, err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "content" {
		t.Fatalf("a replaced ancestor redirected cleanup into its sibling: %q, %v", data, err)
	}
	if err := RemoveOwnedDir(root, tree); err != nil {
		t.Fatalf("stable retry could not remove the renamed tree and link: %v", err)
	}
}

func TestRemoveOwnedDirMultipleBatches(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "task")
	for i := range 3*readBatchSize + 7 {
		dir := tree
		if i%5 == 0 {
			dir = filepath.Join(tree, fmt.Sprintf("dir-%d", i))
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%d", i)), nil, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveOwnedDir(root, tree); err != nil {
		t.Fatalf("removing interleaved directories and several batches of files: %v", err)
	}
	if _, err := os.Stat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("batched removal left its root: %v", err)
	}
}

func TestRemoveOwnedDirBeyondPathMax(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "task")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(tree)
	if err != nil {
		t.Fatal(err)
	}
	const component = "a-directory-name-long-enough"
	// Build a path longer than the kernel's pathname limit while retaining only one descriptor. Cleanup must use
	// descriptor-relative components too, and repair each read-only ancestor as it descends.
	for range 170 {
		if err := unix.Mkdirat(int(dir.Fd()), component, 0o700); err != nil {
			dir.Close()
			t.Fatal(err)
		}
		fd, err := unix.Openat(int(dir.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			dir.Close()
			t.Fatal(err)
		}
		if err := dir.Chmod(0o500); err != nil {
			dir.Close()
			unix.Close(fd)
			t.Fatal(err)
		}
		dir.Close()
		dir = os.NewFile(uintptr(fd), component)
	}
	fd, err := unix.Openat(int(dir.Fd()), "artifact", unix.O_CREAT|unix.O_WRONLY|unix.O_CLOEXEC, 0o400)
	if err != nil {
		dir.Close()
		t.Fatal(err)
	}
	unix.Close(fd)
	if err := dir.Chmod(0o000); err != nil {
		dir.Close()
		t.Fatal(err)
	}
	dir.Close()
	if err := RemoveOwnedDir(root, tree); err != nil {
		t.Fatalf("removing a deep read-only tree: %v", err)
	}
	if _, err := os.Stat(tree); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deep tree remained after successful removal: %v", err)
	}
}
