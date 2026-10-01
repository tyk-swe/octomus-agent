package testutil_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestIsolateGitEnvironment(t *testing.T) {
	const child = "OCTOMUS_TEST_GIT_ENVIRONMENT_CHILD"
	if os.Getenv(child) == "1" {
		if err := testutil.IsolateGitEnvironment(); err != nil {
			t.Fatal(err)
		}
		repo := t.TempDir()
		git := func(args ...string) string {
			t.Helper()
			cmd := exec.Command("/usr/bin/git", args...)
			cmd.Dir = repo
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		git("init", "-b", "main")
		git("config", "user.name", "Fixture")
		git("config", "user.email", "fixture@example.com")
		git("commit", "--allow-empty", "-m", "Initial fixture")
		if got := git("log", "-1", "--format=%s"); got != "Initial fixture" {
			t.Fatalf("fixture commit = %q", got)
		}
		if got := git("config", "--local", "user.name"); got != "Fixture" {
			t.Fatalf("repository-local identity = %q", got)
		}
		if got := git("config", "--list"); strings.Contains(got, "ambient") {
			t.Fatalf("inherited config still present: %s", got)
		}
		// Test cases can still inject the environment that their behavior needs.
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "user.name")
		t.Setenv("GIT_CONFIG_VALUE_0", "Explicit fixture identity")
		if got := git("config", "user.name"); got != "Explicit fixture identity" {
			t.Fatalf("explicit fixture override = %q", got)
		}
		return
	}

	root := t.TempDir()
	config := filepath.Join(root, "ambient.gitconfig")
	if err := os.WriteFile(config, []byte("[commit]\n\tgpgSign = true\n[gpg]\n\tprogram = /no-fixture-signing-program\n[fixture]\n\tambient = inherited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	const original = "This file is outside the fixture; do not change it.\n"
	if err := os.WriteFile(outside, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	before := os.Environ()
	for name, extra := range map[string][]string{
		"global":     {"GIT_CONFIG_GLOBAL=" + config},
		"system":     {"GIT_CONFIG_SYSTEM=" + config, "GIT_CONFIG_NOSYSTEM=0"},
		"inline":     {"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=commit.gpgSign", "GIT_CONFIG_VALUE_0=true"},
		"parameters": {"GIT_CONFIG_PARAMETERS='commit.gpgSign=true'"},
		"config":     {"GIT_CONFIG=" + outside},
		"directory":  {"GIT_DIR=" + outside},
		"worktree":   {"GIT_WORK_TREE=" + outside},
		"index":      {"GIT_INDEX_FILE=" + outside},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestIsolateGitEnvironment$")
			for _, entry := range before {
				if !strings.HasPrefix(entry, "GIT_") && !strings.HasPrefix(entry, child+"=") {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, child+"=1", "HOME="+root, "XDG_CONFIG_HOME="+root, "GIT_CONFIG_NOSYSTEM=1")
			cmd.Env = append(cmd.Env, extra...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated Git fixture: %v\n%s", err, out)
			}
			if data, err := os.ReadFile(outside); err != nil || string(data) != original {
				t.Fatalf("outside file changed: %q, %v", data, err)
			}
		})
	}
	if !slices.Equal(os.Environ(), before) {
		t.Fatal("fixture child changed the parent's environment")
	}
}
