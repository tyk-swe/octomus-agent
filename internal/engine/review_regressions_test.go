package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestTickAfterShutdownHasNoSideEffects(t *testing.T) {
	for _, scenario := range []string{"existing branch", "new PR", "planning", "paused"} {
		t.Run(scenario, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			if scenario != "paused" {
				control.SetMode(model.OperatingModeContinuous)
			}
			saveSettings(t, state, cfg, control)
			var queued *model.Task
			if scenario == "existing branch" || scenario == "new PR" {
				target := "tyk/existing"
				if scenario == "new PR" {
					target = cfg.DefaultBranch
				}
				task := queuedTask(cfg, "untouched", target, "tyk/existing")
				if err := state.Put("task", task.ID, task); err != nil {
					t.Fatal(err)
				}
				var err error
				queued, err = store.Get[model.Task](state, "task", task.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			started := make(chan struct{}, 1)
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
				started <- struct{}{}
				return nil
			})))
			t.Cleanup(app.Shutdown)
			app.Shutdown()
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			if len(started) != 0 {
				t.Error("dispatched work after shutdown")
			}
			if queued != nil {
				saved, err := store.Get[model.Task](state, "task", queued.ID)
				if err != nil || !reflect.DeepEqual(saved, queued) {
					t.Fatalf("shutdown tick changed queued work: %+v, %v", saved, err)
				}
			}
			live, err := app.Control()
			if err != nil || !controlsEqual(live, control) {
				t.Fatalf("shutdown tick changed control: %+v, %v", live, err)
			}
			if !app.runtime.lastRetention.IsZero() || !app.runtime.lastObserve.IsZero() || !app.runtime.lastPrAttempt.IsZero() {
				t.Fatal("shutdown tick launched background maintenance")
			}
		})
	}
}

func TestTaskCompletionCancelsContext(t *testing.T) {
	for _, runErr := range []error{nil, errors.New("worker failed")} {
		name := "success"
		if runErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			task := queuedTask(cfg, "completed", "tyk/existing", "tyk/existing")
			task.Status = model.StatusExecuting
			if err := state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			var workerCtx context.Context
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, task model.Task) error {
				workerCtx = ctx
				if runErr == nil {
					task.Status = model.StatusPublished
					return state.Put("task", task.ID, task)
				}
				return runErr
			})))
			t.Cleanup(app.Shutdown)
			app.runTask(task)
			app.wg.Wait()
			if workerCtx == nil || workerCtx.Err() != context.Canceled {
				t.Fatal("completed task retained a live child context")
			}
			if app.ctx.Err() != nil {
				t.Fatal("task completion canceled the application")
			}
		})
	}
}

