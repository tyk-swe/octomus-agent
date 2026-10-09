package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const cleanupTime = 30 * time.Second

type cleanupLimits struct {
	entries, reopens, depth int
}

// RemoveOwnedDir removes one direct child of root, including directories made read-only by a sandbox or package
// manager. Removal and permission repair share the same traversal bounds; an incomplete removal returns an error
// and leaves its remaining tree available for a later retry.
func RemoveOwnedDir(root, path string) error {
	return RemoveOwnedDirContext(context.Background(), root, path)
}

// RemoveOwnedDirContext additionally stops between filesystem operations when ctx ends. Each attempt has a
// 30-second deadline and limits on entries, depth and repeated ancestor opens, and holds at most three directory
// descriptors. Neither removal nor permission repair follows a symlink, including one swapped in during cleanup.
func RemoveOwnedDirContext(ctx context.Context, root, path string) error {
	ctx, cancel := context.WithTimeout(ctx, cleanupTime)
	defer cancel()
	return removeOwnedDir(ctx, root, path, cleanupLimits{maxOwnerEntries, maxReopens, maxMeasuredDepth})
}

func removeOwnedDir(ctx context.Context, root, path string, limits cleanupLimits) error {
	if path != filepath.Clean(path) {
		return errors.New("Cleanup path must be canonical")
	}
	if filepath.Dir(path) != filepath.Clean(root) {
		return errors.New("Cleanup path must be a direct child of the owned workspace root")
	}
	name := filepath.Base(path)
	if name == "." || name == ".." || name == "/" {
		return errors.New("Invalid cleanup path")
	}
	for ancestor := path; ; {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta, err := os.Lstat(ancestor)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil && meta.Mode()&fs.ModeSymlink != 0 {
			return errors.New("Cleanup refuses symlink paths")
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			break
		}
		ancestor = parent
	}
	owned, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Keep the root's descriptor rather than spelling descendants as full paths. Every operation below resolves
	// one component with openat/unlinkat, and closed ancestors reopen only if their recorded identities still match.
	parent, err := owned.Open(".")
	owned.Close()
	if err != nil {
		return err
	}
	defer parent.Close()
	w := cleanupWalker{ctx: ctx, root: parent, limits: limits}
	if err := w.remove(name); err != nil {
		return fmt.Errorf("Removing owned workspace: %w", err)
	}
	return nil
}

type cleanupWalker struct {
	ctx    context.Context
	root   *os.File
	limits cleanupLimits
}

func (w *cleanupWalker) check() error { return w.ctx.Err() }

func (w *cleanupWalker) remove(name string) error {
	if err := w.check(); err != nil {
		return err
	}
	var meta unix.Stat_t
	if err := unix.Fstatat(int(w.root.Fd()), name, &meta, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if meta.Mode&unix.S_IFMT == unix.S_IFLNK {
		return errors.New("cleanup refuses symlink paths")
	}
	if meta.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := w.check(); err != nil {
			return err
		}
		return unix.Unlinkat(int(w.root.Fd()), name, 0)
	}
	dir, component, err := w.open(w.root, name, true)
	if err != nil {
		return err
	}
	defer func() {
		if dir != nil && dir != w.root {
			dir.Close()
		}
	}()
	path := []measuredDir{component}
	for {
		if err := w.check(); err != nil {
			return err
		}
		names, err := dir.Readdirnames(readBatchSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(names) == 0 {
			dir.Close()
			dir = nil
			parent, err := w.reopen(path[:len(path)-1])
			if err != nil {
				return err
			}
			dir = parent
			last := path[len(path)-1]
			if err := w.check(); err != nil {
				return err
			}
			if err := unix.Fstatat(int(parent.Fd()), last.name, &meta, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
			if !last.matches(&meta) {
				return errors.New("cleanup directory changed while removing it")
			}
			if err := w.check(); err != nil {
				return err
			}
			if err := unix.Unlinkat(int(parent.Fd()), last.name, unix.AT_REMOVEDIR); err != nil {
				return err
			}
			path = path[:len(path)-1]
			if len(path) == 0 {
				return nil
			}
			continue
		}
		descended := false
		for _, name := range names {
			if err := w.check(); err != nil {
				return err
			}
			if w.limits.entries <= 0 {
				return errors.New("cleanup entry limit exceeded; retry to remove the remaining tree")
			}
			w.limits.entries--
			err := unix.Unlinkat(int(dir.Fd()), name, 0)
			if err == nil || errors.Is(err, fs.ErrNotExist) {
				continue
			}
			// Unix reports an attempt to unlink a directory as EISDIR or EPERM. open verifies the type without
			// following links, repairs owner permissions, and returns the directory's actual identity.
			if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
				return err
			}
			if len(path) >= w.limits.depth {
				return errors.New("cleanup directory depth limit exceeded")
			}
			child, component, err := w.open(dir, name, true)
			if err != nil {
				return err
			}
			dir.Close()
			dir = child
			path = append(path, component)
			descended = true
			break
		}
		if !descended {
			if err := w.check(); err != nil {
				return err
			}
			// Removed entries can invalidate a directory stream's offset. A fresh pass sees everything that remains;
			// each pass makes progress or fails, and repeated entries still spend the shared entry budget.
			if _, err := dir.Seek(0, io.SeekStart); err != nil {
				return err
			}
		}
	}
}

func (w *cleanupWalker) open(parent *os.File, name string, repair bool) (*os.File, measuredDir, error) {
	if err := w.check(); err != nil {
		return nil, measuredDir{}, err
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if repair && errors.Is(err, fs.ErrPermission) {
		if err = w.check(); err == nil {
			err = writableDirectory(w.ctx, int(parent.Fd()), name)
		}
		if err == nil {
			err = w.check()
		}
		if err == nil {
			fd, err = unix.Openat(int(parent.Fd()), name, flags, 0)
		}
	}
	if err != nil {
		return nil, measuredDir{}, err
	}
	dir := os.NewFile(uintptr(fd), name)
	var meta unix.Stat_t
	if err = w.check(); err == nil {
		err = unix.Fstat(fd, &meta)
	}
	if err == nil && repair && meta.Mode&0o700 != 0o700 {
		if err = w.check(); err == nil {
			err = unix.Fchmod(fd, uint32(meta.Mode)&0o777|0o700)
		}
	}
	if err != nil {
		dir.Close()
		return nil, measuredDir{}, err
	}
	return dir, measuredDir{name: name, dev: uint64(meta.Dev), ino: uint64(meta.Ino)}, nil
}

func (w *cleanupWalker) reopen(path []measuredDir) (*os.File, error) {
	dir := w.root
	for _, component := range path {
		if w.limits.reopens <= 0 {
			if dir != w.root {
				dir.Close()
			}
			return nil, errors.New("cleanup ancestor traversal limit exceeded; retry to remove the remaining tree")
		}
		w.limits.reopens--
		next, actual, err := w.open(dir, component.name, false)
		if dir != w.root {
			dir.Close()
		}
		if err != nil {
			return nil, err
		}
		if actual != component {
			next.Close()
			return nil, errors.New("cleanup ancestor changed while removing it")
		}
		dir = next
	}
	return dir, nil
}
