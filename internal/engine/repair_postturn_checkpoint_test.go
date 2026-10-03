package engine

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

func TestRepairSummaryCheckpointFailureSurvivesShutdownReopen(t *testing.T) {
	for _, test := range []struct {
		name               string
		shutdown, findings bool
	}{
		{"shutdown failing verification", true, false},
		{"shutdown fresh findings", true, true},
		{"storage failure explicit retry", false, false},
	} {
		t.Run(test.name, func(t *testing.T) { testRepairSummaryCheckpointFailure(t, test.shutdown, test.findings) })
	}
}

func testRepairSummaryCheckpointFailure(t *testing.T, shutdown, findings bool) {
	t.Helper()
	var app *App
	var refused atomic.Bool
	var completedCheckpoint atomic.Bool
	var taskID string
	hook := "repair_summary_write_" + strings.ReplaceAll(model.ID(), "-", "")
	if err := sqlite.RegisterScalarFunction(hook, 5, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		if args[0] == taskID && args[1] == "repairing" && args[2] == "Successful repair summary" && refused.CompareAndSwap(false, true) {
			completedCheckpoint.Store(args[3] == int64(1) && args[4] == "completed")
			if shutdown {
				app.cancel()
			}
			return int64(1), nil
		}
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxRepairRounds = 1
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("Before repair"))
	finalReview := cleanReview("Fresh recovery review")
	if findings {
		finalReview = `{"completed":true,"summary":"Still unresolved","findings":[{"title":"Unresolved behavior","file":"feature.txt:1","detail":"The repair misses the requirement.","priority":"P1"}]}`
	}
	script.Answer(routes.Reviewer, finalReview)
	secondRepair := runnertest.Reply{Err: errors.New("unexpected extra repair")}
	if !shutdown {
		secondRepair = runnertest.Reply{Answer: "Retried repair", Effect: writeFile("feature.txt", "retried\n")}
		script.Answer(routes.Reviewer, cleanReview("After explicit retry repair"))
	}
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "Successful repair summary", Effect: writeFile("feature.txt", "repaired\n")},
		secondRepair)
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	task.Status = model.StatusExecuting
	taskID = task.ID
	saveExecutionTask(t, fixture.planningFixture, task)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER refuse_repair_summary BEFORE UPDATE ON records
  WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='repairing'
  AND EXISTS (SELECT 1 FROM json_each(NEW.data,'$.sessions') WHERE json_extract(value,'$.role')='repair'
   AND json_extract(value,'$.summary')='Successful repair summary')
  AND %s(NEW.id,json_extract(NEW.data,'$.status'),json_extract(NEW.data,'$.sessions[#-1].summary'),json_extract(NEW.data,'$.repair_rounds'),json_extract(NEW.data,'$.sessions[#-1].status'))=1 BEGIN SELECT RAISE(ABORT, 'synthetic repair summary write refusal'); END`, task.ID, hook))
	app = fixture.newApp(t)
	app.runTask(task)
	app.wg.Wait()
	app.Shutdown()
	stopped := loadTask(t, fixture.state, task.ID)
	wantStatus := model.StatusRepairing
	if !shutdown {
		wantStatus = model.StatusBlocked
	}
	if !refused.Load() || stopped.Status != wantStatus || len(script.Turns(routes.Repair)) != 1 {
		t.Fatalf("did not interrupt successful repair checkpoint: refused=%t status=%s repairs=%d", refused.Load(), stopped.Status, len(script.Turns(routes.Repair)))
	}
	if contents, err := os.ReadFile(filepath.Join(stopped.Workspace, "feature.txt")); err != nil || string(contents) != "repaired\n" {
		t.Fatalf("repair workspace effect missing: %q %v", contents, err)
	}
	if !completedCheckpoint.Load() {
		t.Fatal("first post-turn summary checkpoint omitted completed repair accounting")
	}
	sessions := sessionByRole(stopped, "repair")
	if stopped.RepairRounds == nil || *stopped.RepairRounds != 1 || stopped.RepairProgress == nil || !stopped.RepairProgress.AwaitingReview || len(sessions) != 1 || sessions[0].Status != model.SessionCompleted || sessions[0].Summary != "Successful repair summary" {
		t.Fatalf("supervisor lost successful turn accounting: counter=%v progress=%+v sessions=%+v", stopped.RepairRounds, stopped.RepairProgress, sessions)
	}
	if !shutdown && (stopped.Error == nil || !strings.Contains(*stopped.Error, "synthetic repair summary write refusal")) {
		t.Fatalf("storage failure lost diagnostic: %s", optionalText(stopped.Error))
	}
	t.Logf("durable checkpoint: repairs=%d progress=%+v session=%s", *stopped.RepairRounds, stopped.RepairProgress, sessions[0].Status)
	// Physically close and reopen SQLite; the temporary refusal disappears with its connection.
	if err := fixture.state.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.state = openStore(t, fixture.root)
	live := fixture.cfg.Clone()
	live.MaxRepairRounds = 5
	if !shutdown {
		live.MaxRepairRounds = 1
	}
	live.RepairRoute = config.NewRoute("unconfigured-live-repair", "high")
	live.Roles["code_reviewer"] = config.NewRoute("unconfigured-live-reviewer", "high")
	live.VerificationCommands = []string{"true"}
	if err := fixture.state.Put("settings", "config", live); err != nil {
		t.Fatal(err)
	}
	app = fixture.newApp(t)
	if err := app.Recover(); err != nil {
		t.Fatal(err)
	}
	wantRepairs, wantReviews, wantChecks := 1, 2, 2
	if findings {
		wantChecks = 1
	}
	if !shutdown {
		if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
			t.Fatal(err)
		}
		queued := loadTask(t, fixture.state, task.ID)
		if queued.RepairRounds == nil || *queued.RepairRounds != 0 || queued.RepairProgress != nil || queued.ReviewBaseline != uint64(len(stopped.Reviews)) || queued.RepairSession == nil || *queued.RepairSession != *stopped.RepairSession {
			t.Fatal("explicit retry lost reset budget, progress or retained repair thread/history")
		}
		wantRepairs, wantReviews, wantChecks = 2, 3, 3
	}
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if !blockedAs(saved, model.BlockedReasonVerificationFailed) || saved.Error == nil || !strings.Contains(*saved.Error, "Repair budget exhausted") || len(script.Turns(routes.Repair)) != wantRepairs {
		t.Fatalf("successful repair was not charged through summary failure/reopen: status=%s error=%s repairs=%d counter=%v", saved.Status, optionalText(saved.Error), len(script.Turns(routes.Repair)), saved.RepairRounds)
	}
	if saved.RepairRounds == nil || *saved.RepairRounds != 1 || saved.RepairProgress == nil || !saved.RepairProgress.AwaitingReview || len(saved.Reviews) != wantReviews || len(saved.Verification) != wantChecks || saved.Verification[wantChecks-1].Success {
		t.Fatalf("recovery lost completed repair/pinned failed verification: counter=%v progress=%+v reviews=%d checks=%+v", saved.RepairRounds, saved.RepairProgress, len(saved.Reviews), saved.Verification)
	}
	if !wirejson.Equal(saved.Config, task.Config) || !wirejson.Equal(saved.Reviews[:1], stopped.Reviews) || saved.ComparisonBase != stopped.ComparisonBase || saved.SourceRevision != stopped.SourceRevision {
		t.Fatal("recovery changed pinned config or retained review/source/base")
	}
	if !shutdown {
		starts := script.Starts(routes.Repair)
		if starts[1].Resume == nil || *starts[1].Resume != *stopped.RepairSession {
			t.Fatal("explicit retry did not resume pinned repair session")
		}
	}
	if len(script.Turns(routes.Executor)) != 1 {
		t.Fatal("recovery/retry repeated executor")
	}
	assertRecoveryReviewThreads(t, fixture, saved)
	assertUnpublished(t, fixture, saved)
	assertNoOpenClients(t, script)
}
