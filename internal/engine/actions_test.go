package engine

// Per-task operator actions: retry, supersede, archive and discard.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func heldPreflightFixture(t *testing.T) (*fixture, *App, model.Task) {
	t.Helper()
	f := newFixture(t)
	heldUploadPack(t, f)
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusBlocked
	putTask(t, f, task)
	app := New(f.state, f.dataDir)
	t.Cleanup(app.Shutdown)
	return f, app, task
}

func TestRetryResetsRepairBudget(t *testing.T) {
	t.Parallel()
	f, app, task := heldPreflightFixture(t)
	round := model.ReviewRound{
		SessionID:      "reviewer",
		Revision:       "r",
		ComparisonBase: "c",
		Result:         model.Review{Completed: true, Summary: "findings", Findings: []model.Finding{}},
		CreatedAt:      model.Now(),
	}
	task.Reviews = []model.ReviewRound{round, round}
	reason := model.BlockedReasonVerificationFailed
	task.BlockedReason = &reason
	putTask(t, f, task)
	go func() { _ = app.TaskAction(context.Background(), task.ID, "retry") }()
	waitForPreflights(t, f, 1)
	releasePreflight(t, f)
	var saved model.Task
	if !testutil.WaitUntil(15*time.Second, func() bool {
		saved = loadTask(t, f.state, task.ID)
		return saved.Status == model.StatusQueued
	}) {
		t.Fatalf("retry did not queue: %+v", saved)
	}
	if saved.Attempts != 1 || saved.ReviewBaseline != 2 {
		t.Fatalf("retried task = %+v; want attempt 1 with review baseline 2", saved)
	}
	if len(saved.Reviews) != 2 || saved.AttemptReviews() != 0 {
		t.Fatalf("retry lost retained evidence: %+v", saved)
	}
}

func TestTaskActions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	app := New(f.state, f.dataDir)
	t.Cleanup(app.Shutdown)
	ctx := context.Background()

	stale := executionTask(t, f, f.cfg.DefaultBranch)
	stale.Status = model.StatusBlocked
	reason := model.BlockedReasonStaleBase
	stale.BlockedReason = &reason
	putTask(t, f, stale)
	if err := app.TaskAction(ctx, stale.ID, "supersede"); err != nil {
		t.Fatal(err)
	}
	saved := loadTask(t, f.state, stale.ID)
	if saved.Status != model.StatusCancelled || !saved.RediscoveryRequested || saved.RediscoveryResult != nil {
		t.Fatalf("superseded task = %+v", saved)
	}

	done := checkpointedTask(t, f, f.cfg.DefaultBranch)
	done.Status = model.StatusPublished
	putTask(t, f, done)
	if err := app.TaskAction(ctx, done.ID, "discard"); err == nil || !IsActionConflict(err) {
		t.Fatalf("discard before archive = %v; want ineligible", err)
	}
	if err := app.TaskAction(ctx, done.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	saved = loadTask(t, f.state, done.ID)
	if saved.Lifecycle.ArchivedAt == nil {
		t.Fatalf("archived task = %+v", saved)
	}
	if err := app.TaskAction(ctx, done.ID, "discard"); err != nil {
		t.Fatal(err)
	}
	saved = loadTask(t, f.state, done.ID)
	if saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("discarded task = %+v", saved)
	}
	if _, err := os.Stat(done.Workspace); !os.IsNotExist(err) {
		t.Fatalf("discarded workspace still present: %v", err)
	}
	for _, action := range loadTask(t, f.state, done.ID).AllowedActions() {
		t.Fatalf("discarded task still offers %s", action)
	}
}

func TestRetryOnStaleBaseBlocks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	app := New(f.state, f.dataDir)
	t.Cleanup(app.Shutdown)
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Status = model.StatusBlocked
	putTask(t, f, task)
	git(t, f.repo, "commit", "--allow-empty", "-m", "External work")
	git(t, f.repo, "push", "origin", f.cfg.DefaultBranch)
	err := app.TaskAction(context.Background(), task.ID, "retry")
	if err == nil || model.BlockedReasonFromError(err) != model.BlockedReasonStaleBase {
		t.Fatalf("stale retry = %v; want the recorded stale-base failure", err)
	}
	saved := loadTask(t, f.state, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonStaleBase {
		t.Fatalf("stale retry outcome = %+v", saved)
	}
	if saved.Attempts != 0 {
		t.Fatalf("stale retry consumed an attempt: %+v", saved)
	}
}
