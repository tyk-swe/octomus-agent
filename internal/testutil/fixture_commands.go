package testutil

import (
	"os"
	"path/filepath"
	"strings"
)

const fixtureRootMarker = ".octomus-fixture-root"

// A POSIX shell (no Python interpreter startup) walks up from the working
// directory to the nearest marked fixture root and execs its bin command,
// never escaping an incomplete fixture. git falls back to the real binary.
const fixtureDispatcher = `#!/bin/sh
name=${0##*/}
cd -P . || exit 127
dir=$PWD
while :; do
	if [ -f "$dir/` + fixtureRootMarker + `" ]; then
		if [ -f "$dir/bin/$name" ]; then
			export OCTOMUS_FIXTURE="$dir"
			exec "$dir/bin/$name" "$@"
		fi
		break
	fi
	[ -n "$dir" ] || break
	dir=${dir%/*}
done
if [ "$name" = git ]; then
	exec /usr/bin/git "$@"
fi
echo "no fixture command '$name' for $PWD" >&2
exit 127
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
