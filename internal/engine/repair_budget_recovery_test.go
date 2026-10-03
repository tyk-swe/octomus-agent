package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestRecoveryVerifiesAfterFinalAllowedRepair(t *testing.T) {
	for _, test := range []struct {
		name, repaired, finalReview string
		legacy                      bool
		published                   bool
		verifications               int
	}{
		{"passing verification", "fixed\n", cleanReview("Recovery reviewed"), false, true, 2},
		{"failing verification", "still broken\n", cleanReview("Recovery reviewed"), false, false, 2},
		{"fresh findings", "fixed\n", `{"completed":true,"summary":"Fresh finding","findings":[{"title":"Unresolved behavior","file":"feature.txt:1","detail":"The repair still misses the requirement.","priority":"P1"}]}`, false, false, 1},
		{"legacy passing verification", "fixed\n", cleanReview("Recovery reviewed"), true, true, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScriptedFixture(t, withGitHubIdentity())
			fixture.configure(t, func(cfg *config.Config) {
				cfg.VerificationCommands = []string{`test "$(cat feature.txt)" = fixed`}
				cfg.MaxRepairRounds = 1
			})
			routes, script := fixture.routes, fixture.script
			script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature", Effect: writeFile("feature.txt", "draft\n")})
			script.Answer(routes.Reviewer, cleanReview("Draft reviewed"), cleanReview("Repair reviewed"), test.finalReview)
			script.Queue(routes.Repair, runnertest.Reply{Answer: "Repaired feature", Effect: writeFile("feature.txt", test.repaired)})
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture.planningFixture, task)
			box := &interruptedVerification{stopAt: 2, entered: make(chan struct{})}
			app := fixture.newApp(t, WithSandbox(box))
			tickUntil(t, app, box.entered, "verification after final repair")
			app.Shutdown()
			stopped := loadTask(t, fixture.state, task.ID)
			if stopped.Status != model.StatusVerifying || len(stopped.Reviews) != 2 || len(stopped.Verification) != 1 {
				t.Fatalf("unexpected stopped evidence: %+v", stopped)
			}
			if stopped.RepairRounds == nil || *stopped.RepairRounds != 1 {
				t.Fatalf("completed repair counter = %v; want one", stopped.RepairRounds)
			}
			if test.legacy {
				stopped.RepairRounds = nil
				saveExecutionTask(t, fixture.planningFixture, stopped)
			}
			restarted := fixture.newApp(t)
			if err := restarted.Recover(); err != nil {
				t.Fatal(err)
			}
			saved := driveTask(t, fixture.planningFixture, restarted, task.ID)
			if test.published && saved.Status != model.StatusPublished {
				t.Fatalf("final repair verification never resumed: status=%s reason=%v error=%s reviews=%d verifications=%d repairs=%d", saved.Status, saved.BlockedReason, optionalText(saved.Error), len(saved.Reviews), len(saved.Verification), len(script.Turns(routes.Repair)))
			}
			if !test.published {
				if !blockedAs(saved, model.BlockedReasonVerificationFailed) || saved.Error == nil || !strings.Contains(*saved.Error, "Repair budget exhausted") {
					t.Fatalf("exhausted repair allowed unresolved work: status=%s error=%s", saved.Status, optionalText(saved.Error))
				}
				assertUnpublished(t, fixture, saved)
			}
			if len(saved.Reviews) != 3 || len(saved.Verification) != test.verifications || len(script.Turns(routes.Repair)) != 1 {
				t.Fatalf("recovery review/verification/repair counts = %d/%d/%d", len(saved.Reviews), len(saved.Verification), len(script.Turns(routes.Repair)))
			}
			if !wirejson.Equal(saved.Reviews[:2], stopped.Reviews) || saved.ReviewBaseline != stopped.ReviewBaseline || saved.SourceRevision != stopped.SourceRevision || saved.ComparisonBase != stopped.ComparisonBase {
				t.Fatal("recovery changed retained review evidence or source/base attribution")
			}
			assertRecoveryReviewThreads(t, fixture, saved)
			assertNoOpenClients(t, script)
		})
	}
}

func assertRecoveryReviewThreads(t *testing.T, fixture *scriptedFixture, task model.Task) {
	t.Helper()
	seen := map[string]bool{}
	for i, start := range fixture.script.Starts(fixture.routes.Reviewer) {
		if start.Resume != nil || seen[start.Session] {
			t.Fatalf("reviewer %d reused a thread: %+v", i, start)
		}
		seen[start.Session] = true
		if i < len(task.Reviews) {
			turn := fixture.script.Turns(fixture.routes.Reviewer)[i]
			if !strings.Contains(turn.Prompt, "git diff "+task.ComparisonBase+" HEAD. Recorded HEAD: "+task.Reviews[i].Revision+".") {
				t.Fatalf("reviewer %d lost full comparison base/revision: %s", i, turn.Prompt)
			}
		}
	}
}

