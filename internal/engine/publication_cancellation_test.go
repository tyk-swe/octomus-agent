package engine

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

func TestAcceptedCancellationBeforePublicationDoesNotCreateCheckpoint(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	task := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
	revision := *task.OutputCommit
	task.OutputCommit = nil
	task.Status = model.StatusVerifying
	saveExecutionTask(t, fixture, task)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	ctx, cancel := context.WithCancel(app.Context())
	defer cancel()
	app.runtimeMu.Lock()
	app.runtime.tasks[task.ID] = taskJob{branch: task.Branch, cancel: cancel}
	app.runtimeMu.Unlock()

	// Verification and the final base check have completed. Cancellation lands
	// immediately before the worker attempts to record the reviewed revision.
	if err := app.TaskAction(context.Background(), task.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("accepted cancellation did not stop the worker")
	}
	if err := app.superviseExecution(ctx, task, func(workCtx context.Context, active *model.Task) error {
		return app.publishReviewed(workCtx, active, revision)
	}); err != nil {
		t.Fatal(err)
	}
	app.runtimeMu.Lock()
	delete(app.runtime.tasks, task.ID)
	app.runtimeMu.Unlock()
	saved := loadTask(t, fixture.state, task.ID)
	if saved.Status != model.StatusCancelled || saved.OutputCommit != nil || saved.BlockedReason != nil || optionalText(saved.Error) != "Cancelled by the operator" {
		t.Fatalf("accepted cancellation became a publication checkpoint: status=%s output=%v reason=%v error=%s", saved.Status, saved.OutputCommit, saved.BlockedReason, optionalText(saved.Error))
	}
	if entries := publications(t, fixture); len(entries) != 0 {
		t.Fatalf("cancelled publication wrote remote actions: %+v", entries)
	}
	app.Shutdown()
	restarted := New(fixture.state, fixture.dataDir)
	t.Cleanup(restarted.Shutdown)
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	if recovered := loadTask(t, fixture.state, task.ID); !wirejson.Equal(saved, recovered) {
		t.Fatalf("restart changed the cancelled task: %+v", recovered)
	}
}

