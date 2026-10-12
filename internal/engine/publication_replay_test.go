package engine

// Publication replay: a delivery whose pull request already merged must be recorded as delivered, not delivered a
// second time.

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestPublicationReplayOfSettledPullRequestWritesNothing(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			replaySettledPullRequest(t, state)
		})
	}
}

func replaySettledPullRequest(t *testing.T, state string) {
	t.Helper()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("First pass looks complete"))
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)
	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusPublished || saved.PRNumber == nil || saved.OutputCommit == nil {
		t.Fatalf("scripted task did not publish: %+v", saved)
	}
	delivered := publications(t, f)
	if len(delivered) != 1 {
		t.Fatalf("initial delivery wrote %d remote records: %+v", len(delivered), delivered)
	}

	// Its durable settlement was refused, so the task is blocked as uncertain, and the pull request it created has
	// merged since. Replay must only confirm what the remote already shows.
	patch := map[string]any{"state": state, "merged_at": nil}
	if state == "merged" {
		patch["merged_at"] = "2026-10-12T00:00:00Z"
	}
	patchPRs(t, f, patch)
	blocked := saved.Clone()
	blocked.Status = model.StatusBlocked
	blocked.BlockedReason = new(model.BlockedPublicationUncertain)
	blocked.Error = new("The publication result was unconfirmed")
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), blocked))

	if err := app.TaskAction(saved.ID, model.TaskActionReconcile); err != nil {
		t.Fatalf("publication reconciliation failed: %v", err)
	}
	current := loadTask(t, f.state, saved.ID)
	if current.Status != model.StatusPublished || current.PRNumber == nil || *current.PRNumber != *saved.PRNumber ||
		current.OutputCommit == nil || *current.OutputCommit != *saved.OutputCommit {
		t.Fatalf("replayed delivery = %+v", current)
	}
	if now := publications(t, f); len(now) != len(delivered) {
		t.Fatalf("replay issued %d further remote writes: %+v", len(now)-len(delivered), now[len(delivered):])
	}
	if prs := prsJSON(t, f); len(prs) != 1 {
		t.Fatalf("replay created another pull request: %+v", prs)
	}
	if recorded := prObservation(t, f, *saved.PRNumber).PR.State; recorded != state {
		t.Fatalf("replayed observation records the pull request as %q; want %s", recorded, state)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("replaying a settled pull request issued a merge")
	}
	assertNoOpenClients(t, script)
}