func TestRecoveryReviewsDoNotSpendRepairRounds(t *testing.T) {
	fixture := newScriptedFixture(t, withGitHubIdentity())
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{`test "$(cat feature.txt)" = fixed`}
		cfg.MaxRepairRounds = 1
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature", Effect: writeFile("feature.txt", "draft\n")})
	for range 4 {
		script.Answer(routes.Reviewer, cleanReview("Full change reviewed"))
	}
	script.Queue(routes.Repair, runnertest.Reply{Answer: "Fixed feature", Effect: writeFile("feature.txt", "fixed\n")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	for interruption := range 2 {
		box := &interruptedVerification{stopAt: 1, entered: make(chan struct{})}
		app := fixture.newApp(t, WithSandbox(box))
		if interruption > 0 {
			if err := app.Recover(); err != nil {
				t.Fatal(err)
			}
		}
		tickUntil(t, app, box.entered, "verification before any repair")
		app.Shutdown()
		stopped := loadTask(t, fixture.state, task.ID)
		if stopped.RepairRounds == nil || *stopped.RepairRounds != 0 || len(script.Turns(routes.Repair)) != 0 || len(stopped.Reviews) != interruption+1 {
			t.Fatalf("review recovery spent a repair round: %+v", stopped)
		}
	}
	app := fixture.pausedApp(t)
	if err := app.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := app.Recover(); err != nil {
		t.Fatal(err)
	}
	current, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	revision, err := current.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.SaveConfig(revision, map[string]json.RawMessage{
		"max_repair_rounds":     json.RawMessage("2"),
		"verification_commands": json.RawMessage(`["false"]`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusPublished || saved.RepairRounds == nil || *saved.RepairRounds != 1 || len(saved.Reviews) != 4 || len(saved.Verification) != 2 || saved.ReviewBaseline != 0 || !wirejson.Equal(saved.Config, task.Config) {
		t.Fatalf("recovery did not preserve repair allowance and pinned verification: status=%s error=%s counter=%v reviews=%d checks=%d", saved.Status, optionalText(saved.Error), saved.RepairRounds, len(saved.Reviews), len(saved.Verification))
	}
	if len(script.Turns(routes.Executor)) != 1 || len(script.Turns(routes.Repair)) != 1 {
		t.Fatal("recovery repeated executor or spent an extra repair")
	}
	assertRecoveryReviewThreads(t, fixture, saved)
	assertNoOpenClients(t, script)
}

func TestCompletedRepairCounterSurvivesFinalWriteFailure(t *testing.T) {
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxRepairRounds = 1
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("Before repair"), cleanReview("Explicit retry"), cleanReview("After retried repair"))
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "First repair", Effect: writeFile("feature.txt", "first repair\n")},
		runnertest.Reply{Answer: "Retried repair", Effect: writeFile("feature.txt", "second repair\n")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER refuse_completed_repair BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='repairing'
		AND json_extract(NEW.data,'$.repair_rounds')=1
		BEGIN SELECT RAISE(ABORT, 'synthetic completed repair write refusal'); END`, task.ID))
	app := fixture.newApp(t)
	blocked := driveTask(t, fixture.planningFixture, app, task.ID)
	if blocked.Status != model.StatusBlocked || blocked.RepairRounds == nil || *blocked.RepairRounds != 1 || blocked.RepairProgress == nil || !blocked.RepairProgress.AwaitingReview || len(script.Turns(routes.Repair)) != 1 {
		t.Fatalf("supervisor lost completed repair accounting: status=%s counter=%v progress=%+v", blocked.Status, blocked.RepairRounds, blocked.RepairProgress)
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_completed_repair")
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	queued := loadTask(t, fixture.state, task.ID)
	if queued.RepairRounds == nil || *queued.RepairRounds != 0 || queued.ReviewBaseline != uint64(len(blocked.Reviews)) || queued.RepairSession == nil || *queued.RepairSession != *blocked.RepairSession {
		t.Fatal("explicit retry failed to reset counter while retaining repair thread/history")
	}
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if !blockedAs(saved, model.BlockedReasonVerificationFailed) || saved.RepairRounds == nil || *saved.RepairRounds != 1 || len(script.Turns(routes.Repair)) != 2 {
		t.Fatalf("explicit retry repair budget was inaccurate: status=%s counter=%v", saved.Status, saved.RepairRounds)
	}
	starts := script.Starts(routes.Repair)
	if starts[1].Resume == nil || *starts[1].Resume != *blocked.RepairSession {
		t.Fatal("explicit retry lost persistent repair context")
	}
	assertUnpublished(t, fixture, saved)
	assertNoOpenClients(t, script)
}
