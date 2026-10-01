package broker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// TestRunnerGitIgnoresWhatEarlierTurnsLeftInTheHome holds runner sandboxes' git to the broker's configuration: the
// executor, repair and reviewer turns of a task share one home, so an earlier turn must not be able to change the
// diff a later reviewer reads.
func TestRunnerGitIgnoresWhatEarlierTurnsLeftInTheHome(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	tools := t.TempDir()
	executable := filepath.Join(t.TempDir(), "octomus-agent")
	if err := os.WriteFile(executable, []byte("helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installTools(executable, tools); err != nil {
		t.Fatal(err)
	}
	// What an executor could leave in the shared home: a diff driver rewriting one file, attributes hiding another.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[diff \"hide\"]\n\ttextconv = sed s/injected/benign/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "git", "attributes"), []byte("a.txt diff=hide\nb.txt -diff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	clean := []string{"HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "PATH=" + os.Getenv("PATH")}
	git := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Octomus", "-c", "user.email=octomus@example.invalid"}, args...)...)
		cmd.Dir, cmd.Env = repo, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git(clean, "init", "-q")
	git(clean, "commit", "-q", "--allow-empty", "-m", "base")
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("injected\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(clean, "add", "a.txt", "b.txt")
	git(clean, "commit", "-q", "-m", "change")

	// The reviewer's git, without and with the runner sandbox's environment.
	ambient := []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "PATH=" + os.Getenv("PATH")}
	if diff := git(ambient, "diff", "HEAD~1", "HEAD"); strings.Count(diff, "+injected") != 0 {
		t.Fatalf("the planted home did not hide the change, so this test proves nothing:\n%s", diff)
	}
	cfg := testConfig(t)
	taskDir := makeRoot(t, cfg, "tasks/"+testUUID, runnerDirs()...)
	p, err := cfg.plan(wire.Request{Kind: "runner", Runner: "codex", Mode: wire.RunnerModeStdio, Dir: taskDir, Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	sandboxed := append([]string(nil), ambient...)
	for _, entry := range cfg.container(p, nil).Env {
		if key, value, _ := strings.Cut(entry, "="); strings.HasPrefix(key, "GIT_") {
			sandboxed = append(sandboxed, key+"="+strings.Replace(value, toolsMount, tools, 1))
		}
	}
	if diff := git(sandboxed, "diff", "HEAD~1", "HEAD"); strings.Count(diff, "+injected") != 2 {
		t.Fatalf("a runner sandbox's git applied configuration from the task home:\n%s", diff)
	}
}
