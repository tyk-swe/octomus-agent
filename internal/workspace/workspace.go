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
	return "", errors.New("Workspace has no trusted git metadata")
}

// maxMeasuredDepth bounds how deep a storage walk descends: it opens no directory that sits this many levels or more
// below the measured one. A sandbox can nest directories without limit, so the walk must bound its in-memory stack.
// Git refuses trees deeper than its core.maxTreeDepth, 2048 by default, and every clone below the data directory
// (the trusted checkout included) sits only a few levels down, so repository content never reaches this depth: only
// a chain a sandbox built itself does.
const maxMeasuredDepth = 2048 + 64

// maxReopens bounds repeated traversal work for each requested ownership group. A deep chain needs no
// reopens, but deep non-leaf siblings otherwise multiply their ancestor depth by their fanout. This budget is much
// larger than the depth limit, allowing ordinary branching without letting one owner force unbounded repeated work.
const maxReopens = 64 * 1024

// maxAttempts lets transient file removals settle without trusting an incomplete snapshot. Every retry
// starts over with fresh descriptors, accounting and traversal budgets; a final incomplete attempt keeps its owners
// unknown. This is a bounded response to detected mutations, not a filesystem snapshot.
const maxAttempts = 3

// Usage is one storage measurement. Bytes counts every file the walk reached. Unmeasured names, once each and sorted,
// the subtrees whose bytes are unknown because they could not be read or exceeded a traversal depth or work bound,
// each by at most the leading components Measure was asked to group by: whoever owns one must be treated as over any
// limit.
type Usage struct {
	Bytes      uint64
	Unmeasured []string
}

// Measure sums the sizes of the files under path. The walk resolves every name relative to its parent directory's
// descriptor, so no path it hands the kernel is longer than one name however deep a sandbox nests, and it never
// follows a symlink, even one swapped in while it runs. It keeps at most three directory descriptors open, regardless
// of depth: the measured root, the current directory, and its next child.
//
// An unmeasured subtree is reported by its first group path components relative to path ("." for group 0), so the
// report grows with the directories at those levels, not with what a sandbox builds below one, and the walk never
// spells out a deeper path. Below path, only what a sandbox can cause in a tree it writes leaves a subtree unmeasured:
// a denied directory, one nested too deeply, one requiring too much repeated ancestor traversal, or an entry moved
// or replaced while the walk runs. Entries disappearing before their first stat trigger a bounded fresh scan, so a
// settled temporary-file removal does not block its owner. Any other error, and any failure to read path itself,
// fails the measurement.
func Measure(path string, group int) (Usage, error) {
	var usage Usage
	for range maxAttempts {
		dir, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			return Usage{}, nil
		}
		if err != nil {
			return Usage{}, err
		}
		w := walker{root: dir, group: group, unmeasured: map[string]struct{}{}}
		err = w.walk(w.root, ".", nil, nil)
		dir.Close()
		if err != nil {
			return Usage{}, err
		}
		usage = Usage{Bytes: w.bytes}
		if len(w.unmeasured) > 0 {
			usage.Unmeasured = slices.Sorted(maps.Keys(w.unmeasured))
		}
		if !w.retry {
			break
		}
	}
	return usage, nil
}

type walker struct {
	root       *os.File
	group      int
	bytes      uint64
	unmeasured map[string]struct{}
	retry      bool           // An entry vanished before its first stat; only a fresh scan can establish its bytes.
	reopened   map[string]int // Component opens per owner; -1 means its budget was exhausted.
}

// measuredDir identifies one component below root. Closed ancestors are reopened through these components rather
// than through ".." (which could escape a renamed subtree) or a full path (which could exceed PATH_MAX).
type measuredDir struct {
	name string
	dev  uint64
	ino  uint64
}

func (d measuredDir) matches(meta *unix.Stat_t) bool {
	return d.dev == uint64(meta.Dev) && d.ino == uint64(meta.Ino)
}

func (w *walker) childPrefix(prefix, name string, depth int) string {
	if depth < w.group {
		return filepath.Join(prefix, name)
	}
	return prefix
}

