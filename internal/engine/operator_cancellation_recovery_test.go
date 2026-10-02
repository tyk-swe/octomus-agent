package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestQueuedCancellationRecoversRefusedStatusWrite(t *testing.T) {
	t.Parallel()
	for _, recoverEngine := range []bool{false, true} {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("engine_recovery=%t/existing=%t", recoverEngine, existing), func(t *testing.T) {
				fixture := newScriptedFixture(t, withGitHubIdentity())
				target := fixture.cfg.DefaultBranch
				if existing {
					existingPrBranch(t, fixture.planningFixture)
					target = "octomus/existing"
				}
				fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Answer: "Implemented", Effect: writeFile("feature.txt", "fixed\n")})
				fixture.script.Answer(fixture.routes.Reviewer, cleanReview("Complete"))
				task := executionTask(t, fixture.planningFixture, target)
				if existing {
					task.Branch = target
					number := uint64(42)
					task.PRNumber = &number
					task.PRURL = stringPointer("https://github.com/fixture/project/pull/42")
				}
				policy := model.AttemptPolicyFromConfig(task.Config)
				task.AttemptPolicy = &policy
				saveExecutionTask(t, fixture.planningFixture, task)
				app := fixture.pausedApp(t)
				schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER fail_cancel BEFORE UPDATE ON records
					WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='cancelled'
					BEGIN SELECT RAISE(ABORT, 'synthetic cancellation refusal'); END`, task.ID))
				if err := app.TaskAction(context.Background(), task.ID, "cancel"); err == nil || !strings.Contains(err.Error(), "synthetic cancellation refusal") {
					t.Fatalf("cancel error = %v", err)
				}
				if queued := loadTask(t, fixture.state, task.ID); !wirejson.Equal(queued, task) {
					t.Fatal("refused cancellation changed the task record")
				}
				if marked, err := fixture.state.MarkerSet("cancel", task.ID); err != nil || !marked {
					t.Fatalf("cancel marker = %t, %v", marked, err)
				}
				// A pending cancellation must never acquire a fresh PR slot, even briefly.
				schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER reject_cancelled_reservation BEFORE INSERT ON pr_reservations
					WHEN NEW.task_id='%s' BEGIN SELECT RAISE(ABORT, 'cancelled task reserved a PR slot'); END`, task.ID))
				control := model.DefaultControl()
				control.SetMode(model.OperatingModeContinuous)
				control.NextCycleAt = time.Now().Add(time.Hour).Unix()
				if err := fixture.state.SaveControl(control); err != nil {
					t.Fatal(err)
				}
				if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "synthetic cancellation refusal") {
					t.Fatalf("dispatch under the remaining refusal = %v", err)
				}
				app.wg.Wait()
				assertAdmissions(t, fixture.state, 0, "a refused cancellation remains blocked before admission")
				schedulerSQL(t, fixture.state, "DROP TRIGGER fail_cancel")
				if recoverEngine {
					app.Shutdown()
					app = fixture.pausedApp(t)
					if err := app.Recover(); err != nil {
						t.Fatal(err)
					}
				}
				saved := driveTask(t, fixture.planningFixture, app, task.ID)
				expected := task.Clone()
				expected.Status, expected.UpdatedAt = model.StatusCancelled, saved.UpdatedAt
				if !wirejson.Equal(saved, expected) {
					t.Fatalf("cancellation changed preserved task state: status=%s attempts=%d policy=%+v", saved.Status, saved.Attempts, saved.AttemptPolicy)
				}
				if actions := saved.AllowedActions(); len(actions) != 2 || actions[0] != "archive" || actions[1] != "supersede" {
					t.Fatalf("cancelled task actions = %v", actions)
				}
				assertAdmissions(t, fixture.state, 0, "durable cancellation starts no model work")
				if reserved, err := fixture.state.HasPrReservation(task.ID); err != nil || reserved {
					t.Fatalf("cancelled task reservation = %t, %v", reserved, err)
				}
				if entries := publications(t, fixture.planningFixture); len(entries) != 0 {
					t.Fatalf("cancelled task published: %+v", entries)
				}
				if fixture.script.Pending(fixture.routes.Executor) != 1 || fixture.script.Pending(fixture.routes.Reviewer) != 1 {
					t.Fatal("cancelled task reached a runner")
				}
				assertNoOpenClients(t, fixture.script)
			})
		}
	}
}

