package engine

// A runner that commits its own work: the nested repository it committed must not reach review or publication.

import (
	"context"
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

func TestPublicationReplayRejectsUnvalidatedChangeSets(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"checkpoint replay", "reconciliation"} {
		for _, invalid := range []string{"committed gitlink", "missing comparison base"} {
			t.Run(entry+"/"+invalid, func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				task := checkpointedTask(t, f, f.cfg.DefaultBranch)
				if invalid == "committed gitlink" {
					must0(t, commitNestedRepository(task.Workspace))
					commit := git(t, task.Workspace, "rev-parse", "HEAD")
					// Model a checkpoint accepted before committed gitlinks were refused.
					task.OutputCommit = &commit
					task.Reviews[0].Revision = commit
					task.Verification[0].Revision = commit
				} else {
					task.ComparisonBase = ""
				}
				task.Status = model.StatusExecuting
				if entry == "reconciliation" {
					task.Status = model.StatusBlocked
					task.BlockedReason = new(model.BlockedPublicationUncertain)
				}
				putTask(t, f, task)
				app := f.pausedApp(t)
				if entry == "reconciliation" {
					must0(t, app.TaskAction(task.ID, model.TaskActionReconcile))
				} else {
					must0(t, app.superviseTask(context.Background(), task))
				}
				saved := loadTask(t, f.state, task.ID)
				if !blockedAs(saved, model.BlockedWorkspaceInvalid) {
					t.Fatalf("replayed publication status = %s, reason = %v, error = %s", saved.Status, saved.BlockedReason, optionalText(saved.Error))
				}
				if saved.OutputCommit == nil || *saved.OutputCommit != *task.OutputCommit {
					t.Fatal("refused replay lost its output checkpoint")
				}
				if saved.PRNumber != nil || len(publications(t, f)) != 0 {
					t.Fatal("an unvalidated change set reached publication")
				}
				if refs := git(t, f.repo, "ls-remote", "--heads", "origin", "refs/heads/"+task.Branch); refs != "" {
					t.Fatalf("an unvalidated change set was pushed: %s", refs)
				}
				assertNoOpenClients(t, f.script)
			})
		}
	}
}
