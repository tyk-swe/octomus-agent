package workspace

import (
	"errors"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"golang.org/x/sys/unix"
)

// GitDirName is the trusted git metadata beside each owned work tree; sandboxes mount it read-only, so work-tree content can never rewrite it.
const GitDirName = "repo.git"

var ErrNoGitDir = errors.New("Workspace has no trusted git metadata")

func Initialized(task model.Task) bool {
	if task.ExecutionSession == nil || task.ComparisonBase == "" {
		return false
	}
	_, err := GitDir(task.Workspace)
	return err == nil
}

// GitDir resolves the git metadata of an owned work tree without following symlinks. A split root always wins, so
// replacing the work tree's .git pointer can never redirect it; a .git directory inside the work tree is accepted only
// for clones made before the split layout, which never have a repo.git beside them.
func GitDir(workTree string) (string, error) {
	split := filepath.Join(filepath.Dir(workTree), GitDirName)
	info, err := os.Lstat(split)
	switch {
	case err == nil && info.IsDir():
		return split, nil
	case err == nil:
		return "", errors.New("Trusted git metadata is not a directory")
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if info, err := os.Lstat(filepath.Join(workTree, ".git")); err == nil && info.IsDir() {
		return filepath.Join(workTree, ".git"), nil
	}
	return "", ErrNoGitDir
}

// maxMeasuredDepth bounds how deep a storage walk descends: it opens no directory that sits this many levels or more
// below the measured one. A sandbox can nest directories without limit, and the walk holds one descriptor per level.
// Git refuses trees deeper than its core.maxTreeDepth, 2048 by default, and every clone below the data directory
// (the trusted checkout included) sits only a few levels down, so repository content never reaches this depth: only
// a chain a sandbox built itself does.
const maxMeasuredDepth = 2048 + 64

// Usage is one storage measurement. Bytes counts every file the walk reached. Unmeasured names, once each and sorted,
// the subtrees whose bytes are unknown because they could not be read or sit maxMeasuredDepth or more levels below the
// measured directory, each by at most the leading components Measure was asked to group by: whoever owns one must be
// treated as over any limit.
type Usage struct {
	Bytes      uint64
	Unmeasured []string
}

// Measure sums the sizes of the files under path. The walk resolves every name relative to its parent directory's
// descriptor, so no path it hands the kernel is longer than one name however deep a sandbox nests, and it never
// follows a symlink, even one swapped in while it runs.
//
// An unmeasured subtree is reported by its first group path components relative to path ("." for group 0), so the
// report grows with the directories at those levels, not with what a sandbox builds below one, and the walk never
// spells out a deeper path. Below path, only what a sandbox can cause in a tree it writes leaves a subtree unmeasured:
// a denied directory, one nested too deeply, or one swapped for a symlink or a file while the walk runs. Any other
// error, and any failure to read path itself, fails the measurement.
func Measure(path string, group int) (Usage, error) {
	dir, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Usage{}, nil
	}
	if err != nil {
		return Usage{}, err
	}
	defer dir.Close()
	w := walker{group: group, unmeasured: map[string]struct{}{}}
	if err := w.walk(dir, ".", 0); err != nil {
		return Usage{}, err
	}
	usage := Usage{Bytes: w.bytes}
	if len(w.unmeasured) > 0 {
		usage.Unmeasured = slices.Sorted(maps.Keys(w.unmeasured))
	}
	return usage, nil
}

type walker struct {
	group      int
	bytes      uint64
	unmeasured map[string]struct{}
}

// unmeasurable reports whether a walk error is one a sandbox can cause in a tree it writes: a directory it denied, or
// one it replaced with a symlink (O_NOFOLLOW) or a file (O_DIRECTORY) after the walk saw it.
func unmeasurable(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

// walk adds the files below dir, which sits depth levels below the measured directory and is reported as prefix.
func (w *walker) walk(dir *os.File, prefix string, depth int) error {
	names, err := dir.Readdirnames(-1)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Removed while the walk ran.
		return nil
	case err != nil && depth > 0 && unmeasurable(err):
		w.unmeasured[prefix] = struct{}{}
		return nil
	case err != nil:
		return err
	}
	fd := int(dir.Fd())
	for _, name := range names {
		var meta unix.Stat_t
		if err := unix.Fstatat(fd, name, &meta, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case depth > 0 && unmeasurable(err):
				// A directory that denies search denies every name in it.
				w.unmeasured[prefix] = struct{}{}
				return nil
			}
			return &fs.PathError{Op: "fstatat", Path: filepath.Join(dir.Name(), name), Err: err}
		}
		switch meta.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			continue
		case unix.S_IFDIR:
			child := prefix
			if depth < w.group {
				child = filepath.Join(prefix, name)
			}
			if depth+1 >= maxMeasuredDepth {
				w.unmeasured[child] = struct{}{}
				continue
			}
			// O_NOFOLLOW refuses a directory replaced by a symlink since the stat above.
			childFd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil && unmeasurable(err):
				w.unmeasured[child] = struct{}{}
				continue
			case err != nil:
				return &fs.PathError{Op: "openat", Path: filepath.Join(dir.Name(), name), Err: err}
			}
			childDir := os.NewFile(uintptr(childFd), name)
			err = w.walk(childDir, child, depth+1)
			childDir.Close()
			if err != nil {
				return err
			}
		default:
			if n := uint64(max(meta.Size, 0)); math.MaxUint64-w.bytes < n {
				w.bytes = math.MaxUint64
			} else {
				w.bytes += n
			}
		}
	}
	return nil
}

func RemoveOwnedDir(root, path string) error {
	if path != filepath.Clean(path) {
		return errors.New("Cleanup path must be canonical")
	}
	if filepath.Dir(path) != filepath.Clean(root) {
		return errors.New("Cleanup path must be a direct child of the owned workspace root")
	}
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
	makeDirsWritable(filepath.Dir(path), filepath.Base(path))
	return os.RemoveAll(path)
}

// makeDirsWritable walks through os.Root and never follows symlinks, so a swap cannot redirect a change outside root.
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
		f, err := owned.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return
		}
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