func TestRecordedPublicationRefusesCancellationWithoutStoppingWorker(t *testing.T) {
	// Register before the package's parallel database tests start. This barrier
	// is inside the real store update reached by the full execution pipeline.
	checkpoint := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCheckpoint := func() { releaseOnce.Do(func() { close(release) }) }
	hook := "publication_checkpoint_" + strings.ReplaceAll(model.ID(), "-", "")
	if err := sqlite.RegisterScalarFunction(hook, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		close(checkpoint)
		<-release
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture := newScriptedFixture(t, withGitHubIdentity())
	fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Answer: "Implemented", Effect: writeFile("feature.txt", "fixed\n")})
	fixture.script.Answer(fixture.routes.Reviewer, cleanReview("Complete"))
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER hold_publication AFTER UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='publishing'
		BEGIN SELECT %s(); END`, task.ID, hook))
	app := fixture.pausedApp(t)
	t.Cleanup(releaseCheckpoint)
	// Start a normally admitted task directly to keep unrelated scheduler
	// reads from waiting behind the deliberate SQLite barrier.
	task.Status = model.StatusExecuting
	saveExecutionTask(t, fixture.planningFixture, task)
	app.runTask(task)
	select {
	case <-checkpoint:
	case <-time.After(30 * time.Second):
		t.Fatal("execution did not reach its publication checkpoint")
	}
	cancelResult := make(chan error, 1)
	go func() { cancelResult <- app.TaskAction(context.Background(), task.ID, "cancel") }()
	releaseCheckpoint()
	select {
	case err := <-cancelResult:
		if !IsActionConflict(err) {
			t.Fatalf("cancellation after the checkpoint = %v; want conflict", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancellation did not finish")
	}
	app.wg.Wait()
	saved := loadTask(t, fixture.state, task.ID)
	if saved.Status != model.StatusPublished || saved.OutputCommit == nil || saved.PRNumber == nil {
		t.Fatalf("refused cancellation interrupted publication: status=%s output=%v error=%s", saved.Status, saved.OutputCommit, optionalText(saved.Error))
	}
	if marked, err := fixture.state.MarkerSet("cancel", task.ID); err != nil || marked {
		t.Fatalf("refused cancellation left a marker: %t, %v", marked, err)
	}
	if entries := publications(t, fixture.planningFixture); len(entries) != 1 || entries[0]["action"] != "create" {
		t.Fatalf("publication actions = %+v; want exactly one create", entries)
	}
	if head := remoteHead(t, fixture.planningFixture, saved.Branch); head != *saved.OutputCommit {
		t.Fatalf("remote branch = %s; want reviewed output %s", head, *saved.OutputCommit)
	}
	assertAdmissions(t, fixture.state, 2, "executor + reviewer")
	assertNoOpenClients(t, fixture.script)
}

func TestEligibleCancellationWinsBeforeConcurrentPublicationCheckpoint(t *testing.T) {
	// Hold a normally admitted cancellation after eligibility is read. Let the
	// actual worker finish review, verification and its base check while that
	// action still owns admission. Publication must not overtake cancellation.
	checkpoint := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCheckpoint := func() { releaseOnce.Do(func() { close(release) }) }
	hook := "cancellation_first_" + strings.ReplaceAll(model.ID(), "-", "")
	if err := sqlite.RegisterScalarFunction(hook, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		close(checkpoint)
		<-release
		return int64(1), nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture := newScriptedFixture(t, withGitHubIdentity())
	reviewer := runnertest.NewGate()
	fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Answer: "Implemented", Effect: writeFile("feature.txt", "fixed\n")})
	fixture.script.Queue(fixture.routes.Reviewer, runnertest.Reply{Answer: cleanReview("Complete"), Gate: reviewer})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusExecuting
	saveExecutionTask(t, fixture.planningFixture, task)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER hold_publication AFTER UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='publishing'
		BEGIN SELECT %s(); END`, task.ID, hook))
	app := fixture.pausedApp(t)
	t.Cleanup(releaseCheckpoint)
	t.Cleanup(reviewer.Release)
	app.runTask(task)
	select {
	case <-reviewer.Entered():
	case <-time.After(30 * time.Second):
		t.Fatal("execution did not reach the held reviewer")
	}
	app.runtimeMu.Lock()
	job := app.runtime.tasks[task.ID]
	cancel := job.cancel
	job.cancel = func() {
		reviewer.Release()
		// A checkpoint on the broken path wakes this immediately. The bounded
		// window lets the worker contend with the admitted action; on the fixed
		// path it cannot checkpoint until this callback returns. Never wait for
		// a checkpoint unconditionally: that would deadlock correct exclusion.
		select {
		case <-checkpoint:
		case <-time.After(5 * time.Second):
		}
		cancel()
		releaseCheckpoint()
	}
	app.runtime.tasks[task.ID] = job
	app.runtimeMu.Unlock()

	cancelErr := app.TaskAction(context.Background(), task.ID, "cancel")
	finished := make(chan struct{})
	go func() {
		app.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled worker did not finish")
	}
	saved := loadTask(t, fixture.state, task.ID)
	if cancelErr != nil {
		t.Fatalf("eligible cancellation lost to publication: %v; status=%s output=%v error=%s", cancelErr, saved.Status, saved.OutputCommit, optionalText(saved.Error))
	}
	if saved.Status != model.StatusCancelled || saved.OutputCommit != nil || saved.BlockedReason != nil || optionalText(saved.Error) != "Cancelled by the operator" {
		t.Fatalf("accepted cancellation created a publication checkpoint: status=%s output=%v reason=%v error=%s", saved.Status, saved.OutputCommit, saved.BlockedReason, optionalText(saved.Error))
	}
	if marked, err := fixture.state.MarkerSet("cancel", task.ID); err != nil || !marked {
		t.Fatalf("accepted cancellation marker = %t, %v", marked, err)
	}
	if entries := publications(t, fixture.planningFixture); len(entries) != 0 {
		t.Fatalf("cancelled execution wrote publication actions: %+v", entries)
	}
	assertAdmissions(t, fixture.state, 2, "executor + reviewer")
	assertNoOpenClients(t, fixture.script)
}
