package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestRecoveryPreservesNoProgressLimit(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxRepairRounds = 4
		cfg.MaxNoProgressRounds = 1
	})
	routes, script := fixture.routes, fixture.script
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Queue(routes.Reviewer,
		runnertest.Reply{Answer: cleanReview("First review")},
		runnertest.Reply{Answer: cleanReview("Interrupted review"), Gate: gate})
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "No changes needed"},
		runnertest.Reply{Err: errors.New("A second repair exceeds the no-progress limit")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	policy := model.AttemptPolicyFromConfig(fixture.cfg)
	task.AttemptPolicy = &policy
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)
	tickUntil(t, app, gate.Entered(), "review after a repair with no changes")
	app.Shutdown()
	stopped := loadTask(t, fixture.state, task.ID)
	if stopped.Status != model.StatusReviewing || len(stopped.Reviews) != 1 || len(script.Turns(routes.Repair)) != 1 {
		t.Fatalf("interruption did not preserve the no-progress repair: %+v", stopped)
	}
	live := fixture.cfg.Clone()
	live.MaxRetries, live.MaxRepairRounds, live.MaxNoProgressRounds = 0, 1, 3
	live.TaskTimeoutSeconds, live.SessionTimeoutSeconds, live.CommandTimeoutSeconds = 100, 20, 10
	live.VerificationCommands = []string{"true"}
	if err := fixture.state.Put("settings", "config", live); err != nil {
		t.Fatal(err)
	}

	script.Answer(routes.Reviewer, cleanReview("Fresh review after restart"))
	restarted := fixture.newApp(t)
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered := loadTask(t, fixture.state, task.ID)
	if recovered.Status != model.StatusQueued || !wirejson.Equal(recovered.AttemptPolicy, task.AttemptPolicy) || !wirejson.Equal(recovered.Config, task.Config) {
		t.Fatal("automatic recovery adopted current settings instead of the saved attempt")
	}
	saved := driveTask(t, fixture.planningFixture, restarted, task.ID)
	if !blockedAs(saved, model.BlockedReasonVerificationFailed) || saved.Error == nil || !strings.Contains(*saved.Error, "Repairs made no progress") {
		t.Fatalf("recovered no-progress outcome: status=%s reason=%v error=%s", saved.Status, saved.BlockedReason, optionalText(saved.Error))
	}
	if turns := script.Turns(routes.Repair); len(turns) != 1 || script.Pending(routes.Repair) != 1 {
		t.Fatalf("automatic recovery reset the no-progress budget: repair turns=%d pending=%d", len(turns), script.Pending(routes.Repair))
	}
	if saved.Attempts != 1 || saved.ReviewBaseline != 0 || len(saved.Reviews) != 2 || saved.Reviews[0].Revision != saved.Reviews[1].Revision {
		t.Fatalf("recovery lost the unchanged review history: %+v", saved)
	}
	starts := script.Starts(routes.Reviewer)
	if len(starts) != 3 || starts[2].Resume != nil || starts[2].Session == starts[1].Session {
		t.Fatalf("recovery must create a fresh reviewer: %+v", starts)
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 5, "executor + three reviewers + one repair")
	assertNoOpenClients(t, script)
}

