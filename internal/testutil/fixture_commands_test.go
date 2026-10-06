// The git/gh fixture dispatcher relays into the marked fixture root's bin directory.

package testutil_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestFixtureCommandsDispatch(t *testing.T) {
	installFixtureCommands(t)

	for _, fixture := range []string{"first", "second"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := testutil.MarkFixtureRoot(root); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"git", "gh"} {
				writeFixtureCommand(t, root, name)
			}
			nested := filepath.Join(root, "repository", "nested")
			if err := os.MkdirAll(nested, 0o755); err != nil {
				t.Fatal(err)
			}
			// An unmarked bin directory must not shadow its enclosing fixture.
			writeFixtureCommand(t, nested, "gh")
			for _, cwd := range []string{root, nested} {
				for _, name := range []string{"git", "gh"} {
					cmd := exec.Command(name, "argument with spaces", "--flag")
					cmd.Dir = cwd
					output, err := cmd.CombinedOutput()
					want := root + "\nargument with spaces\n--flag\n"
					if err != nil || string(output) != want {
						t.Fatalf("%s in %s = %q, %v; want %q", name, cwd, output, err, want)
					}
				}
			}
		})
	}
}

func installFixtureCommands(t *testing.T) {
	t.Helper()
	cleanup, err := testutil.InstallFixtureCommands()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
}

func writeFixtureCommand(t *testing.T, root, name string) {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteExecutable(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s\\n' \"$OCTOMUS_FIXTURE\" \"$@\"\n")); err != nil {
		t.Fatal(err)
	}
}
