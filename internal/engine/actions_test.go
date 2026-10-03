package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func heldPreflightFixture(t *testing.T) (*planningFixture, *App, model.Task) {
	t.Helper()
	fixture := newExecutionFixture(t)
	heldUploadPack(t, fixture)
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusBlocked
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	return fixture, app, task
}

func TestRetryStartsAFreshRepairRoundBudget(t *testing.T) {
	t.Parallel()
	fixture, app, task := heldPreflightFixture(t)
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
	saveExecutionTask(t, fixture, task)
	go func() { _ = app.TaskAction(context.Background(), task.ID, "retry") }()
	waitForPreflights(t, fixture, 1)
	releasePreflight(t, fixture)
	var saved model.Task
	if !testutil.WaitUntil(15*time.Second, func() bool {
		saved = loadTask(t, fixture.state, task.ID)
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

func TestTaskActionSupersedeArchiveDiscard(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	ctx := context.Background()

	stale := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	stale.Status = model.StatusBlocked
	reason := model.BlockedReasonStaleBase
	stale.BlockedReason = &reason
	saveExecutionTask(t, fixture, stale)
	if err := app.TaskAction(ctx, stale.ID, "supersede"); err != nil {
		t.Fatal(err)
	}
	saved := loadTask(t, fixture.state, stale.ID)
	if saved.Status != model.StatusCancelled || !saved.RediscoveryRequested || saved.RediscoveryResult != nil {
		t.Fatalf("superseded task = %+v", saved)
	}

	done := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
	done.Status = model.StatusPublished
	saveExecutionTask(t, fixture, done)
	if err := app.TaskAction(ctx, done.ID, "discard"); err == nil || !IsActionConflict(err) {
		t.Fatalf("discard before archive = %v; want ineligible", err)
	}
	if err := app.TaskAction(ctx, done.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	saved = loadTask(t, fixture.state, done.ID)
	if saved.Lifecycle.ArchivedAt == nil {
		t.Fatalf("archived task = %+v", saved)
	}
	if err := app.TaskAction(ctx, done.ID, "discard"); err != nil {
		t.Fatal(err)
	}
	saved = loadTask(t, fixture.state, done.ID)
	if saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("discarded task = %+v", saved)
	}
	if _, err := os.Stat(done.Workspace); !os.IsNotExist(err) {
		t.Fatalf("discarded workspace still present: %v", err)
	}
	for _, action := range loadTask(t, fixture.state, done.ID).AllowedActions() {
		t.Fatalf("discarded task still offers %s", action)
	}
}

func TestRetryOnStaleBaseStaysBlocked(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusBlocked
	saveExecutionTask(t, fixture, task)
	git(t, fixture.repo, "commit", "--allow-empty", "-m", "External work")
	git(t, fixture.repo, "push", "origin", fixture.cfg.DefaultBranch)
	err := app.TaskAction(context.Background(), task.ID, "retry")
	if err == nil || model.BlockedReasonFromError(err) != model.BlockedReasonStaleBase {
		t.Fatalf("stale retry = %v; want the recorded stale-base failure", err)
	}
	saved := loadTask(t, fixture.state, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonStaleBase {
		t.Fatalf("stale retry outcome = %+v", saved)
	}
	if saved.Attempts != 0 {
		t.Fatalf("stale retry consumed an attempt: %+v", saved)
	}
}