func TestRecoveryDoesNotChargeInterruptedRepairsTwice(t *testing.T) {
	t.Parallel()
	for _, completed := range []int{0, 1} {
		t.Run(fmt.Sprintf("%d completed repairs", completed), func(t *testing.T) {
			fixture := newScriptedFixture(t)
			fixture.configure(t, func(cfg *config.Config) {
				cfg.VerificationCommands = []string{"false"}
				cfg.MaxNoProgressRounds = uint64(completed + 1)
			})
			routes, script := fixture.routes, fixture.script
			gate := runnertest.NewGate()
			t.Cleanup(gate.Release)
			script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
			for range completed {
				script.Answer(routes.Repair, "Completed without changes")
			}
			script.Queue(routes.Repair, runnertest.Reply{Answer: "Interrupted", Gate: gate})
			for range completed + 1 {
				script.Answer(routes.Reviewer, cleanReview("Before interruption"))
			}
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture.planningFixture, task)
			app := fixture.newApp(t)
			tickUntil(t, app, gate.Entered(), "interrupted repair")
			app.Shutdown()
			stopped := loadTask(t, fixture.state, task.ID)
			if stopped.Status != model.StatusRepairing || stopped.RepairSession == nil {
				t.Fatalf("repair did not remain interrupted: %+v", stopped)
			}
			if completed == 0 && stopped.RepairProgress != nil || completed > 0 &&
				(stopped.RepairProgress == nil || stopped.RepairProgress.AwaitingReview || stopped.RepairProgress.NoProgressRounds != uint64(completed)) {
				t.Fatalf("interrupted repair changed completed progress: %+v", stopped.RepairProgress)
			}
			script.Answer(routes.Reviewer, cleanReview("Review after restart"), cleanReview("Completed resumed repair"))
			script.Answer(routes.Repair, "Resumed without changes")
			restarted := fixture.newApp(t)
			if err := restarted.Recover(); err != nil {
				t.Fatal(err)
			}
			saved := driveTask(t, fixture.planningFixture, restarted, task.ID)
			if !blockedAs(saved, model.BlockedReasonVerificationFailed) || saved.Error == nil || !strings.Contains(*saved.Error, "Repairs made no progress") {
				t.Fatalf("recovered repair: status=%s reason=%v error=%s", saved.Status, saved.BlockedReason, optionalText(saved.Error))
			}
			if turns := script.Turns(routes.Repair); len(turns) != completed+2 || saved.RepairProgress.NoProgressRounds != uint64(completed+1) {
				t.Fatalf("interrupted repair was charged twice: turns=%d progress=%+v", len(turns), saved.RepairProgress)
			}
			starts := script.Starts(routes.Repair)
			if starts[len(starts)-1].Resume == nil || *starts[len(starts)-1].Resume != *stopped.RepairSession {
				t.Fatalf("recovery did not resume the repair session: %+v", starts)
			}
			assertUnpublished(t, fixture, saved)
			assertNoOpenClients(t, script)
		})
	}
}

type interruptedVerification struct {
	sandbox.Host
	commands int
	stopAt   int
	entered  chan struct{}
}

