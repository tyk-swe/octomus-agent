package testutil

import (
	"os"
	"path/filepath"
	"strings"
)

const fixtureRootMarker = ".octomus-fixture-root"

const fixtureDispatcher = `#!/usr/bin/env python3
import os
from pathlib import Path
import sys

name = Path(sys.argv[0]).name
cwd = Path.cwd().resolve()
root = next((candidate for candidate in (cwd, *cwd.parents) if (candidate / "` + fixtureRootMarker + `").is_file()), None)
if root is not None:
    target = root / "bin" / name
    if target.is_file():
        env = os.environ.copy()
        env["OCTOMUS_FIXTURE"] = str(root)
        os.execve(target, [name, *sys.argv[1:]], env)
if name == "git":
    os.execv("/usr/bin/git", ["git", *sys.argv[1:]])
sys.stderr.write(f"no fixture command {name!r} for {cwd}\n")
sys.exit(127)
`

// MarkFixtureRoot identifies a directory whose bin commands belong to a test fixture.
func MarkFixtureRoot(root string) error {
	return os.WriteFile(filepath.Join(root, fixtureRootMarker), nil, 0o644)
}

func InstallFixtureCommands() (func(), error) {
	dir, err := os.MkdirTemp("", "octomus-fixture-bin")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (func(), error) {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	for _, name := range []string{"git", "gh"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(fixtureDispatcher), 0o755); err != nil {
			return fail(err)
		}
	}
	previous, had := os.LookupEnv("PATH")
	entries := append([]string{dir}, filepath.SplitList(previous)...)
	if err := os.Setenv("PATH", strings.Join(entries, string(os.PathListSeparator))); err != nil {
		return fail(err)
	}
	return func() {
		if had {
			_ = os.Setenv("PATH", previous)
		} else {
			_ = os.Unsetenv("PATH")
		}
		_ = os.RemoveAll(dir)
	}, nil
}
