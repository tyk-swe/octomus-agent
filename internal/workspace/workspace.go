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

func Initialized(task model.Task) bool {
	if task.ExecutionSession == nil || task.ComparisonBase == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(task.Workspace, ".git"))
	return err == nil
}

func DirectorySize(path string) (uint64, error) {
	return directorySize(path, true)
}

func directorySize(path string, top bool) (uint64, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || !top && errors.Is(err, fs.ErrPermission) {
			return 0, nil
		}
		return 0, err
	}
	var size uint64
	for _, e := range entries {
		meta, err := os.Lstat(filepath.Join(path, e.Name()))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || !top && errors.Is(err, fs.ErrPermission) {
				continue
			}
			return 0, err
		}
		if meta.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		var n uint64
		if meta.IsDir() {
			n, err = directorySize(filepath.Join(path, e.Name()), false)
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
