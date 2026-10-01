package workspace

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
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

// maxMeasuredDepth bounds how deep a storage walk descends. A sandbox can nest directories without limit, and the walk
// holds one descriptor for each level.
const maxMeasuredDepth = 256

// Usage is one storage measurement. Bytes counts every file the walk reached. Unmeasured lists, relative to the
// measured directory, each subtree it could not read or that nests deeper than maxMeasuredDepth: its bytes are
// unknown, so whoever owns it must be treated as over any limit.
type Usage struct {
	Bytes      uint64
	Unmeasured []string
}

// Measure sums the sizes of the files under path. The walk resolves every name relative to its parent directory's
// descriptor, so no path it hands the kernel is longer than one name however deep a sandbox nests, and it never
// follows a symlink, even one swapped in while it runs. Only a failure to read path itself is an error.
func Measure(path string) (Usage, error) {
	dir, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Usage{}, nil
	}
	if err != nil {
		return Usage{}, err
	}
	defer dir.Close()
	var usage Usage
	if err := usage.walk(dir, ".", 0); err != nil {
		return Usage{}, err
	}
	return usage, nil
}

func (u *Usage) walk(dir *os.File, rel string, depth int) error {
	unreadable := func(err error) error {
		if depth == 0 {
			return err
		}
		u.Unmeasured = append(u.Unmeasured, rel)
		return nil
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return unreadable(err)
	}
	fd := int(dir.Fd())
	for _, name := range names {
		var meta unix.Stat_t
		if err := unix.Fstatat(fd, name, &meta, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return unreadable(&fs.PathError{Op: "fstatat", Path: filepath.Join(dir.Name(), name), Err: err})
		}
		switch meta.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			continue
		case unix.S_IFDIR:
			child := filepath.Join(rel, name)
			if depth+1 >= maxMeasuredDepth {
				u.Unmeasured = append(u.Unmeasured, child)
				continue
			}
			// O_NOFOLLOW refuses a directory replaced by a symlink since the stat above.
			childFd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				u.Unmeasured = append(u.Unmeasured, child)
				continue
			}
			childDir := os.NewFile(uintptr(childFd), name)
			err = u.walk(childDir, child, depth+1)
			childDir.Close()
			if err != nil {
				return err
			}
		default:
			if n := uint64(max(meta.Size, 0)); math.MaxUint64-u.Bytes < n {
				u.Bytes = math.MaxUint64
			} else {
				u.Bytes += n
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
