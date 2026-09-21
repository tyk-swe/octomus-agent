package engine

// Ports of the M6-allocated tests/hardening.rs task-control cases: remote
// preflights release the gate, concurrent actions never clobber each other,
// retries adopt live policy and start a fresh repair-round budget. These run
// at engine level (App.TaskAction) instead of the HTTP router, which is M7.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestShutdownOwnsRetryPreflight(t *testing.T) {
	fixture, app, task := heldPreflightFixture(t)
	finished := make(chan error, 1)
	go func() { finished <- app.TaskAction(context.Background(), task.ID, "retry") }()
	waitForPreflights(t, fixture, 1)
	joined := make(chan struct{})
	go func() { app.wg.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("retry preflight was not registered with shutdown")
	case <-time.After(100 * time.Millisecond):
	}
	app.Shutdown()
	saved := durableTask(t, fixture, task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil || saved.Attempts != task.Attempts {
		t.Fatalf("shutdown returned before retry recorded cancellation: %+v", saved)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled retry succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not return after shutdown")
	}
	// A caller may close the database as soon as Shutdown returns. Every new
	// action must reject before even reading it.
	if err := fixture.state.Close(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"retry", "reconcile", "cancel", "supersede", "archive", "discard"} {
		if err := app.TaskAction(context.Background(), task.ID, action); !errors.Is(err, context.Canceled) {
			t.Errorf("%s after shutdown = %v; want cancellation", action, err)
		}
	}
}

