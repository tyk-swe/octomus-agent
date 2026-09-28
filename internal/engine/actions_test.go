package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestShutdownOwnsRetryPreflight(t *testing.T) {
	t.Parallel()
	fixture, app, task := heldPreflightFixture(t)
	finished := make(chan error, 1)
	go func() { finished <- app.TaskAction(context.Background(), task.ID, "retry") }()
	waitForPreflights(t, fixture, 1)
	joined := make(chan struct{})
	go func() {
		app.gate.Lock()
		app.gate.Unlock()
		app.wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
		t.Fatal("retry preflight was not registered with shutdown")
	case <-time.After(100 * time.Millisecond):
	}
	app.Shutdown()
	saved := loadTask(t, fixture.state, task.ID)
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
	t.Parallel()
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
	saved := loadTask(t, fixture.state, task.ID)
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
	if current := loadTask(t, fixture.state, task.ID); !wirejson.Equal(&saved, &current) {
		t.Fatal("reconciliation after shutdown changed the durable task")
	}
}

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

func TestConcurrentRetriesQueueOnlyOneAttempt(t *testing.T) {
	t.Parallel()
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
	saved := loadTask(t, fixture.state, task.ID)
	if saved.Status != model.StatusQueued || saved.Attempts != 1 {
		t.Fatalf("retried task = %+v; want queued with one attempt", saved)
	}
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

func TestRetryRechecksPolicyAfterRemoteChecks(t *testing.T) {
	t.Parallel()
	fixture, app, task := heldPreflightFixture(t)
	done := make(chan error, 1)
	go func() { done <- app.TaskAction(context.Background(), task.ID, "retry") }()
	waitForPreflights(t, fixture, 1)
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
	saved := loadTask(t, fixture.state, task.ID)
	if !wirejson.Equal(&saved, &task) {
		t.Fatalf("conflicted retry rewrote the durable task: %+v", saved)
	}
}

func TestRemotePreflightsReleaseControlsAndPreserveConcurrentTaskActions(t *testing.T) {
	t.Parallel()
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
				changed := loadTask(t, fixture.state, task.ID)
				if scenario.remoteFails {
					writeFixtureMode(t, fixture, "fail")
				}
				releasePreflight(t, fixture)
				if err := <-done; err == nil || !IsActionConflict(err) {
					t.Fatalf("stale %s = %v; want conflict", action, err)
				}
				saved := loadTask(t, fixture.state, task.ID)
				if !wirejson.Equal(&saved, &changed) {
					t.Fatalf("stale %s overwrote %s: %+v", action, scenario.mutation, saved)
				}
			})
		}
	}
}

