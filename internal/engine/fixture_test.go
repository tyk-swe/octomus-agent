package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

type planningFixture struct {
	root    string
	dataDir string
	repo    string
	cfg     config.Config
	state   *store.Store
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func pythonFixtureShim(t *testing.T, path, root, fixture string) {
	t.Helper()
	fixtures := filepath.Join(repositoryRoot(t), "tests", "fixtures")
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os, runpy, sys\nos.environ['OCTOMUS_FIXTURE'] = %s\nsys.path.insert(0, %s)\nrunpy.run_path(%s, run_name='__main__')\n", strconv.Quote(root), strconv.Quote(fixtures), strconv.Quote(filepath.Join(fixtures, fixture)))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := gitCommand(directory, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), directory, err, stderr.String())
	}
	return strings.TrimSpace(string(output))
}

func gitCommand(directory string, args ...string) *exec.Cmd {
	cmd := process.Command("/usr/bin/git", directory)
	cmd.Args = append(cmd.Args, args...)
	return cmd
}

func newPlanningFixture(t *testing.T) *planningFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	pythonFixtureShim(t, filepath.Join(root, "bin", "git"), root, "git.py")
	codex := filepath.Join(root, "codex")
	pythonFixtureShim(t, codex, root, "codex.py")
	return newFixture(t, root, func(cfg *config.Config) {
		cfg.CodexBinary = codex
		cfg.OpencodeBinary = "/no-opencode-installed"
	})
}

func newFixture(t *testing.T, root string, configure func(*config.Config)) *planningFixture {
	t.Helper()
	bin := filepath.Join(root, "bin")
	repo := filepath.Join(root, "repository")
	remote := filepath.Join(root, "remote.git")
	dataDir := filepath.Join(root, "data")
	for _, directory := range []string{bin, repo, dataDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pythonFixtureShim(t, filepath.Join(bin, "gh"), root, "gh.py")
	if err := os.WriteFile(filepath.Join(root, "prs.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}

	git(t, root, "init", "--bare", "--initial-branch=main", remote)
	git(t, root, "init", "--initial-branch=main", repo)
	git(t, repo, "config", "user.name", "Fixture")
	git(t, repo, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Fixture\n\nThe feature contract requires fixed output.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "Initial fixture")
	git(t, repo, "remote", "add", "origin", remote)
	git(t, repo, "push", "-u", "origin", "main")

	previousWebhook, hadWebhook := os.LookupEnv(redact.WebhookEnv)
	if err := os.Unsetenv(redact.WebhookEnv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadWebhook {
			_ = os.Setenv(redact.WebhookEnv, previousWebhook)
		} else {
			_ = os.Unsetenv(redact.WebhookEnv)
		}
	})
	t.Setenv("OCTOMUS_FIXTURE", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := testConfig(repo)
	cfg.SessionTimeoutSeconds = 15
	cfg.CommandTimeoutSeconds = 5
	cfg.MaxSessionsPerDay = 30
	configure(&cfg)
	state := openStore(t, root)
	saveSettings(t, state, cfg, model.DefaultControl())
	return &planningFixture{root: root, dataDir: dataDir, repo: repo, cfg: cfg, state: state}
}

func waitCycle(t *testing.T, state *store.Store, id string) model.Cycle {
	t.Helper()
	var cycle *model.Cycle
	if !testutil.WaitUntil(30*time.Second, func() bool {
		var err error
		if cycle, err = store.Get[model.Cycle](state, "cycle", id); err != nil {
			t.Fatal(err)
		}
		return cycle != nil && cycle.Status != model.CycleRunning
	}) {
		t.Fatalf("cycle %s did not finish", id)
	}
	return *cycle
}