func (b *interruptedVerification) Start(ctx context.Context, spec sandbox.Spec) (sandbox.Child, error) {
	if spec.Kind == sandbox.KindVerify {
		b.commands++
		if b.commands == b.stopAt {
			close(b.entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	return b.Host.Start(ctx, spec)
}

func TestRecoveryChecksVerificationBeforeNoProgress(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		stopAt     int
		succeed    bool
		wantReview int
	}{
		{"before first repair", 1, false, 3},
		{"after completed repair", 2, false, 3},
		{"passing verification after completed repair", 2, true, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScriptedFixture(t, withGitHubIdentity())
			pass := filepath.Join(fixture.root, "verification-pass")
			fixture.configure(t, func(cfg *config.Config) {
				cfg.VerificationCommands = []string{fmt.Sprintf("test -f %q", pass)}
				cfg.MaxNoProgressRounds = 1
			})
			routes, script := fixture.routes, fixture.script
			script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
			for range test.wantReview {
				script.Answer(routes.Reviewer, cleanReview("Verification is required"))
			}
			script.Answer(routes.Repair, "Completed without changes")
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture.planningFixture, task)
			box := &interruptedVerification{stopAt: test.stopAt, entered: make(chan struct{})}
			app := fixture.newApp(t, WithSandbox(box))
			tickUntil(t, app, box.entered, "interrupted verification")
			app.Shutdown()
			stopped := loadTask(t, fixture.state, task.ID)
			if stopped.Status != model.StatusVerifying || len(stopped.Verification) != test.stopAt-1 {
				t.Fatalf("interrupted verification became evidence: %+v", stopped)
			}
			if test.succeed {
				if err := os.WriteFile(pass, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			restarted := fixture.newApp(t)
			if err := restarted.Recover(); err != nil {
				t.Fatal(err)
			}
			saved := driveTask(t, fixture.planningFixture, restarted, task.ID)
			if test.succeed {
				if saved.Status != model.StatusPublished || !saved.Verification[len(saved.Verification)-1].Success {
					t.Fatalf("successful verification was blocked by identical revisions: status=%s error=%s", saved.Status, optionalText(saved.Error))
				}
			} else {
				if !blockedAs(saved, model.BlockedReasonVerificationFailed) || saved.Error == nil || !strings.Contains(*saved.Error, "Repairs made no progress") {
					t.Fatalf("failed verification did not preserve the bound: status=%s error=%s", saved.Status, optionalText(saved.Error))
				}
				assertUnpublished(t, fixture, saved)
			}
			if len(script.Turns(routes.Repair)) != 1 || len(saved.Reviews) != test.wantReview {
				t.Fatalf("recovery charged a verification interruption as repair: repairs=%d reviews=%d", len(script.Turns(routes.Repair)), len(saved.Reviews))
			}
			assertNoOpenClients(t, script)
		})
	}
}

func TestExplicitRetryResetsProgressAndAdoptsOnlyAttemptPolicy(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxNoProgressRounds = 1
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	for range 4 {
		script.Answer(routes.Reviewer, cleanReview("Verification is required"))
	}
	script.Answer(routes.Repair, "First no-op repair", "Second no-op repair")
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	policy := model.AttemptPolicyFromConfig(fixture.cfg)
	task.AttemptPolicy = &policy
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)
	first := driveTask(t, fixture.planningFixture, app, task.ID)
	if first.RepairProgress == nil || first.RepairProgress.NoProgressRounds != 1 || first.AttemptReviews() != 2 {
		t.Fatalf("first attempt did not exhaust no-progress budget: %+v", first)
	}
	first.Attempts = first.Config.MaxRetries
	saveExecutionTask(t, fixture.planningFixture, first)
	live := fixture.cfg.Clone()
	live.MaxRepairRounds, live.MaxNoProgressRounds, live.MaxRetries = 1, 2, 3
	live.TaskTimeoutSeconds, live.SessionTimeoutSeconds, live.CommandTimeoutSeconds = 120, 20, 10
	live.VerificationCommands = []string{"true"}
	live.RepairRoute = config.NewRoute("different-repair", "low")
	live.Roles["code_reviewer"] = config.NewRoute("different-reviewer", "low")
	for _, tier := range config.Tiers() {
		live.Tiers[tier] = config.NewRoute("different-executor", "low")
	}
	if err := fixture.state.Put("settings", "config", live); err != nil {
		t.Fatal(err)
	}
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	queued := loadTask(t, fixture.state, task.ID)
	wantPolicy := model.AttemptPolicyFromConfig(live)
	if queued.RepairProgress != nil || queued.ReviewBaseline != 2 || queued.Attempts != 3 || queued.AttemptPolicy == nil || *queued.AttemptPolicy != wantPolicy {
		t.Fatalf("explicit retry did not adopt fresh attempt limits: %+v", queued)
	}
	if !wirejson.Equal(queued.Config, task.Config) || !wirejson.Equal(queued.Route, task.Route) || !wirejson.Equal(queued.Reviews, first.Reviews) {
		t.Fatal("explicit retry changed immutable configuration, route or retained evidence")
	}
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if !blockedAs(saved, model.BlockedReasonVerificationFailed) || saved.Error == nil || !strings.Contains(*saved.Error, "Repair budget exhausted (max_repair_rounds 1)") {
		t.Fatalf("retry did not use fresh repair budget with saved verification: status=%s error=%s", saved.Status, optionalText(saved.Error))
	}
	if len(script.Turns(routes.Executor)) != 1 || len(script.Turns(routes.Reviewer)) != 4 || len(script.Turns(routes.Repair)) != 2 {
		t.Fatal("retry changed saved routes or repeated the completed executor")
	}
	starts := script.Starts(routes.Repair)
	if starts[1].Resume == nil || *starts[1].Resume != *first.RepairSession {
		t.Fatalf("explicit retry did not retain the repair thread: %+v", starts)
	}
	if err := app.TaskAction(context.Background(), task.ID, "retry"); !IsActionConflict(err) {
		t.Fatalf("retry beyond the current ceiling = %v; want a conflict", err)
	}
	if after := loadTask(t, fixture.state, task.ID); !wirejson.Equal(after, saved) {
		t.Fatal("exhausted retry changed the task")
	}
	assertUnpublished(t, fixture, saved)
	assertNoOpenClients(t, script)
}

func TestAttemptRetryStorageFailuresKeepDurableProgress(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"policy event", "cancel marker", "task record"} {
		t.Run(target, func(t *testing.T) {
			fixture := newScriptedFixture(t)
			fixture.configure(t, func(cfg *config.Config) {
				cfg.VerificationCommands = []string{"false"}
				cfg.MaxNoProgressRounds = 1
			})
			fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
			fixture.script.Answer(fixture.routes.Reviewer, cleanReview("First review"), cleanReview("After repair"))
			fixture.script.Answer(fixture.routes.Repair, "Completed without changes")
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture.planningFixture, task)
			app := fixture.newApp(t)
			task = driveTask(t, fixture.planningFixture, app, task.ID)
			if !blockedAs(task, model.BlockedReasonVerificationFailed) || task.RepairProgress == nil || task.RepairProgress.NoProgressRounds != 1 {
				t.Fatalf("fixture did not stop at the no-progress bound: %+v", task)
			}
			var trigger string
			switch target {
			case "policy event":
				trigger = fmt.Sprintf("BEFORE INSERT ON events WHEN NEW.entity_id='%s' AND NEW.kind='attempt_policy'", task.ID)
			case "cancel marker":
				trigger = fmt.Sprintf("BEFORE INSERT ON records WHEN NEW.kind='cancel' AND NEW.id='%s'", task.ID)
			case "task record":
				trigger = fmt.Sprintf("BEFORE UPDATE ON records WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='queued'", task.ID)
			}
			schedulerSQL(t, fixture.state, "CREATE TEMP TRIGGER fail_attempt "+trigger+" BEGIN SELECT RAISE(ABORT, 'synthetic attempt storage failure'); END")
			if err := app.TaskAction(context.Background(), task.ID, "retry"); err == nil || !strings.Contains(err.Error(), "synthetic attempt storage failure") {
				t.Fatalf("retry did not reach storage failure: %v", err)
			}
			if after := loadTask(t, fixture.state, task.ID); !wirejson.Equal(after, task) {
				t.Fatal("failed retry changed durable progress or spent an attempt")
			}
			assertAdmissions(t, fixture.state, 4, "failed retry adds no work after executor, two reviews and repair")
			schedulerSQL(t, fixture.state, "DROP TRIGGER fail_attempt")
			if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
				t.Fatal(err)
			}
			if after := loadTask(t, fixture.state, task.ID); after.Status != model.StatusQueued || after.Attempts != 1 || after.RepairProgress != nil {
				t.Fatalf("retry after storage recovery = %+v", after)
			}
		})
	}
}

