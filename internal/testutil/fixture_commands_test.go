package testutil_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestFixtureCommandsRejectUnmarkedRoots(t *testing.T) {
	installFixtureCommands(t)

	for _, withCommand := range []bool{false, true} {
		name := "outside fixture"
		if withCommand {
			name = "unmarked ancestor with bin gh"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if withCommand {
				writeFixtureCommand(t, root, "gh")
			}
			cwd := filepath.Join(root, "unrelated", "nested")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("gh", "--version")
			cmd.Dir = cwd
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 127 {
				t.Fatalf("gh outside a fixture = %q, %v; want exit 127", output, err)
			}
			if !strings.Contains(string(output), "no fixture command 'gh'") {
				t.Fatalf("gh error = %q; want a missing fixture diagnostic", output)
			}
		})
	}
}

func TestFixtureCommandsDispatchMarkedRoots(t *testing.T) {
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

func TestFixtureCommandsDoNotEscapeIncompleteFixtures(t *testing.T) {
	installFixtureCommands(t)
	outer := t.TempDir()
	if err := testutil.MarkFixtureRoot(outer); err != nil {
		t.Fatal(err)
	}
	writeFixtureCommand(t, outer, "gh")
	root := filepath.Join(outer, "incomplete")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testutil.MarkFixtureRoot(root); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("gh", "--version")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 {
		t.Fatalf("gh without a fixture command = %q, %v; want exit 127", output, err)
	}
}

func TestFixtureCommandsFallBackToRealGit(t *testing.T) {
	installFixtureCommands(t)
	want, err := exec.Command("/usr/bin/git", "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	for _, marked := range []bool{false, true} {
		root := t.TempDir()
		if marked {
			if err := testutil.MarkFixtureRoot(root); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", "--version")
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		if err != nil || string(output) != string(want) {
			t.Fatalf("git fallback (marked=%v) = %q, %v; want %q", marked, output, err, want)
		}
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
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s\\n' \"$OCTOMUS_FIXTURE\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
