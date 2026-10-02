package engine

import (
	"context"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	"modernc.org/sqlite"
)

func TestTickRecoversPublicationWorkerTerminalWriteRefusal(t *testing.T) {
	for _, origin := range []string{"task worker", "operator reconciliation"} {
		for _, delivery := range []string{"new PR", "follow-up comment"} {
			for _, exhausted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/exhausted=%t", origin, delivery, exhausted), func(t *testing.T) {
					var publishedWrites, blockedWrites atomic.Uint32
					hook := "publication_refusal_" + strings.ReplaceAll(model.ID(), "-", "")
					if err := sqlite.RegisterScalarFunction(hook, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
						switch args[0] {
						case "published":
							publishedWrites.Add(1)
						case "blocked":
							blockedWrites.Add(1)
						}
						return int64(1), nil
					}); err != nil {
						t.Fatal(err)
					}
					fixture := newScriptedFixture(t, withGitHubIdentity())
					fixture.configure(t, func(cfg *config.Config) {
						cfg.VerificationCommands = []string{"test -f feature.txt"}
					})
					target := fixture.cfg.DefaultBranch
					wantAction := "create"
					if delivery == "follow-up comment" {
						existingPrBranch(t, fixture.planningFixture)
						git(t, fixture.repo, "fetch", filepath.Join(fixture.root, "remote.git"), "octomus/existing")
						target, wantAction = "octomus/existing", "comment"
					}
					task := executionTask(t, fixture.planningFixture, target)
					var wantAdmissions uint64 = 2
					if origin == "operator reconciliation" {
						task = checkpointedTask(t, fixture.planningFixture, target)
						task.Status = model.StatusBlocked
						task.BlockedReason = blockedReasonPtr(model.BlockedReasonPublicationUncertain)
						wantAdmissions = 0
					} else {
						task.Status = model.StatusExecuting
						fixture.script.Queue(fixture.routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
						fixture.script.Answer(fixture.routes.Reviewer, cleanReview("Reviewed the fixture change"))
					}
					if delivery == "follow-up comment" {
						task.Branch = target
						number := uint64(42)
						task.PRNumber = &number
						task.PRURL = stringPointer("https://github.com/fixture/project/pull/42")
					}
					if exhausted {
						task.Attempts = task.ExecutionConfig().MaxRetries
					}
					task.RunID = stringPointer(model.ID())
					saveExecutionTask(t, fixture.planningFixture, task)
					if delivery == "new PR" {
						if err := fixture.state.SeedPrReservation(task); err != nil {
							t.Fatal(err)
						}
					}
					schedulerSQL(t, fixture.state, fmt.Sprintf(`CREATE TEMP TRIGGER refuse_publication_outcome BEFORE UPDATE ON records
						WHEN NEW.kind='task' AND NEW.id='%s' AND json_extract(NEW.data,'$.status') IN ('published','blocked')
						BEGIN SELECT %s(json_extract(NEW.data,'$.status')); SELECT RAISE(ABORT, 'synthetic publication outcome refusal'); END`, task.ID, hook))
					app := fixture.pausedApp(t)
					if origin == "operator reconciliation" {
						if err := app.TaskAction(context.Background(), task.ID, "reconcile"); err == nil || !strings.Contains(err.Error(), "synthetic publication outcome refusal") {
							t.Fatalf("reconciliation terminal error = %v", err)
						}
					} else {
						app.runTask(task)
						app.wg.Wait()
					}
					if publishedWrites.Load() != 1 || blockedWrites.Load() != 1 {
						t.Fatalf("refused writes: published=%d blocked=%d; want terminal and fallback writes", publishedWrites.Load(), blockedWrites.Load())
					}
					before := loadTask(t, fixture.state, task.ID)
					if before.Status != model.StatusPublishing || before.OutputCommit == nil || len(before.AllowedActions()) != 0 {
						t.Fatalf("saved state after refusals = %+v", before)
					}
					if !app.Drained() {
						t.Fatal("worker retained a runtime slot")
					}
					entries := publications(t, fixture.planningFixture)
					if len(entries) != 1 || entries[0]["action"] != wantAction {
						t.Fatalf("accepted publications = %+v; want one %s", entries, wantAction)
					}
					prData, err := os.ReadFile(filepath.Join(fixture.root, "prs.json"))
					if err != nil || !strings.Contains(string(prData), "<!-- octomus:task:"+task.ID+" -->") {
						t.Fatalf("accepted delivery marker missing: %s, %v", prData, err)
					}
					apiBefore, err := os.ReadFile(filepath.Join(fixture.root, "gh-api.jsonl"))
					if err != nil {
						t.Fatal(err)
					}
					if err := fixture.state.MarkCancel(task.ID); err != nil {
						t.Fatal(err)
					}
					if err := fixture.state.ConfigureNotifications(stringPointer("synthetic-destination"), "enabled", nil); err != nil {
						t.Fatal(err)
					}
					if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "synthetic publication outcome refusal") {
						t.Fatalf("recovery under continued storage refusal = %v", err)
					}
					if current := loadTask(t, fixture.state, task.ID); !wirejson.Equal(before, current) {
						t.Fatal("refused recovery changed saved evidence")
					}
					schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_publication_outcome")
					// Recovery works while paused and does not require runner or remote readiness.
					if err := app.Tick(); err != nil {
						t.Fatal(err)
					}
					recovered := loadTask(t, fixture.state, task.ID)
					if !blockedAs(recovered, model.BlockedReasonPublicationUncertain) || !wirejson.Equal(recovered.AllowedActions(), []string{"archive", "reconcile"}) || recovered.Error == nil || !strings.Contains(*recovered.Error, "no active worker") {
						t.Fatalf("workerless checkpoint recovery = %+v", recovered)
					}
					expected := before.Clone()
					expected.Status, expected.BlockedReason, expected.Error, expected.UpdatedAt = recovered.Status, recovered.BlockedReason, recovered.Error, recovered.UpdatedAt
					if !wirejson.Equal(expected, recovered) {
						t.Fatal("recovery changed checkpoint, PR identity, attempts, batch membership or retained evidence")
					}
					if reserved, err := fixture.state.HasPrReservation(task.ID); err != nil || reserved != (delivery == "new PR") {
						t.Fatalf("recovery changed PR reservation: %t, %v", reserved, err)
					}
					if marked, err := fixture.state.MarkerSet("cancel", task.ID); err != nil || !marked {
						t.Fatalf("recovery changed cancellation marker: %t, %v", marked, err)
					}
					if pending, unresolved, err := fixture.state.BatchCounts(*task.RunID); err != nil || pending != 0 || unresolved != 1 {
						t.Fatalf("recovered batch counts = %d pending, %d unresolved: %v", pending, unresolved, err)
					}
					// Further paused/continuous/run-once ticks cannot replay the checkpoint.
					for _, mode := range []model.OperatingMode{model.OperatingModePaused, model.OperatingModeContinuous, model.OperatingModeRunOnce} {
						control := model.DefaultControl()
						control.SetMode(mode)
						control.NextCycleAt = time.Now().Unix() + 3600
						if mode == model.OperatingModeRunOnce {
							control.Batch = &model.RunBatch{ID: *task.RunID, Phase: model.BatchPhaseExecuting}
						}
						if err := fixture.state.SaveControl(control); err != nil {
							t.Fatal(err)
						}
						if err := app.Tick(); err != nil {
							t.Fatal(err)
						}
						app.wg.Wait()
						if after := loadTask(t, fixture.state, task.ID); !wirejson.Equal(recovered, after) {
							t.Fatalf("%s tick changed recovered publication", mode)
						}
					}
					apiAfter, err := os.ReadFile(filepath.Join(fixture.root, "gh-api.jsonl"))
					if err != nil || string(apiAfter) != string(apiBefore) || !wirejson.Equal(publications(t, fixture.planningFixture), entries) {
						t.Fatalf("recovery called GitHub before operator reconciliation: %s, %v", apiAfter, err)
					}
					assertAdmissions(t, fixture.state, wantAdmissions, "recovery must not invoke a model")
					if health, err := fixture.state.NotificationHealth(); err != nil || health.Pending != 1 {
						t.Fatalf("recovery did not record exactly one attention notification: %+v, %v", health, err)
					}
					for _, action := range []string{"cancel", "retry", "supersede", "discard"} {
						if err := app.TaskAction(context.Background(), task.ID, action); !IsActionConflict(err) {
							t.Fatalf("recovered %s = %v; want recorded-state conflict", action, err)
						}
					}
					if err := app.TaskAction(context.Background(), task.ID, "reconcile"); err != nil {
						t.Fatal(err)
					}
					finished := loadTask(t, fixture.state, task.ID)
					if finished.Status != model.StatusPublished || finished.PRNumber == nil || optionalText(finished.OutputCommit) != optionalText(before.OutputCommit) || finished.Workspace != before.Workspace || !wirejson.Equal(finished.Sessions, before.Sessions) || !wirejson.Equal(finished.Reviews, before.Reviews) || !wirejson.Equal(finished.Verification, before.Verification) {
						t.Fatalf("recovery lost delivery or checkpoint evidence: %+v", finished)
					}
					afterPR, err := os.ReadFile(filepath.Join(fixture.root, "prs.json"))
					if err != nil || string(afterPR) != string(prData) || !wirejson.Equal(publications(t, fixture.planningFixture), entries) {
						t.Fatalf("recovery rewrote accepted delivery: %s, %v", afterPR, err)
					}
					assertAdmissions(t, fixture.state, wantAdmissions, "explicit publication reconciliation needs no model turn")
					assertNoOpenClients(t, fixture.script)
				})
			}
		}
	}
}