func TestQueuedPublicationCheckpointIgnoresCancellationMarker(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t, withGitHubIdentity())
	task := checkpointedTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusPublishing
	saveExecutionTask(t, fixture.planningFixture, task)
	if err := fixture.state.MarkCancel(task.ID); err != nil {
		t.Fatal(err)
	}
	app := fixture.pausedApp(t)
	if err := app.Recover(); err != nil {
		t.Fatal(err)
	}
	queued := loadTask(t, fixture.state, task.ID)
	if queued.Status != model.StatusQueued || queued.OutputCommit == nil {
		t.Fatalf("publication checkpoint recovery = %+v", queued)
	}
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusPublished || saved.OutputCommit == nil || *saved.OutputCommit != *task.OutputCommit {
		t.Fatalf("durable checkpoint did not reconcile: %+v", saved)
	}
	if !wirejson.Equal(saved.Sessions, task.Sessions) || !wirejson.Equal(saved.Reviews, task.Reviews) || !wirejson.Equal(saved.Verification, task.Verification) {
		t.Fatal("publication recovery changed retained execution evidence")
	}
	assertAdmissions(t, fixture.state, 0, "checkpoint recovery publishes without another model turn")
	if entries := publications(t, fixture.planningFixture); len(entries) != 1 || entries[0]["action"] != "create" {
		t.Fatalf("checkpoint publication = %+v", entries)
	}
	assertNoOpenClients(t, fixture.script)
}

func TestInitializedQueuedCancellationPreservesRecoveryEvidence(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t, withGitHubIdentity())
	task := checkpointedTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.OutputCommit = nil
	task.Status = model.StatusQueued
	task.Attempts = 1
	policy := model.AttemptPolicyFromConfig(task.Config)
	task.AttemptPolicy = &policy
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.pausedApp(t)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER fail_cancel BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='cancelled'
		BEGIN SELECT RAISE(ABORT, 'synthetic cancellation refusal'); END`, task.ID))
	if err := app.TaskAction(context.Background(), task.ID, "cancel"); err == nil || !strings.Contains(err.Error(), "synthetic cancellation refusal") {
		t.Fatalf("cancel error = %v", err)
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER fail_cancel")
	app.Shutdown()
	recovered := fixture.pausedApp(t)
	if err := recovered.Recover(); err != nil {
		t.Fatal(err)
	}
	// Recovery restores the initialized queue's reservation. Dispatch must
	// settle its earlier cancellation and release that slot without more work.
	if reserved, err := fixture.state.HasPrReservation(task.ID); err != nil || !reserved {
		t.Fatalf("initialized recovery reservation = %t, %v", reserved, err)
	}
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = time.Now().Add(time.Hour).Unix()
	if err := fixture.state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture.planningFixture, recovered, task.ID)
	expected := task.Clone()
	expected.Status, expected.UpdatedAt = model.StatusCancelled, saved.UpdatedAt
	if !wirejson.Equal(saved, expected) {
		t.Fatalf("cancelled recovery changed retained execution state: %+v", saved)
	}
	assertAdmissions(t, fixture.state, 0, "initialized cancellation adds no model turns")
	if reserved, err := fixture.state.HasPrReservation(task.ID); err != nil || reserved {
		t.Fatalf("cancelled recovery reservation = %t, %v", reserved, err)
	}
	if entries := publications(t, fixture.planningFixture); len(entries) != 0 {
		t.Fatalf("cancelled recovery published: %+v", entries)
	}
	assertNoOpenClients(t, fixture.script)
}
