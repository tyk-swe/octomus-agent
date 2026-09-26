package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// TestVerificationCommandThatMutatesTheWorktreeIsFailedEvidenceAndStopsTheRun:
// a command that changes a tracked file is the run's single recorded failure,
// naming the command and the mutation; later commands, including one that
// would restore the file, never run.
func TestVerificationCommandThatMutatesTheWorktreeIsFailedEvidenceAndStopsTheRun(t *testing.T) {
	app, task, revision := verificationFixture(t, []string{
		"printf 1 > impl.txt",
		"test \"$(cat impl.txt)\" = 1",
		"git checkout -- impl.txt",
	})
	_, err := app.verifyRevision(context.Background(), &task, revision)
	if err == nil || model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
		t.Fatalf("err = %v; want workspace_invalid", err)
	}
	saved, err := store.Get[model.Task](app.Store, "task", task.ID)
	if err != nil || saved == nil {
		t.Fatalf("saved task = %+v, %v", saved, err)
	}
	if len(saved.Verification) != 1 {
		t.Fatalf("verification = %+v; later commands must not run", saved.Verification)
	}
	first := saved.Verification[0]
	if first.Success || first.Command != "printf 1 > impl.txt" {
		t.Fatalf("first record = %+v; want the failed mutating command", first)
	}
	if !strings.Contains(first.Output, "changed during this verification command") {
		t.Fatalf("mutation evidence missing: %q", first.Output)
	}
}
