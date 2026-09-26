// Package workspace holds the filesystem-safety helpers the scheduler and
// housekeeping share when creating, measuring, or deleting managed workspace
// directories.
package workspace

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// Initialized reports whether a task has a workspace it can resume in: a
// recorded session, a comparison base and a real checkout. Recovery, admission
// and preflight all read those three facts.
func Initialized(task model.Task) bool {
	if task.ExecutionSession == nil || task.ComparisonBase == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(task.Workspace, ".git"))
	return err == nil
}

// DirectorySize sums the sizes of non-symlink entries below path; a missing
// tree measures as zero and entries that vanish mid-scan are skipped.
func DirectorySize(path string) (uint64, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var size uint64
	for _, e := range entries {
		meta, err := os.Lstat(filepath.Join(path, e.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return 0, err
		}
		if meta.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		var n uint64
		if meta.IsDir() {
			n, err = DirectorySize(filepath.Join(path, e.Name()))
			if err != nil {
				return 0, err
			}
		} else {
			n = uint64(meta.Size())
		}
		if math.MaxUint64-size < n {
			size = math.MaxUint64
		} else {
			size += n
		}
	}
	return size, nil
}

// RemoveOwnedDir deletes path only when it is a plainly-named direct child of
// the owned workspace root and no component on the path — including path
// itself — is a symlink. A missing directory is already gone, not an error, and
// read-only directories inside the tree do not prevent its removal.
func RemoveOwnedDir(root, path string) error {
	// Dir cleans its argument, but RemoveAll uses the original path. Reject
	// components such as link/.. before they can hide a symlink from validation.
	if path != filepath.Clean(path) {
		return errors.New("Cleanup path must be canonical")
	}
	if filepath.Dir(path) != filepath.Clean(root) {
		return errors.New("Cleanup path must be a direct child of the owned workspace root")
	}
	// filepath.Base yields "/" for the filesystem root and "."/".." for dot
	// components; none names a child, so none may reach os.RemoveAll.
	if name := filepath.Base(path); name == "." || name == ".." || name == "/" {
		return errors.New("Invalid cleanup path")
	}
	for ancestor := path; ; {
		meta, err := os.Lstat(ancestor)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		} else if meta.Mode()&fs.ModeSymlink != 0 {
			return errors.New("Cleanup refuses symlink paths")
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	err := os.RemoveAll(path)
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	// Tools such as the Go module cache leave read-only directories whose
	// entries cannot be unlinked. The validated tree is owned: make its
	// directories writable and retry once.
	makeDirsWritable(filepath.Dir(path), filepath.Base(path))
	return os.RemoveAll(path)
}

// makeDirsWritable grants the owner full access to every directory in the tree
// name below root, each before its entries are read, so a directory that could
// not be listed or searched becomes reachable. Symlinks are never followed or
// changed, and every access goes through an os.Root, so a directory swapped for
// a symlink cannot redirect a change outside root. The walk uses the Root
// itself rather than its io/fs view, whose path rules stop at names Linux
// allows, such as bytes that are not UTF-8. Failures are ignored: the caller's
// retry reports whatever still cannot be removed.
func makeDirsWritable(root, name string) {
	owned, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	defer owned.Close()
	var visit func(dir string)
	visit = func(dir string) {
		info, err := owned.Lstat(dir)
		if err != nil || !info.IsDir() {
			return
		}
		if info.Mode().Perm()&0o700 != 0o700 {
			_ = owned.Chmod(dir, info.Mode().Perm()|0o700)
		}
		// O_DIRECTORY: an entry swapped for a FIFO since Lstat fails here
		// instead of blocking cleanup on an open that waits for a writer.
		f, err := owned.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return
		}
		// ReadDir returns the entries it read before any error.
		entries, _ := f.ReadDir(-1)
		f.Close()
		for _, entry := range entries {
			if entry.IsDir() {
				visit(filepath.Join(dir, entry.Name()))
			}
		}
	}
	visit(name)
}