func TestPrInventoryAuthorizesOnlyOneAdmissionBatch(t *testing.T) {
	fixture := newPlanningFixture(t)
	cfg := fixture.cfg.Clone()
	cfg.BranchPrefix = "tyk/"
	cfg.MaxOpenPRs = 3
	cfg.ExecutionConcurrency = 2
	saveSettings(t, fixture.state, cfg, model.DefaultControl())
	for _, id := range []string{"first", "second", "third"} {
		task := queuedTask(cfg, id, cfg.DefaultBranch, cfg.BranchPrefix+id)
		if err := fixture.state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan string, 3)
	app := New(fixture.state, fixture.dataDir, WithTaskRunner(TaskRunnerFunc(func(_ context.Context, task model.Task) error {
		started <- task.ID
		task.Status = model.StatusPublished
		return fixture.state.Put("task", task.ID, task)
	})))
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	// The first pass fetches an inventory; the next admits both available slots.
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if len(started) != 0 {
		t.Fatal("tasks started before a complete inventory was available")
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if len(started) != 2 {
		t.Fatalf("one inventory admitted %d tasks; want both slots in the same batch", len(started))
	}
	capacity, err := app.PrCapacity()
	if err != nil || capacity.Status != "ready" || capacity.Remaining == nil || *capacity.Remaining != 1 {
		t.Fatalf("consuming admission evidence lost dashboard capacity: %+v, %v", capacity, err)
	}

	// Another owned PR now fills the last slot alongside our two reservations.
	// The scheduler must observe it before admitting the next batch.
	command(t, fixture.root, "/usr/bin/git", "--git-dir", filepath.Join(fixture.root, "remote.git"), "branch", "tyk/another-session", "main")
	pr := `[{"number":7,"title":"Other owned work","body":"<!-- octomus:task:other -->","head":{"ref":"tyk/another-session","sha":"","repo":{"full_name":"fixture/project"}},"base":{"ref":"main","repo":{"full_name":"fixture/project"}},"html_url":"https://github.com/fixture/project/pull/7","state":"open","merged_at":null,"additions":1,"deletions":0,"created_at":"2026-09-07T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), []byte(pr), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if len(started) != 2 {
		t.Fatal("the previous inventory authorized a second admission batch")
	}
	capacity, err = app.PrCapacity()
	if err != nil || capacity.Status != "full" || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 1 || capacity.Reserved != 2 {
		t.Fatalf("next batch did not refresh immediately and observe the full capacity: %+v, %v", capacity, err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	third, err := store.Get[model.Task](fixture.state, "task", "third")
	if err != nil || third == nil || third.Status != model.StatusQueued || len(started) != 2 {
		t.Fatalf("full inventory admitted a new PR: %+v, %v; started=%d", third, err, len(started))
	}
}

func TestPrAdmissionFreshnessUsesInventoryObservation(t *testing.T) {
	cfg := testConfig(t.TempDir())
	app := New(testStore(t), t.TempDir())
	t.Cleanup(app.Shutdown)
	now := time.Now()
	for _, test := range []struct {
		name      string
		age       time.Duration
		invalid   bool
		wantFresh bool
	}{
		{name: "recent", age: 10 * time.Second, wantFresh: true},
		{name: "at admission limit", age: time.Minute, wantFresh: true},
		{name: "slow refresh within dashboard lifetime", age: 61 * time.Second},
		{name: "older than dashboard lifetime", age: 6 * time.Minute},
		{name: "invalid observation time", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := now.Add(-test.age).UTC().Format(time.RFC3339Nano)
			if test.invalid {
				observed = "invalid"
			}
			app.runtime.prObservation = &freshPrObservation{
				identity: store.PrIdentityOf(cfg), fetchedAt: now,
				inventory: model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: observed, PRs: []model.PullRequest{}},
			}
			inventory, reason := app.takePrAdmissionInventory(cfg, now)
			if (inventory != nil) != test.wantFresh {
				t.Fatalf("admission freshness = %t, want %t: %s", inventory != nil, test.wantFresh, reason)
			}
			if test.wantFresh {
				if second, _ := app.takePrAdmissionInventory(cfg, now); second != nil {
					t.Fatal("consumed inventory remained admission authority")
				}
			}
		})
	}
}

func TestRunOnceAcceptsPreviouslyPublishedDependency(t *testing.T) {
	for _, test := range []struct {
		name  string
		runID *string
	}{
		{name: "published in Continuous"},
		{name: "published in earlier RunOnce", runID: stringPointer("earlier-batch")},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			saveSettings(t, state, cfg, model.DefaultControl())
			dependency := queuedTask(cfg, "dependency", "tyk/existing", "tyk/existing")
			dependency.Status = model.StatusPublished
			dependency.RunID = test.runID
			dependent := queuedTask(cfg, "dependent", dependency.Branch, dependency.Branch)
			dependent.Proposal.Dependencies = []string{dependency.ID}
			for _, task := range []model.Task{dependency, dependent} {
				if err := state.Put("task", task.ID, task); err != nil {
					t.Fatal(err)
				}
			}
			app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(_ context.Context, task model.Task) error {
				task.Status = model.StatusPublished
				return state.Put("task", task.ID, task)
			})))
			t.Cleanup(app.Shutdown)
			app.runtime.lastRetention = time.Now()
			app.runtime.lastObserve = time.Now()
			if err := app.RunOnce(); err != nil {
				t.Fatal(err)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			saved, err := store.Get[model.Task](state, "task", dependent.ID)
			if err != nil || saved == nil || saved.Status != model.StatusPublished {
				t.Fatalf("published prerequisite blocked its RunOnce successor: %+v, %v", saved, err)
			}
		})
	}
}

func TestPlanningPreflightRejectsMissingAuthenticationBeforeSideEffects(t *testing.T) {
	for _, mode := range []string{"audit", "run_once", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newPlanningFixture(t)
			if err := os.WriteFile(filepath.Join(fixture.root, "codex-mode"), []byte("no-auth"), 0o600); err != nil {
				t.Fatal(err)
			}
			app := New(fixture.state, fixture.dataDir)
			t.Cleanup(app.Shutdown)
			app.runtime.lastRetention = time.Now()
			app.runtime.lastObserve = time.Now()
			if mode == "audit" {
				id, err := app.StartAudit(context.Background())
				if err == nil || !strings.Contains(err.Error(), "authentication") || id != "" {
					t.Fatalf("unauthenticated audit passed preflight: id=%q err=%v", id, err)
				}
			} else {
				var err error
				if mode == "run_once" {
					err = app.RunOnce()
				} else {
					err = app.Resume()
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
				app.wg.Wait()
				control, err := app.Control()
				if err != nil || control.Error == nil || !strings.Contains(*control.Error, "authentication") {
					t.Fatalf("missing durable preflight authentication error: %+v, %v", control, err)
				}
				if mode == "run_once" && (control.Mode != model.OperatingModePaused || control.Batch != nil) {
					t.Fatalf("RunOnce did not pause after failed preflight: %+v", control)
				}
				if mode == "continuous" && (control.Mode != model.OperatingModeContinuous || control.NextCycleAt <= time.Now().Unix()) {
					t.Fatalf("Continuous did not defer after failed preflight: %+v", control)
				}
			}
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 0 {
				t.Fatalf("unauthenticated preflight created cycles: %d, %v", len(cycles), err)
			}
			used, err := fixture.state.SessionsToday()
			if err != nil || used != 0 {
				t.Fatalf("unauthenticated preflight consumed admissions: %d, %v", used, err)
			}
			if !app.runtimeIdle() {
				t.Fatal("failed preflight retained runtime work")
			}
		})
	}
}

func TestRunnerStorageDistinguishesUnavailableFromEmpty(t *testing.T) {
	state := testStore(t)
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	cfg := testConfig(t.TempDir())
	empty := t.TempDir()
	populated := t.TempDir()
	file := filepath.Join(populated, "transcript")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing-mount")
	type measurement struct {
		Bytes   *uint64                `json:"bytes"`
		Status  string                 `json:"status"`
		Runners map[string]measurement `json:"runners"`
	}
	type usage struct {
		RunnerTranscripts measurement `json:"runner_transcripts"`
		TaskBytes         uint64      `json:"task_bytes"`
		PlanningBytes     uint64      `json:"planning_bytes"`
	}
	for _, test := range []struct {
		name       string
		paths      map[string]string
		codexState string
		state      string
		bytes      uint64
	}{
		{name: "unconfigured", paths: map[string]string{}, codexState: "unconfigured", state: "unavailable"},
		{name: "missing directory", paths: map[string]string{"codex": missing}, codexState: "unavailable", state: "unavailable"},
		{name: "not a directory", paths: map[string]string{"codex": file}, codexState: "unavailable", state: "unavailable"},
		{name: "empty directory", paths: map[string]string{"codex": empty}, codexState: "measured", state: "partial"},
		{name: "one missing runner", paths: map[string]string{"codex": missing, "opencode": populated}, codexState: "unavailable", state: "partial", bytes: 5},
		{name: "both measured", paths: map[string]string{"codex": populated, "opencode": empty}, codexState: "measured", state: "measured", bytes: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg.RunnerStoragePaths = test.paths
			if err := app.measureStorage(cfg); err != nil {
				t.Fatal(err)
			}
			saved, err := store.Get[usage](state, "settings", "storage")
			if err != nil || saved == nil {
				t.Fatalf("storage measurement: %+v, %v", saved, err)
			}
			runners := saved.RunnerTranscripts
			codex := runners.Runners["codex"]
			if codex.Status != test.codexState || (codex.Bytes != nil) != (test.codexState == "measured") {
				t.Fatalf("runner measurement = %+v; want %s", codex, test.codexState)
			}
			if runners.Status != test.state || (runners.Bytes != nil) != (test.state != "unavailable") || runners.Bytes != nil && *runners.Bytes != test.bytes {
				t.Fatalf("aggregate measurement = %+v; want %s, %d bytes", runners, test.state, test.bytes)
			}
			if saved.TaskBytes != 0 || saved.PlanningBytes != 0 {
				t.Fatalf("absent application workspaces should measure zero: %+v", saved)
			}
		})
	}
}
