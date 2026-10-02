package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestPublicationCheckpointWriteFailureRecovery(t *testing.T) {
	t.Parallel()
	for _, check := range []struct {
		name               string
		existingCheckpoint bool
		failEvent          bool
	}{
		{name: "fresh checkpoint write"},
		{name: "existing checkpoint write", existingCheckpoint: true},
		{name: "event after saved checkpoint", failEvent: true},
	} {
		t.Run(check.name, func(t *testing.T) {
			fixture := newExecutionFixture(t)
			task := checkpointedTask(t, fixture, fixture.cfg.DefaultBranch)
			revision := *task.OutputCommit
			if !check.existingCheckpoint {
				task.OutputCommit = nil
			}
			task.Status = model.StatusVerifying
			task.UpdatedAt = "2026-01-01T00:00:00Z"
			saveExecutionTask(t, fixture, task)
			before := loadTask(t, fixture.state, task.ID)
			if check.failEvent {
				schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER fail_checkpoint_event BEFORE INSERT ON events
					WHEN NEW.entity_id='%s' AND NEW.kind='status' AND NEW.message='Publishing'
					BEGIN SELECT RAISE(ABORT, 'synthetic checkpoint failure'); END`, task.ID))
			} else {
				schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER fail_checkpoint_write BEFORE UPDATE ON records
					WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='publishing'
					BEGIN SELECT RAISE(ABORT, 'synthetic checkpoint failure'); END`, task.ID))
			}
			type outcome struct {
				err  error
				task model.Task
			}
			failed := make(chan outcome, 1)
			resume := make(chan struct{})
			var resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			var app *App
			app = New(fixture.state, fixture.dataDir, WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, active model.Task) error {
				return app.superviseExecution(ctx, active, func(workCtx context.Context, current *model.Task) error {
					err := app.publishReviewed(workCtx, current, revision)
					failed <- outcome{err: err, task: current.Clone()}
					<-resume
					return err
				})
			})))
			t.Cleanup(app.Shutdown)
			t.Cleanup(release)
			app.runTask(task)
			var result outcome
			select {
			case result = <-failed:
			case <-time.After(10 * time.Second):
				t.Fatal("publication did not reach the injected failure")
			}
			if result.err == nil || !strings.Contains(result.err.Error(), "synthetic checkpoint failure") {
				t.Fatalf("publication error = %v", result.err)
			}
			stored := loadTask(t, fixture.state, task.ID)
			if check.failEvent {
				if stored.Status != model.StatusPublishing || stored.OutputCommit == nil || *stored.OutputCommit != revision || result.task.OutputCommit == nil || result.task.Status != model.StatusPublishing {
					t.Errorf("status-event failure lost a saved checkpoint: stored=%+v local=%+v", stored, result.task)
				}
			} else {
				if stored.Status != before.Status || optionalText(stored.OutputCommit) != optionalText(before.OutputCommit) || stored.UpdatedAt != before.UpdatedAt {
					t.Fatal("failed checkpoint write changed durable task fields")
				}
				if result.task.Status != before.Status || optionalText(result.task.OutputCommit) != optionalText(before.OutputCommit) || result.task.UpdatedAt != before.UpdatedAt {
					t.Errorf("failed checkpoint write changed local fields: status=%s output=%v updated=%s", result.task.Status, result.task.OutputCommit, result.task.UpdatedAt)
				}
			}
			cancelErr := app.TaskAction(context.Background(), task.ID, "cancel")
			cancellable := !check.existingCheckpoint && !check.failEvent
			if cancellable && cancelErr != nil || !cancellable && !IsActionConflict(cancelErr) {
				t.Errorf("cancellation after failed transition = %v; cancellable=%t", cancelErr, cancellable)
			}
			release()
			finished := make(chan struct{})
			go func() { app.wg.Wait(); close(finished) }()
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("worker did not finish after the failed checkpoint")
			}
			saved := loadTask(t, fixture.state, task.ID)
			if cancellable {
				if saved.Status != model.StatusCancelled || saved.OutputCommit != nil || optionalText(saved.Error) != "Cancelled by the operator" {
					t.Fatalf("accepted cancellation was overwritten by a failed checkpoint: status=%s output=%v error=%s", saved.Status, saved.OutputCommit, optionalText(saved.Error))
				}
			} else if saved.Status != model.StatusBlocked || saved.OutputCommit == nil || *saved.OutputCommit != revision {
				t.Fatalf("failed attempt discarded a durable checkpoint: %+v", saved)
			}
			if entries := publications(t, fixture); len(entries) != 0 {
				t.Fatalf("failed checkpoint attempted remote publication: %+v", entries)
			}
		})
	}
}