func TestShutdownWaitsForPublicationReconciliation(t *testing.T) {
	fixture := newExecutionFixture(t)
	task := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusBlocked
	task.BlockedReason = blockedReasonPtr(model.BlockedReasonPublicationUncertain)
	saveExecutionTask(t, fixture, task)
	heldUploadPack(t, fixture)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	finished := make(chan error, 1)
	go func() { finished <- app.TaskAction(context.Background(), task.ID, "reconcile") }()
	waitForPreflights(t, fixture, 1)

	app.Shutdown()
	// These assertions deliberately precede waiting for TaskAction: shutdown
	// must itself guarantee durable completion and removal of runtime ownership.
	saved := durableTask(t, fixture, task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil || saved.OutputCommit == nil || *saved.OutputCommit != *task.OutputCommit {
		t.Errorf("shutdown returned before reconciliation recorded its outcome: %+v", saved)
	}
	app.runtimeMu.Lock()
	active := len(app.runtime.tasks) != 0 || app.runtime.reconcilingPublication
	app.runtimeMu.Unlock()
	if active {
		t.Error("shutdown returned with reconciliation still active")
	}
	events, err := fixture.state.Events(&task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconciled := false
	for _, event := range events {
		if event.Kind == "operator" && event.Message == "reconcile" {
			reconciled = true
		}
	}
	if !reconciled {
		t.Error("shutdown returned before the reconciliation operator event")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconciliation did not return after shutdown")
	}
	if err := app.TaskAction(context.Background(), task.ID, "reconcile"); !errors.Is(err, context.Canceled) {
		t.Fatalf("reconciliation after shutdown = %v; want cancellation", err)
	}
	if current := durableTask(t, fixture, task.ID); !sameRecordJSON(&saved, &current) {
		t.Fatal("reconciliation after shutdown changed the durable task")
	}
}

// heldPreflightFixture mirrors the Rust held_preflight_fixture: a blocked
// durable task whose remote preflights hold behind a controlled upload-pack.
// The app is created but not resumed — controls run against durable state
// exactly like the reference API exercised through the router.
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

func durableTask(t *testing.T, fixture *planningFixture, id string) model.Task {
	t.Helper()
	task, err := store.Get[model.Task](fixture.state, "task", id)
	if err != nil || task == nil {
		t.Fatalf("durable task %s: %v", id, err)
	}
	return *task
}

// TestConcurrentRetriesQueueOnlyOneAttempt ports
// concurrent_retries_queue_only_one_attempt: two retries interleave at the
// released gate; only the first queues an attempt.
func TestConcurrentRetriesQueueOnlyOneAttempt(t *testing.T) {
	fixture, app, task := heldPreflightFixture(t)
	results := make(chan error, 2)
	go func() { results <- app.TaskAction(context.Background(), task.ID, "retry") }()
	go func() { results <- app.TaskAction(context.Background(), task.ID, "retry") }()
	waitForPreflights(t, fixture, 2)
	releasePreflight(t, fixture)
	outcomes := []error{<-results, <-results}
	succeeded, conflicted := 0, 0
	for _, err := range outcomes {
		if err == nil {
			succeeded++
		} else if IsActionConflict(err) {
			conflicted++
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent retries = %v; want one success and one conflict", outcomes)
	}
	saved := durableTask(t, fixture, task.ID)
	if saved.Status != model.StatusQueued || saved.Attempts != 1 {
		t.Fatalf("retried task = %+v; want queued with one attempt", saved)
	}
}

// TestRetryStartsAFreshRepairRoundBudget ports
// retry_starts_a_fresh_repair_round_budget: the new attempt budgets repairs
// from the recorded reviews while retaining the earlier evidence.
func TestRetryStartsAFreshRepairRoundBudget(t *testing.T) {
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
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if saved := durableTask(t, fixture, task.ID); saved.Status == model.StatusQueued {
			if saved.Attempts != 1 || saved.ReviewBaseline != 2 {
				t.Fatalf("retried task = %+v; want attempt 1 with review baseline 2", saved)
			}
			if len(saved.Reviews) != 2 || saved.AttemptReviews() != 0 {
				t.Fatalf("retry lost retained evidence: %+v", saved)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("retry did not queue: %+v", durableTask(t, fixture, task.ID))
}

// TestRetryRechecksPolicyAfterRemoteChecks ports
// retry_rechecks_policy_after_remote_checks: a policy change during the
// released remote check is a conflict, and the durable task is untouched.
func TestRetryRechecksPolicyAfterRemoteChecks(t *testing.T) {
	fixture, app, task := heldPreflightFixture(t)
	done := make(chan error, 1)
	go func() { done <- app.TaskAction(context.Background(), task.ID, "retry") }()
	waitForPreflights(t, fixture, 1)
	// The live policy tightens while the remote check is in flight.
	cfg, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxRetries = 0
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	releasePreflight(t, fixture)
	if err := <-done; err == nil || !IsActionConflict(err) {
		t.Fatalf("retry under changed policy = %v; want conflict", err)
	}
	saved := durableTask(t, fixture, task.ID)
	if !sameRecordJSON(&saved, &task) {
		t.Fatalf("conflicted retry rewrote the durable task: %+v", saved)
	}
}

// TestRemotePreflightsReleaseControlsAndPreserveConcurrentTaskActions ports
// remote_preflights_release_controls_and_preserve_concurrent_task_actions:
// controls do not wait on held Git work, and a stale retry/reconcile cannot
// overwrite a concurrent mutation.
func TestRemotePreflightsReleaseControlsAndPreserveConcurrentTaskActions(t *testing.T) {
	for _, action := range []string{"retry", "reconcile"} {
		for _, scenario := range []struct {
			mutation    string
			remoteFails bool
		}{{"cancel", false}, {"archive", true}} {
			t.Run(action+"/"+scenario.mutation, func(t *testing.T) {
				fixture, app, task := heldPreflightFixture(t)
				if action == "reconcile" {
					reason := model.BlockedReasonRemoteConflict
					task.BlockedReason = &reason
					saveExecutionTask(t, fixture, task)
				}
				done := make(chan error, 1)
				go func() { done <- app.TaskAction(context.Background(), task.ID, action) }()
				waitForPreflights(t, fixture, 1)
				// An unrelated running task plus operator controls must all
				// complete while the remote check holds.
				unrelated := executionTask(t, fixture, fixture.cfg.DefaultBranch)
				unrelated.Status = model.StatusExecuting
				saveExecutionTask(t, fixture, unrelated)
				unrelatedCtx, unrelatedCancel := context.WithCancel(context.Background())
				app.runtimeMu.Lock()
				app.runtime.tasks[unrelated.ID] = taskJob{branch: unrelated.Branch, cancel: unrelatedCancel}
				app.runtimeMu.Unlock()
				controlDeadline := time.Now().Add(15 * time.Second)
				paused := make(chan error, 1)
				go func() { paused <- app.Pause() }()
				select {
				case err := <-paused:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(controlDeadline.Sub(time.Now())):
					t.Fatal("pause waited for Git")
				}
				canceled := make(chan error, 1)
				go func() { canceled <- app.TaskAction(context.Background(), unrelated.ID, "cancel") }()
				select {
				case err := <-canceled:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(controlDeadline.Sub(time.Now())):
					t.Fatal("unrelated cancel waited for Git")
				}
				mutated := make(chan error, 1)
				go func() { mutated <- app.TaskAction(context.Background(), task.ID, scenario.mutation) }()
				select {
				case err := <-mutated:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(controlDeadline.Sub(time.Now())):
					t.Fatalf("%s waited for Git", scenario.mutation)
				}
				if unrelatedCtx.Err() == nil {
					t.Fatal("unrelated cancel did not reach the running worker")
				}
				control, err := app.Control()
				if err != nil || !control.Paused {
					t.Fatalf("control = %+v, %v", control, err)
				}
				changed := durableTask(t, fixture, task.ID)
				if scenario.remoteFails {
					writeFixtureMode(t, fixture, "fail")
				}
				releasePreflight(t, fixture)
				if err := <-done; err == nil || !IsActionConflict(err) {
					t.Fatalf("stale %s = %v; want conflict", action, err)
				}
				saved := durableTask(t, fixture, task.ID)
				if !sameRecordJSON(&saved, &changed) {
					t.Fatalf("stale %s overwrote %s: %+v", action, scenario.mutation, saved)
				}
			})
		}
	}
}

// TestRetryPreflightAdoptsTheCurrentCommandTimeout ports
// retry_preflight_adopts_the_current_command_timeout: the remote preflight
// runs under the live attempt policy, never the task's saved snapshot.
func TestRetryPreflightAdoptsTheCurrentCommandTimeout(t *testing.T) {
	fixture := newExecutionFixture(t)
	// A remote read slower than the first configured command timeout.
	script := "#!/bin/sh\nsleep 2\nexec git-upload-pack \"$@\"\n"
	path := filepath.Join(fixture.root, "slow-upload-pack")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	command(t, fixture.repo, "/usr/bin/git", "config", "remote.origin.uploadpack", path)
	cfg := fixture.cfg.Clone()
	cfg.CommandTimeoutSeconds = 1
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	task := executionTask(t, fixture, cfg.DefaultBranch)
	// The saved snapshot holds the generous timeout the task was created with;
	// the retry must adopt the live (tighter) one.
	policy := model.AttemptPolicyFromConfig(fixture.cfg)
	task.AttemptPolicy = &policy
	task.Status = model.StatusBlocked
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	err := app.TaskAction(context.Background(), task.ID, "retry")
	if err == nil || IsActionConflict(err) {
		t.Fatalf("retry under the live 1s timeout = %v; want the remote error", err)
	}
	if saved := durableTask(t, fixture, task.ID); saved.Attempts != 0 {
		t.Fatalf("failed preflight consumed an attempt: %+v", saved)
	}
	cfg.CommandTimeoutSeconds = 5
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatalf("retry under the relaxed timeout failed: %v", err)
	}
	saved := durableTask(t, fixture, task.ID)
	if saved.Status != model.StatusQueued || saved.Attempts != 1 {
		t.Fatalf("relaxed retry = %+v; want queued attempt 1", saved)
	}
	if saved.AttemptPolicy == nil || saved.AttemptPolicy.CommandTimeoutSeconds != 5 {
		t.Fatalf("retry did not adopt the live policy: %+v", saved.AttemptPolicy)
	}
}

// TestTaskActionSupersedeArchiveDiscard covers the lifecycle controls beyond
// retry/reconcile: supersede marks durable rediscovery intent, archive moves a
// finished task out of the active set, and discard removes the owned workspace
// and records disposal — each gated by AllowedActions.
func TestTaskActionSupersedeArchiveDiscard(t *testing.T) {
	fixture := newExecutionFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	ctx := context.Background()

	// A stale-base block offers supersede; superseding preserves the durable
	// record while requesting fresh evidence on the next cycle.
	stale := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	stale.Status = model.StatusBlocked
	reason := model.BlockedReasonStaleBase
	stale.BlockedReason = &reason
	saveExecutionTask(t, fixture, stale)
	if err := app.TaskAction(ctx, stale.ID, "supersede"); err != nil {
		t.Fatal(err)
	}
	saved := durableTask(t, fixture, stale.ID)
	if saved.Status != model.StatusCancelled || !saved.RediscoveryRequested || saved.RediscoveryResult != nil {
		t.Fatalf("superseded task = %+v", saved)
	}

	// A published task archives, then discards; discard removes only the
	// task's owned workspace directory.
	done := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
	done.Status = model.StatusPublished
	saveExecutionTask(t, fixture, done)
	if err := app.TaskAction(ctx, done.ID, "discard"); err == nil || !IsActionConflict(err) {
		t.Fatalf("discard before archive = %v; want ineligible", err)
	}
	if err := app.TaskAction(ctx, done.ID, "archive"); err != nil {
		t.Fatal(err)
	}
	saved = durableTask(t, fixture, done.ID)
	if saved.Lifecycle.ArchivedAt == nil {
		t.Fatalf("archived task = %+v", saved)
	}
	if err := app.TaskAction(ctx, done.ID, "discard"); err != nil {
		t.Fatal(err)
	}
	saved = durableTask(t, fixture, done.ID)
	if saved.Lifecycle.DiscardedAt == nil {
		t.Fatalf("discarded task = %+v", saved)
	}
	if _, err := os.Stat(done.Workspace); !os.IsNotExist(err) {
		t.Fatalf("discarded workspace still present: %v", err)
	}
	for _, action := range durableTask(t, fixture, done.ID).AllowedActions() {
		t.Fatalf("discarded task still offers %s", action)
	}
}

// TestRetryOnStaleBaseStaysBlocked ports the stale-retry case: remote
// movement since the recorded base fails the retry preflight and preserves
// the stale evidence instead of queuing an attempt.
func TestRetryOnStaleBaseStaysBlocked(t *testing.T) {
	fixture := newExecutionFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusBlocked
	saveExecutionTask(t, fixture, task)
	// The remote target and default branches move past the recorded revisions.
	command(t, fixture.repo, "/usr/bin/git", "commit", "--allow-empty", "-m", "External work")
	command(t, fixture.repo, "/usr/bin/git", "push", "origin", fixture.cfg.DefaultBranch)
	err := app.TaskAction(context.Background(), task.ID, "retry")
	if err == nil || model.BlockedReasonFromError(err) != model.BlockedReasonStaleBase {
		t.Fatalf("stale retry = %v; want the recorded stale-base failure", err)
	}
	saved := durableTask(t, fixture, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonStaleBase {
		t.Fatalf("stale retry outcome = %+v", saved)
	}
	if saved.Attempts != 0 {
		t.Fatalf("stale retry consumed an attempt: %+v", saved)
	}
}
