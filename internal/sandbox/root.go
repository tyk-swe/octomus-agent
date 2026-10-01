package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// RunnerHomeDirs are where the shared runner state appears inside a runner sandbox's home, with the runner volume
// subdirectory each one mounts.
var RunnerHomeDirs = []struct{ Home, Volume string }{
	{".codex", "codex"},
	{".local/share/opencode", "opencode/data"},
	{".config/opencode", "opencode/config"},
	{".cache/opencode", "opencode/cache"},
	{".local/state/opencode", "opencode/state"},
}

const (
	RunnerHome = "home"
	VerifyHome = "verify-home"
)

// PrepareRoot makes the directories a sandbox of this kind mounts inside its owned root. Anything an earlier sandbox
// left in their place, a symlink or a file, is replaced by a plain directory, and a directory it chmodded is given
// back its owner-only mode; the walk goes through os.Root, so a planted link can never redirect it outside the root.
func PrepareRoot(spec Spec) error {
	rootPath := spec.Root()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()
	switch spec.Kind {
	case KindRunner:
		if err := plainDirectories(root, RunnerHome); err != nil {
			return err
		}
		for _, dir := range RunnerHomeDirs {
			if err := plainDirectories(root, filepath.Join(RunnerHome, dir.Home)); err != nil {
				return err
			}
		}
	case KindVerify:
		if spec.FreshHome {
			if err := workspace.RemoveOwnedDir(rootPath, filepath.Join(rootPath, VerifyHome)); err != nil {
				return err
			}
		}
		return plainDirectories(root, VerifyHome)
	}
	return nil
}

// plainDirectories makes every component of path a plain directory with mode 0700. A sandbox owns the directories it
// mounts and can chmod them, even to 000; each component is restored before the walk descends into it, so a later
// sandbox of the same task never finds its home locked.
func plainDirectories(root *os.Root, path string) error {
	current := ""
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		switch {
		case err == nil && info.IsDir():
			if info.Mode().Perm() != 0o700 {
				if err := root.Chmod(current, 0o700); err != nil {
					return err
				}
			}
			continue
		case err == nil:
			if err := root.RemoveAll(current); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		if err := root.Mkdir(current, 0o700); err != nil {
			return err
		}
	}
	return nil
}
