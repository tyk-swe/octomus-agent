package engine

// A runner that commits its own work: the nested repository it committed must not reach review or publication.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
)

// commitNestedRepository builds a nested repository in the workspace and commits it, as a runner that owns its own
// commits does: the work tree is clean afterwards, so the snapshot has nothing staged to refuse.
func commitNestedRepository(cwd string) error {
	run := func(directory string, args ...string) error {
		command := gitCommand(directory, args...)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, output)
		}
		return nil
	}
	nested := filepath.Join(cwd, "vendor", "module")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		return err
	}
	for _, step := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.name", "Executor"},
		{"config", "user.email", "executor@example.test"},
	} {
		if err := run(nested, step...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(nested, "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cwd, "feature.txt"), []byte("fixed\n"), 0o644); err != nil {
		return err
	}
	if err := run(nested, "add", "."); err != nil {
		return err
	}
	if err := run(nested, "commit", "-m", "Nested repository"); err != nil {
		return err
	}
	if err := run(cwd, "add", "--all"); err != nil {
		return err
	}
	return run(cwd, "commit", "-m", "Committed by the executor")
}

// The snapshot only refuses a staged nested repository. A runner that committed one leaves nothing staged, so without
// the refused range the gitlink reached review, verification and publication as content nobody reviewed.
func TestRunnerCommittedNestedRepositoryBlocks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Committed a nested repository", Effect: commitNestedRepository})
	script.Answer(routes.Reviewer, cleanReview("Looks complete"))
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if !blockedAs(saved, model.BlockedWorkspaceInvalid) {
		t.Fatalf("committed nested repository outcome = %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, `Nested repository or changed submodule at "vendor/module"`) {
		t.Fatalf("refusal = %q; want the committed gitlink's path", optionalText(saved.Error))
	}
	if len(saved.Reviews) != 0 || len(script.Turns(routes.Reviewer)) != 0 {
		t.Fatalf("a committed nested repository reached review: %+v", saved.Reviews)
	}
	if len(saved.Verification) != 0 {
		t.Fatalf("a committed nested repository reached verification: %+v", saved.Verification)
	}
	assertUnpublished(t, f, saved)
	assertNoOpenClients(t, script)
}