func TestTickRecoversPublicationWithoutInterruptingCurrentWorker(t *testing.T) {
	t.Parallel()
	for _, reconcile := range []bool{false, true} {
		t.Run(fmt.Sprintf("reconcile=%t", reconcile), func(t *testing.T) {
			fixture := newScriptedFixture(t, withGitHubIdentity())
			task := checkpointedTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			task.Status = model.StatusExecuting
			if reconcile {
				task.Status = model.StatusBlocked
				task.BlockedReason = blockedReasonPtr(model.BlockedReasonPublicationUncertain)
			}
			saveExecutionTask(t, fixture.planningFixture, task)
			heldUploadPack(t, fixture.planningFixture)
			app := fixture.pausedApp(t)
			t.Cleanup(func() { _ = os.Remove(filepath.Join(fixture.root, "hold")) })
			var completed chan error
			if reconcile {
				completed = make(chan error, 1)
				go func() { completed <- app.TaskAction(context.Background(), task.ID, "reconcile") }()
			} else {
				app.runTask(task)
			}
			waitForPreflights(t, fixture.planningFixture, 1)
			before := loadTask(t, fixture.state, task.ID)
			if before.Status != model.StatusPublishing {
				t.Fatalf("held task is not publishing: %+v", before)
			}
			orphan := before.Clone()
			orphan.ID = model.ID()
			orphan.Branch = fixture.cfg.BranchPrefix + orphan.ID
			saveExecutionTask(t, fixture.planningFixture, orphan)
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			if active := loadTask(t, fixture.state, task.ID); !wirejson.Equal(active, before) {
				t.Fatal("recovery rewrote a live publication's evidence")
			}
			if current := loadTask(t, fixture.state, orphan.ID); !blockedAs(current, model.BlockedReasonPublicationUncertain) {
				t.Fatalf("live publication prevented orphan recovery: %+v", current)
			}
			if err := app.TaskAction(context.Background(), task.ID, "reconcile"); !IsActionConflict(err) {
				t.Fatalf("live publication accepted another reconcile: %v", err)
			}
			if err := app.TaskAction(context.Background(), orphan.ID, "reconcile"); !IsActionConflict(err) {
				t.Fatalf("orphan reconciliation interrupted active publication: %v", err)
			}
			releasePreflight(t, fixture.planningFixture)
			app.wg.Wait()
			if completed != nil {
				if err := <-completed; err != nil {
					t.Fatal(err)
				}
			}
			finished := loadTask(t, fixture.state, task.ID)
			if finished.Status != model.StatusPublished || finished.PRNumber == nil {
				t.Fatalf("current publication did not finish: %+v", finished)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			if after := loadTask(t, fixture.state, task.ID); !wirejson.Equal(after, finished) {
				t.Fatal("later recovery demoted accepted publication")
			}
			assertAdmissions(t, fixture.state, 0, "publication recovery preserves current work without model calls")
		})
	}
}

func TestTickExcludesOwnedAndIneligiblePublicationsBeforeDecodingEvidence(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	app := fixture.pausedApp(t)
	ctx, cancel := context.WithCancel(app.Context())
	defer cancel()
	preserved := map[string]map[string]any{}
	for _, kind := range []string{"active worker", "cleanup claim", "archived", "discarded", "no checkpoint", "published", "executing"} {
		task := queuedTask(fixture.cfg, model.ID(), fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+model.ID())
		task.Status = model.StatusPublishing
		task.OutputCommit = stringPointer("retained-output")
		switch kind {
		case "active worker":
			app.runtimeMu.Lock()
			app.runtime.tasks[task.ID] = taskJob{branch: task.Branch, cancel: cancel}
			app.runtimeMu.Unlock()
		case "cleanup claim":
			if !app.claimCleanup(cleanupTask, task.ID) {
				t.Fatal("fixture could not claim cleanup")
			}
		case "archived":
			task.Lifecycle.ArchivedAt = stringPointer(model.Now())
		case "discarded":
			task.Lifecycle.DiscardedAt = stringPointer(model.Now())
		case "no checkpoint":
			task.OutputCommit = nil
		case "published":
			task.Status = model.StatusPublished
		case "executing":
			task.Status = model.StatusExecuting
		}
		raw, err := wirejson.GenericMap(task)
		if err != nil {
			t.Fatal(err)
		}
		raw["sessions"] = "synthetic evidence decoding tripwire"
		if err := fixture.state.Put("task", task.ID, raw); err != nil {
			t.Fatal(err)
		}
		preserved[task.ID] = raw
	}
	orphan := queuedTask(fixture.cfg, model.ID(), fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+model.ID())
	orphan.Status = model.StatusPublishing
	orphan.OutputCommit = stringPointer("orphan-output")
	saveExecutionTask(t, fixture.planningFixture, orphan)
	if _, err := fixture.state.PublishingTasksExcept(nil); err == nil || !strings.Contains(err.Error(), "sessions") {
		t.Fatalf("unfiltered publishing query did not hit live evidence tripwire: %v", err)
	}
	if err := app.Tick(); err != nil {
		t.Fatalf("scheduler decoded owned or ineligible publication evidence: %v", err)
	}
	for id, raw := range preserved {
		if current, found, err := fixture.state.GetValue("task", id); err != nil || !found || !wirejson.Equal(current, raw) {
			t.Fatalf("scheduler rewrote excluded evidence for %s: %+v, %v", id, current, err)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("recovery cancelled the current publication")
	}
	if current := loadTask(t, fixture.state, orphan.ID); !blockedAs(current, model.BlockedReasonPublicationUncertain) {
		t.Fatalf("excluding live evidence prevented orphan recovery: %+v", current)
	}
}

func TestPublicationRecoveryPreservesBlockedStatusWhenActivityFails(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t)
	app := fixture.pausedApp(t)
	task := queuedTask(fixture.cfg, model.ID(), fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+model.ID())
	task.Status = model.StatusPublishing
	task.OutputCommit = stringPointer("retained-output")
	saveExecutionTask(t, fixture.planningFixture, task)
	schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery_activity BEFORE INSERT ON events
		WHEN NEW.kind='status' AND NEW.message='Blocked'
		BEGIN SELECT RAISE(ABORT, 'synthetic recovery activity refusal'); END`)
	if err := app.Tick(); err == nil || !strings.Contains(err.Error(), "synthetic recovery activity refusal") {
		t.Fatalf("recovery activity refusal = %v", err)
	}
	saved := loadTask(t, fixture.state, task.ID)
	if !blockedAs(saved, model.BlockedReasonPublicationUncertain) {
		t.Fatalf("activity refusal lost durable recovery: %+v", saved)
	}
	if err := app.Tick(); err != nil {
		t.Fatalf("durable recovery was repeated after an activity refusal: %v", err)
	}
	if current := loadTask(t, fixture.state, task.ID); !wirejson.Equal(current, saved) {
		t.Fatal("later tick rewrote completed recovery")
	}
	assertAdmissions(t, fixture.state, 0, "recovery never executes a model")
	if entries := publications(t, fixture.planningFixture); len(entries) != 0 {
		t.Fatalf("recovery attempted publication: %+v", entries)
	}
}