// unmeasurable reports whether a walk error is one a sandbox can cause in a tree it writes: a directory it denied, or
// one it replaced with a symlink (O_NOFOLLOW) or a file (O_DIRECTORY) after the walk saw it.
func unmeasurable(err error) bool {
	return errors.Is(err, fs.ErrPermission) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

// walk takes ownership of dir, except for the measured root. A child can scan its files with its parent still open,
// avoiding repeated ancestor reopens for leaf siblings. Before opening a grandchild it releases that parent, so an
// untrusted deep tree cannot exhaust the process's descriptors. Only closed ancestors with pending children reopen.
func (w *walker) walk(dir *os.File, prefix string, path []measuredDir, releaseParent func()) error {
	closeDir := func() {
		if dir != nil && dir != w.root {
			dir.Close()
			dir = nil
		}
	}
	defer closeDir()
	depth := len(path)
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
	directories, err := w.measureEntries(dir, prefix, depth, names)
	if err != nil {
		return err
	}
	for _, component := range directories {
		name := component.name
		if w.reopened[prefix] < 0 {
			return nil
		}
		child := w.childPrefix(prefix, name, depth)
		if depth+1 >= maxMeasuredDepth {
			w.unmeasured[child] = struct{}{}
			continue
		}
		if dir == nil {
			dir, err = w.reopen(path, prefix)
			if err != nil || dir == nil {
				return err
			}
		}
		if releaseParent != nil {
			releaseParent()
			releaseParent = nil
		}
		// Refuse changed or missing children: their original bytes may remain in a scanned part of this owner.
		childFd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		switch {
		case errors.Is(err, fs.ErrNotExist), err != nil && unmeasurable(err):
			w.unmeasured[child] = struct{}{}
			continue
		case err != nil:
			return &fs.PathError{Op: "openat", Path: filepath.Join(dir.Name(), name), Err: err}
		}
		childDir := os.NewFile(uintptr(childFd), name)
		var meta unix.Stat_t
		if err := unix.Fstat(childFd, &meta); err != nil {
			childDir.Close()
			return &fs.PathError{Op: "fstat", Path: name, Err: err}
		}
		if !component.matches(&meta) {
			childDir.Close()
			w.unmeasured[child] = struct{}{}
			continue
		}
		if err := w.walk(childDir, child, append(path, component), closeDir); err != nil {
			return err
		}
	}
	return nil
}

// measureEntries counts files before any descent, so a single-child chain needs no ancestor reopens. Directory
// identities are captured here, before opening any child: a renamed directory may keep its bytes in an already
// scanned part of the owner's tree, and a replacement at the old name must not hide that missing measurement.
func (w *walker) measureEntries(dir *os.File, prefix string, depth int, names []string) ([]measuredDir, error) {
	fd := int(dir.Fd())
	var directories []measuredDir
	for _, name := range names {
		var meta unix.Stat_t
		if err := unix.Fstatat(fd, name, &meta, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist):
				// It may be a removed temporary file or data moved into an already-scanned directory. Mark
				// this attempt incomplete and retry from the root, never carry its partial count forward.
				w.unmeasured[w.childPrefix(prefix, name, depth)] = struct{}{}
				w.retry = true
				continue
			case depth > 0 && unmeasurable(err):
				// A directory that denies search denies every name in it.
				w.unmeasured[prefix] = struct{}{}
				return nil, nil
			}
			return nil, &fs.PathError{Op: "fstatat", Path: filepath.Join(dir.Name(), name), Err: err}
		}
		switch meta.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			continue
		case unix.S_IFDIR:
			directories = append(directories, measuredDir{name: name, dev: uint64(meta.Dev), ino: uint64(meta.Ino)})
		default:
			if n := uint64(max(meta.Size, 0)); math.MaxUint64-w.bytes < n {
				w.bytes = math.MaxUint64
			} else {
				w.bytes += n
			}
		}
	}
	return directories, nil
}

// reopen returns an ancestor with pending children, closing each temporary descriptor before continuing. Verify
// every component's identity so a renamed or replaced ancestor cannot redirect the remainder of the measurement.
// As with a swap during the initial descent, an inaccessible or changed subtree is reported as unmeasured.
func (w *walker) reopen(path []measuredDir, prefix string) (*os.File, error) {
	dir := w.root
	limit := maxReopens
	for _, component := range path {
		// Ancestors above the grouping level are shared by several owners. Reopening those must not spend a
		// shared budget and incorrectly make healthy siblings unmeasured after visiting an expensive owner.
		if len(path) >= w.group {
			if w.reopened == nil {
				w.reopened = map[string]int{}
			}
			used := w.reopened[prefix]
			if used < 0 || used >= limit {
				if dir != w.root {
					dir.Close()
				}
				w.reopened[prefix] = -1
				w.unmeasured[prefix] = struct{}{}
				return nil, nil
			}
			w.reopened[prefix] = used + 1
		}
		fd, err := unix.Openat(int(dir.Fd()), component.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if dir != w.root {
			dir.Close()
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// This ancestor had pending children. It may have moved into an already-scanned directory, so a
			// missing old name does not establish that its remaining bytes left the measured ownership group.
			w.unmeasured[prefix] = struct{}{}
			return nil, nil
		case err != nil && unmeasurable(err):
			w.unmeasured[prefix] = struct{}{}
			return nil, nil
		case err != nil:
			return nil, &fs.PathError{Op: "openat", Path: component.name, Err: err}
		}
		dir = os.NewFile(uintptr(fd), component.name)
		var meta unix.Stat_t
		if err := unix.Fstat(fd, &meta); err != nil {
			dir.Close()
			return nil, &fs.PathError{Op: "fstat", Path: component.name, Err: err}
		}
		if !component.matches(&meta) {
			dir.Close()
			w.unmeasured[prefix] = struct{}{}
			return nil, nil
		}
	}
	return dir, nil
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
