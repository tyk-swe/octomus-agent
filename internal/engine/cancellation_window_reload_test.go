package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestPreparationChangesDeferNewQueueWindow(t *testing.T) {
	t.Parallel()
	testutil.SkipVolumeUnderRace(t)
	for _, next := range []string{"cancelled task", "invalid cycle", "valid task"} {
		t.Run(next, func(t *testing.T) {
			fixture := newScriptedFixture(t, withGitHubIdentity())
			head := existingPrBranch(t, fixture.planningFixture)
			defaultHead := remoteHead(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			if err := fixture.cfg.Validate(true); err != nil {
				t.Fatal(err)
			}
			ground := model.Grounding{Revision: "source", PRs: []model.PullRequest{ownedPR("octomus/existing")}}
			app := fixture.pausedApp(t)
			// Real refused operator writes leave each task queued with a durable
			// marker. The 500 independent one-task cycles all respect policy.
			schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER fail_cancel BEFORE UPDATE ON records
				WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status')='cancelled'
				BEGIN SELECT RAISE(ABORT, 'synthetic cancellation refusal'); END`)
			mark := func(id string) {
				t.Helper()
				if err := app.TaskAction(context.Background(), id, "cancel"); err == nil || !strings.Contains(err.Error(), "synthetic cancellation refusal") {
					t.Fatalf("cancel %s error = %v", id, err)
				}
			}
			for i := 0; i < 500; i++ {
				task := queuedTask(fixture.cfg, fmt.Sprintf("cancel-%03d", i), "octomus/existing", "octomus/existing")
				task.CycleID = fmt.Sprintf("history-%03d", i)
				task.SourceRevision, task.DefaultRevision = head, defaultHead
				if err := ValidateProposals(fixture.cfg, []model.Proposal{task.Proposal}, ground, nil); err != nil {
					t.Fatal(err)
				}
				saveExecutionTask(t, fixture.planningFixture, task)
				mark(task.ID)
			}
			hidden := executionTask(t, fixture.planningFixture, "octomus/existing")
			hidden.CycleID, hidden.Branch = "hidden-cycle", "octomus/existing"
			number := uint64(42)
			hidden.PRNumber, hidden.PRURL = &number, stringPointer("https://github.com/fixture/project/pull/42")
			saveExecutionTask(t, fixture.planningFixture, hidden)
			if next != "invalid cycle" {
				if err := ValidateProposals(fixture.cfg, []model.Proposal{hidden.Proposal}, ground, nil); err != nil {
					t.Fatal(err)
				}
				if next == "cancelled task" {
					mark(hidden.ID)
				}
			} else {
				sibling := queuedTask(fixture.cfg, "hidden-sibling", "octomus/existing", "octomus/existing")
				sibling.CycleID = hidden.CycleID
				saveExecutionTask(t, fixture.planningFixture, sibling)
			}
			schedulerSQL(t, fixture.state, "DROP TRIGGER fail_cancel")
			fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Answer: "Implemented", Effect: writeFile("feature.txt", "fixed\n")})
			fixture.script.Answer(fixture.routes.Reviewer, cleanReview("Complete"))
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			control.NextCycleAt = int64(^uint64(0) >> 1)
			if err := fixture.state.SaveControl(control); err != nil {
				t.Fatal(err)
			}
			visible, err := fixture.state.SchedulingTasks(nil)
			if err != nil || len(visible) != 500 {
				t.Fatalf("initial queue window = %d, %v", len(visible), err)
			}
			for len(app.wake) > 0 {
				<-app.wake
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			if saved := loadTask(t, fixture.state, hidden.ID); saved.Status != model.StatusQueued {
				t.Fatalf("new window ran before preparation: status=%s sessions=%d publications=%v", saved.Status, len(saved.Sessions), publications(t, fixture.planningFixture))
			}
			select {
			case <-app.wake:
			default:
				t.Fatal("changed preparation did not request a fresh scheduler pass")
			}
			assertAdmissions(t, fixture.state, 0, "new rows wait for their own preparation pass")
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			saved := loadTask(t, fixture.state, hidden.ID)
			if next == "valid task" {
				if saved.Status != model.StatusPublished {
					t.Fatalf("newly prepared valid task did not proceed: %s", saved.Status)
				}
				assertAdmissions(t, fixture.state, 2, "valid work runs after its own preparation pass")
				if entries := publications(t, fixture.planningFixture); len(entries) != 1 || entries[0]["action"] != "comment" {
					t.Fatalf("newly prepared valid task publication = %+v", entries)
				}
			} else {
				if next == "cancelled task" {
					if saved.Status != model.StatusCancelled || cancellationEvents(t, fixture.state, hidden.ID) != 1 {
						t.Fatalf("newly visible cancellation was not settled: %s", saved.Status)
					}
				} else if !blockedAs(saved, model.BlockedReasonInvalidPlan) {
					t.Fatalf("newly visible invalid plan was not blocked: %+v", saved)
				}
				assertAdmissions(t, fixture.state, 0, "newly visible cancellations and invalid plans admit no work")
				if entries := publications(t, fixture.planningFixture); len(entries) != 0 {
					t.Fatalf("newly visible forbidden task published: %+v", entries)
				}
			}
			assertNoOpenClients(t, fixture.script)
		})
	}
}

func TestPreparationChangesYieldBeforeIdlePlanning(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t, withGitHubIdentity())
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.pausedApp(t)
	refuseQueuedCancellation(t, app, fixture.state, task.ID)
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	if err := fixture.state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	for len(app.wake) > 0 {
		<-app.wake
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if saved := loadTask(t, fixture.state, task.ID); saved.Status != model.StatusCancelled {
		t.Fatalf("preparation did not settle cancellation: %s", saved.Status)
	}
	if cycles, err := store.List[model.Cycle](fixture.state, "cycle"); err != nil || len(cycles) != 0 {
		t.Fatalf("preparation started idle planning before yielding: cycles=%d, %v", len(cycles), err)
	}
	select {
	case <-app.wake:
	default:
		t.Fatal("changed preparation did not notify the scheduler")
	}
	assertAdmissions(t, fixture.state, 0, "preparation yields before any later planning admission")
	assertNoOpenClients(t, fixture.script)
}