func TestRepairProgressWriteFailureStartsNoFurtherRepair(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"false"}
		cfg.MaxNoProgressRounds = 2
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("First review"), cleanReview("After repair"))
	script.Answer(routes.Repair, "Completed without changes")
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER fail_progress BEFORE UPDATE ON records
		WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status')='verifying'
		AND json_extract(OLD.data,'$.repair_progress.awaiting_review')=1
		AND json_extract(NEW.data,'$.repair_progress.awaiting_review')=0
		BEGIN SELECT RAISE(ABORT, 'synthetic progress storage failure'); END`, task.ID))
	app := fixture.newApp(t)
	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil || !strings.Contains(*saved.Error, "synthetic progress storage failure") {
		t.Fatalf("progress storage failure did not block the task: status=%s error=%s", saved.Status, optionalText(saved.Error))
	}
	if len(script.Turns(routes.Repair)) != 1 || saved.RepairProgress == nil || saved.RepairProgress.NoProgressRounds != 1 || saved.RepairProgress.AwaitingReview {
		t.Fatalf("progress failure started another repair or lost the supervisor's evidence: progress=%+v", saved.RepairProgress)
	}
	schedulerSQL(t, fixture.state, "DROP TRIGGER fail_progress")
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	if queued := loadTask(t, fixture.state, task.ID); queued.Attempts != 1 || queued.RepairProgress != nil {
		t.Fatalf("retry after progress storage failure did not reset the attempt: %+v", queued)
	}
	assertUnpublished(t, fixture, saved)
	assertAdmissions(t, fixture.state, 4, "a progress write failure never starts the next repair")
	assertNoOpenClients(t, script)
}
