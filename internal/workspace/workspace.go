// Package workspace holds the filesystem-safety helpers the scheduler and
// housekeeping share when creating, measuring, or deleting managed workspace
// directories (src/engine.rs workspace_initialized and housekeeping's
// directory_size / remove_owned_dir).
package workspace

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"

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
// itself — is a symlink. A missing directory is already gone, not an error.
func RemoveOwnedDir(root, path string) error {
	// Dir cleans its argument, but RemoveAll uses the original path. Reject
	// components such as link/.. before they can hide a symlink from validation.
	if path != filepath.Clean(path) {
		return errors.New("Cleanup path must be canonical")
	}
	if filepath.Dir(path) != filepath.Clean(root) {
		return errors.New("Cleanup path must be a direct child of the owned workspace root")
	}
	// filepath.Base yields "/" for the root, which Rust's file_name() reports
	// as no name; a direct child of "/" could otherwise reach os.RemoveAll.
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
	return os.RemoveAll(path)
}