func TestRetryPreflightAdoptsTheCurrentCommandTimeout(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	script := "#!/bin/sh\nsleep 2\nexec git-upload-pack \"$@\"\n"
	path := filepath.Join(fixture.root, "slow-upload-pack")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, fixture.repo, "config", "remote.origin.uploadpack", path)
	cfg := fixture.cfg.Clone()
	cfg.CommandTimeoutSeconds = 1
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	task := executionTask(t, fixture, cfg.DefaultBranch)
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
	if saved := loadTask(t, fixture.state, task.ID); saved.Attempts != 0 {
		t.Fatalf("failed preflight consumed an attempt: %+v", saved)
	}
	cfg.CommandTimeoutSeconds = 30
	if err := fixture.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatalf("retry under the relaxed timeout failed: %v", err)
	}
	saved := loadTask(t, fixture.state, task.ID)
	if saved.Status != model.StatusQueued || saved.Attempts != 1 {
		t.Fatalf("relaxed retry = %+v; want queued attempt 1", saved)
	}
	if saved.AttemptPolicy == nil || saved.AttemptPolicy.CommandTimeoutSeconds != 30 {
		t.Fatalf("retry did not adopt the live policy: %+v", saved.AttemptPolicy)
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

func TestUnknownActionsAreNotReportedAsEligibilityConflicts(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	task := queuedTask(cfg, "blocked-task", cfg.DefaultBranch, cfg.BranchPrefix+"blocked-task")
	task.Status = model.StatusBlocked
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: model.ID(), Number: 1, Status: model.CycleRunning,
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{},
		Sessions: []model.Session{}, Repository: cfg.GitHubRepo,
	}
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)

	err := app.TaskAction(context.Background(), task.ID, "bogus")
	if !errors.Is(err, ErrUnknownTaskAction) || IsActionConflict(err) {
		t.Fatalf("bogus task action = %v; want ErrUnknownTaskAction", err)
	}
	if saved := loadTask(t, state, task.ID); !wirejson.Equal(&saved, &task) {
		t.Fatalf("unknown task action changed the record: %+v", saved)
	}
	err = app.CycleAction(cycle.ID, "bogus")
	if !errors.Is(err, ErrUnknownCycleAction) || IsActionConflict(err) {
		t.Fatalf("bogus action on a running cycle = %v; want ErrUnknownCycleAction", err)
	}
	for _, id := range []string{task.ID, cycle.ID} {
		if events, err := state.Events(&id); err != nil || len(events) != 0 {
			t.Fatalf("unknown action recorded events for %s: %+v, %v", id, events, err)
		}
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

func TestRetryChecksARecordedWorkspaceBeforeQueuing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		action  string
		prepare func(t *testing.T, fixture *scriptedFixture, task model.Task, ws string) string
		queued  bool
	}{
		{"initialized clone", "retry", cloneAtSource, true},
		{"partial clone", "retry", func(t *testing.T, _ *scriptedFixture, _ model.Task, ws string) string {
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			return ""
		}, false},
		{"edited clone", "retry", func(t *testing.T, fixture *scriptedFixture, task model.Task, ws string) string {
			base := cloneAtSource(t, fixture, task, ws)
			if err := os.WriteFile(filepath.Join(ws, "edit.txt"), []byte("before any session\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return base
		}, false},
		{"partial clone reconcile", "reconcile", func(t *testing.T, _ *scriptedFixture, _ model.Task, ws string) string {
			if err := os.MkdirAll(ws, 0o755); err != nil {
				t.Fatal(err)
			}
			return ""
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScriptedFixture(t)
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			task.Status = model.StatusBlocked
			reason := model.BlockedReasonUnknown
			if test.action == "reconcile" {
				reason = model.BlockedReasonRemoteConflict
			}
			task.BlockedReason = &reason
			task.Workspace = filepath.Join(fixture.dataDir, "tasks", task.ID, "workspace")
			task.ComparisonBase = test.prepare(t, fixture, task, task.Workspace)
			saveExecutionTask(t, fixture.planningFixture, task)
			app := fixture.newApp(t)

			err := app.TaskAction(context.Background(), task.ID, test.action)
			saved := loadTask(t, fixture.state, task.ID)
			switch {
			case test.queued:
				if err != nil || saved.Status != model.StatusQueued || saved.Attempts != 1 {
					t.Fatalf("retry in an initialized clone = %v; status %s, attempts %d", err, saved.Status, saved.Attempts)
				}
				return
			case test.action == "retry":
				if err == nil || !IsActionConflict(err) || model.BlockedReasonFromError(err) != model.BlockedReasonWorkspaceInvalid {
					t.Fatalf("retry = %v; want a workspace_invalid conflict", err)
				}
			default:
				if err != nil {
					t.Fatalf("reconcile = %v", err)
				}
			}
			if !blockedAs(saved, model.BlockedReasonWorkspaceInvalid) || saved.Attempts != task.Attempts {
				t.Fatalf("%s outcome = status %s, reason %v, attempts %d", test.action, saved.Status, saved.BlockedReason, saved.Attempts)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			if calls := fixture.script.Calls(); len(calls) != 0 {
				t.Fatalf("a refused %s reached the runner: %+v", test.action, calls)
			}
			assertAdmissions(t, fixture.state, 0, "a refused "+test.action+" admits nothing")
		})
	}
}

func cloneAtSource(t *testing.T, fixture *scriptedFixture, task model.Task, ws string) string {
	t.Helper()
	if err := gitops.CloneAt(context.Background(), fixture.cfg, ws, task.SourceRevision); err != nil {
		t.Fatal(err)
	}
	return task.SourceRevision
}
