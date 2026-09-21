package engine

// These tests use deterministic local Git/GitHub/Codex peers. They make no
// network requests and perform no model calls.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

type planningFixture struct {
	root    string
	dataDir string
	repo    string
	cfg     config.Config
	state   *store.Store
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func pythonFixtureShim(t *testing.T, path, root, fixture string) {
	t.Helper()
	fixtures := filepath.Join(repositoryRoot(t), "tests", "fixtures")
	script := fmt.Sprintf("#!/usr/bin/env python3\nimport os, runpy, sys\nos.environ['OCTOMUS_FIXTURE'] = %s\nsys.path.insert(0, %s)\nrunpy.run_path(%s, run_name='__main__')\n", strconv.Quote(root), strconv.Quote(fixtures), strconv.Quote(filepath.Join(fixtures, fixture)))
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func command(t *testing.T, directory, executable string, args ...string) {
	t.Helper()
	cmd := exec.Command(executable, args...)
	cmd.Dir = directory
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", executable, strings.Join(args, " "), err, output)
	}
}

func newPlanningFixture(t *testing.T) *planningFixture {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	repo := filepath.Join(root, "repository")
	remote := filepath.Join(root, "remote.git")
	dataDir := filepath.Join(root, "data")
	for _, directory := range []string{bin, repo, dataDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pythonFixtureShim(t, filepath.Join(bin, "git"), root, "git.py")
	pythonFixtureShim(t, filepath.Join(bin, "gh"), root, "gh.py")
	codex := filepath.Join(root, "codex")
	pythonFixtureShim(t, codex, root, "codex.py")
	if err := os.WriteFile(filepath.Join(root, "prs.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}

	command(t, root, "/usr/bin/git", "init", "--bare", "--initial-branch=main", remote)
	command(t, root, "/usr/bin/git", "init", "--initial-branch=main", repo)
	command(t, repo, "/usr/bin/git", "config", "user.name", "Fixture")
	command(t, repo, "/usr/bin/git", "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Fixture\n\nThe feature contract requires fixed output.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command(t, repo, "/usr/bin/git", "add", "README.md")
	command(t, repo, "/usr/bin/git", "commit", "-m", "Initial fixture")
	command(t, repo, "/usr/bin/git", "remote", "add", "origin", remote)
	command(t, repo, "/usr/bin/git", "push", "-u", "origin", "main")

	previousWebhook, hadWebhook := os.LookupEnv(store.WebhookEnv)
	if err := os.Unsetenv(store.WebhookEnv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadWebhook {
			_ = os.Setenv(store.WebhookEnv, previousWebhook)
		} else {
			_ = os.Unsetenv(store.WebhookEnv)
		}
	})
	t.Setenv("OCTOMUS_FIXTURE", root)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := testConfig(repo)
	cfg.CodexBinary = codex
	cfg.OpencodeBinary = "/no-opencode-installed"
	cfg.SessionTimeoutSeconds = 15
	cfg.CommandTimeoutSeconds = 5
	cfg.MaxSessionsPerDay = 30
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	saveSettings(t, state, cfg, model.DefaultControl())
	return &planningFixture{root: root, dataDir: dataDir, repo: repo, cfg: cfg, state: state}
}

func waitCycle(t *testing.T, state *store.Store, id string) model.Cycle {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cycle, err := store.Get[model.Cycle](state, "cycle", id)
		if err != nil {
			t.Fatal(err)
		}
		if cycle != nil && cycle.Status != model.CycleRunning {
			return *cycle
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cycle %s did not finish", id)
	return model.Cycle{}
}

func assertCompletePlanningPass(t *testing.T, fixture *planningFixture, cycle model.Cycle) {
	t.Helper()
	want := int(fixture.cfg.DiscoveryAgents + 4)
	if cycle.Status != model.CycleCompleted || len(cycle.Sessions) != want || len(cycle.Assessments) != 2 {
		errorText := ""
		if cycle.Error != nil {
			errorText = *cycle.Error
		}
		t.Fatalf("incomplete planning pass: status=%s sessions=%d/%d assessments=%d error=%s", cycle.Status, len(cycle.Sessions), want, len(cycle.Assessments), errorText)
	}
	seenSessions := map[string]struct{}{}
	roles := map[string]int{}
	for _, session := range cycle.Sessions {
		if session.Status != model.SessionCompleted {
			t.Fatalf("nonterminal planning session: %+v", session)
		}
		if _, duplicate := seenSessions[session.ID]; duplicate {
			t.Fatalf("planning session %s was reused", session.ID)
		}
		seenSessions[session.ID] = struct{}{}
		roles[session.Role]++
		workspace := filepath.Join(fixture.dataDir, "cycles", cycle.ID, session.Role, "workspace")
		if _, err := os.Stat(workspace); !os.IsNotExist(err) {
			t.Fatalf("successful immutable role clone remains at %s: %v", workspace, err)
		}
	}
	if roles["adversary-a"] != 1 || roles["adversary-b"] != 1 || roles["consolidation"] != 1 || roles["grounding"] != 1 {
		t.Fatalf("missing independent planning roles: %+v", roles)
	}
	for i := uint64(0); i < fixture.cfg.DiscoveryAgents; i++ {
		if roles[fmt.Sprintf("discovery-%d", i)] != 1 {
			t.Fatalf("missing discovery-%d: %+v", i, roles)
		}
	}
	protocol, err := os.ReadFile(filepath.Join(fixture.root, "protocol.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	turns := bytes.Split(bytes.TrimSpace(protocol), []byte("\n"))
	if len(turns) != want {
		t.Fatalf("planning pass used %d runner turns; want one per role (%d)", len(turns), want)
	}
	seenWorkspaces := map[string]struct{}{}
	for _, line := range turns {
		var turn struct {
			Thread string `json:"thread"`
			CWD    string `json:"cwd"`
		}
		if err := json.Unmarshal(line, &turn); err != nil {
			t.Fatal(err)
		}
		if _, ok := seenSessions[turn.Thread]; !ok {
			t.Fatalf("runner turn used unknown planning session %q", turn.Thread)
		}
		if _, duplicate := seenWorkspaces[turn.CWD]; duplicate {
			t.Fatalf("planning roles shared workspace %s", turn.CWD)
		}
		seenWorkspaces[turn.CWD] = struct{}{}
	}
}

func TestAuditRunsCompleteIndependentPlanWithoutQueueingWork(t *testing.T) {
	fixture := newPlanningFixture(t)
	existing := queuedTask(fixture.cfg, "already-queued", fixture.cfg.DefaultBranch, "octomus/already-queued")
	if err := fixture.state.Put("task", existing.ID, existing); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	assertCompletePlanningPass(t, fixture, cycle)
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Batch != nil {
		t.Fatalf("audit changed queue mode: %+v, %v", control, err)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 1 || tasks[0].ID != existing.ID || tasks[0].Status != model.StatusQueued || tasks[0].RunID != nil {
		t.Fatalf("audit disturbed or queued executable work: %+v, %v", tasks, err)
	}
	used, err := fixture.state.SessionsToday()
	if err != nil || used != fixture.cfg.PlanningAdmissionsRequired() {
		t.Fatalf("audit admissions = %d, %v; want %d", used, err, fixture.cfg.PlanningAdmissionsRequired())
	}
}

func TestFailedPlanningCommitsNoPartialQueueOrDecisionMemory(t *testing.T) {
	fixture := newPlanningFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.root, "failed-discovery"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var failed model.Cycle
	for time.Now().Before(deadline) {
		cycles, err := store.List[model.Cycle](fixture.state, "cycle")
		if err != nil {
			t.Fatal(err)
		}
		if len(cycles) == 1 && cycles[0].Status != model.CycleRunning {
			failed = cycles[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if failed.Status != model.CycleFailed || len(failed.Sessions) == 0 {
		t.Fatalf("failed plan did not retain terminal evidence: %+v", failed)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("partial plan leaked tasks: %+v, %v", tasks, err)
	}
	memory, err := fixture.state.DecisionMemory(fixture.cfg.GitHubRepo)
	if err != nil || len(memory) != 0 {
		t.Fatalf("partial plan leaked decision memory: %+v, %v", memory, err)
	}
	control, _ := app.Control()
	if control.Mode != model.OperatingModePaused || control.Batch != nil || control.Error == nil {
		t.Fatalf("failed RunOnce planning was not paused: %+v", control)
	}
}

func TestPlanningRejectsAndPreservesAMutatedRoleWorkspace(t *testing.T) {
	fixture := newPlanningFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.root, "mutate-planning"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "modified its source snapshot") {
		t.Fatalf("mutated planning workspace was accepted: %+v", cycle)
	}
	foundFailedSession := false
	for _, session := range cycle.Sessions {
		if session.Role == "discovery-0" && session.Status == model.SessionFailed {
			foundFailedSession = true
		}
	}
	if !foundFailedSession {
		t.Fatalf("mutated planning role lost terminal session evidence: %+v", cycle.Sessions)
	}
	mutation := filepath.Join(fixture.dataDir, "cycles", cycle.ID, "discovery-0", "workspace", "planning-mutation.txt")
	if _, err := os.Stat(mutation); err != nil {
		t.Fatalf("failed planning workspace was not preserved: %v", err)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("mutated planning workspace leaked executable work: %+v, %v", tasks, err)
	}
}

func TestConsolidationMustAccountForEveryOriginalProposal(t *testing.T) {
	fixture := newPlanningFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.root, "audit-malformed"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "omitted or invented") {
		t.Fatalf("incomplete consolidation was accepted: %+v", cycle)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("incomplete consolidation leaked executable work: %+v, %v", tasks, err)
	}
	app.wg.Wait() // Cycle status is persisted before control finalization finishes.
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Error == nil || !strings.Contains(*control.Error, "omitted or invented") {
		t.Fatalf("failed audit did not retain durable control evidence: %+v, %v", control, err)
	}
}

func TestRemotePreflightDoesNotHoldControlLockAndRejectsChangedPolicy(t *testing.T) {
	fixture := newPlanningFixture(t)
	hold := filepath.Join(fixture.root, "reconcile-hold")
	if err := os.WriteFile(hold, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(hold) })
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	type preflightResult struct {
		id  string
		err error
	}
	finished := make(chan preflightResult, 1)
	go func() {
		id, err := app.StartAudit(context.Background())
		finished <- preflightResult{id: id, err: err}
	}()
	entered := filepath.Join(fixture.root, "reconcile-entered")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(entered); err != nil {
		t.Fatal("remote preflight did not reach the deterministic barrier")
	}
	paused := make(chan error, 1)
	go func() { paused <- app.Pause() }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote preflight held the control lock")
	}
	changed := fixture.cfg.Clone()
	changed.GitHubRepo = "fixture/changed"
	if err := fixture.state.Put("settings", "config", changed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	outcome := <-finished
	if outcome.err == nil || outcome.id != "" {
		t.Fatalf("stale preflight committed after policy changed: %+v", outcome)
	}
	cycles, err := store.List[model.Cycle](fixture.state, "cycle")
	if err != nil || len(cycles) != 0 {
		t.Fatalf("stale preflight created a cycle: %d, %v", len(cycles), err)
	}
}

func TestPlanningAllowanceConsumedDuringPreflightUsesModeSemantics(t *testing.T) {
	for _, test := range []struct {
		name    string
		runOnce bool
	}{
		{name: "run once pauses with capacity event", runOnce: true},
		{name: "continuous waits without an error", runOnce: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPlanningFixture(t)
			cfg := fixture.cfg.Clone()
			cfg.MaxSessionsPerDay = cfg.PlanningAdmissionsRequired()
			if err := fixture.state.Put("settings", "config", cfg); err != nil {
				t.Fatal(err)
			}
			hold := filepath.Join(fixture.root, "reconcile-hold")
			if err := os.WriteFile(hold, []byte("1"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(hold) })
			app := New(fixture.state, fixture.dataDir)
			t.Cleanup(app.Shutdown)
			app.runtime.lastRetention = time.Now()
			app.runtime.lastObserve = time.Now()
			if test.runOnce {
				if err := app.RunOnce(); err != nil {
					t.Fatal(err)
				}
			} else if err := app.Resume(); err != nil {
				t.Fatal(err)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			entered := filepath.Join(fixture.root, "reconcile-entered")
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(entered); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if _, err := os.Stat(entered); err != nil {
				t.Fatal("planning preflight did not reach the deterministic barrier")
			}
			if err := fixture.state.ReserveSession(0, store.NewAdmission("competing-cycle", nil, "discovery", cfg.Roles["discovery"])); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(hold); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()

			control, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 0 {
				t.Fatalf("capacity race created a cycle: %d, %v", len(cycles), err)
			}
			if test.runOnce {
				if control.Mode != model.OperatingModePaused || control.Batch != nil || control.Error == nil {
					t.Fatalf("RunOnce capacity race did not pause: %+v", control)
				}
				events, err := fixture.state.Events(nil)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, event := range events {
					if event.Kind == "planning_capacity" {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("RunOnce capacity race recorded %d planning-capacity events", count)
				}
			} else if control.Mode != model.OperatingModeContinuous || control.Error != nil || control.NextCycleAt <= time.Now().Unix() {
				t.Fatalf("Continuous capacity race did not wait for UTC reset: %+v", control)
			}
		})
	}
}

func TestPauseCancelsHeldCapacityRefreshAndRejectsItsResult(t *testing.T) {
	fixture := newPlanningFixture(t)
	delay := filepath.Join(fixture.root, "reconcile-delay")
	if err := os.WriteFile(delay, []byte("60"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(delay) })
	queued := queuedTask(fixture.cfg, "waiting-for-refresh", fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+"waiting")
	if err := fixture.state.Put("task", queued.ID, queued); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir, WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error { return nil })))
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	entered := filepath.Join(fixture.root, "reconcile-processes.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(entered); err != nil {
		t.Fatal("capacity refresh did not reach the deterministic barrier")
	}
	if err := app.Pause(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if err := os.Remove(delay); err != nil {
		t.Fatal(err)
	}
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	capacity, err := app.PrCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Status != "unavailable" || capacity.Remaining != nil {
		t.Fatalf("cancelled refresh authorized resumed work: %+v", capacity)
	}
	saved, err := store.Get[model.Task](fixture.state, "task", queued.ID)
	if err != nil || saved.Status != model.StatusQueued {
		t.Fatalf("held refresh changed queued work: %+v, %v", saved, err)
	}
}

func TestRefreshFailureImmediatelyRevokesPrCapacity(t *testing.T) {
	fixture := newPlanningFixture(t)
	queued := queuedTask(fixture.cfg, "waiting-for-capacity", fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+"waiting")
	if err := fixture.state.Put("task", queued.ID, queued); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := app.RefreshPRs(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := app.PrCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "ready" {
		t.Fatalf("capacity after complete refresh = %s: %v", before.Status, before.Reason)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.RefreshPRs(context.Background()); err == nil {
		t.Fatal("malformed remote inventory unexpectedly refreshed")
	}
	after, err := app.PrCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "unavailable" || after.Reason == nil {
		t.Fatalf("capacity survived failed refresh: %+v", after)
	}
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModeContinuous || control.Error != nil {
		t.Fatalf("refresh failure failed Continuous operation: %+v, %v", control, err)
	}
	saved, err := store.Get[model.Task](fixture.state, "task", queued.ID)
	if err != nil || saved.Status != model.StatusQueued {
		t.Fatalf("refresh failure changed queued work: %+v, %v", saved, err)
	}
}

func TestRefreshReleasesOnlyRemotelySettledCheckpointReservation(t *testing.T) {
	fixture := newPlanningFixture(t)
	checkpoint := queuedTask(fixture.cfg, "cancelled-checkpoint", fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+"checkpoint")
	checkpoint.Status = model.StatusCancelled
	commit := "checkpoint-output"
	checkpoint.OutputCommit = &commit
	if err := fixture.state.Put("task", checkpoint.ID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.SeedPrReservation(checkpoint); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := app.RefreshPRs(context.Background()); err != nil {
		t.Fatal(err)
	}
	reservations, err := fixture.state.PrReservations(fixture.cfg.GitHubRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(reservations) != 0 {
		t.Fatalf("remote proved publication absent but reservation remained: %+v", reservations)
	}
}

func TestRunOnceCommitsCompletePlanningQueueAndPhase(t *testing.T) {
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var cycle model.Cycle
	for time.Now().Before(deadline) {
		cycles, err := store.List[model.Cycle](fixture.state, "cycle")
		if err != nil {
			t.Fatal(err)
		}
		if len(cycles) == 1 && cycles[0].Status != model.CycleRunning {
			cycle = cycles[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cycle.ID == "" {
		t.Fatal("execution planning cycle did not finish")
	}
	assertCompletePlanningPass(t, fixture, cycle)
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("complete plan did not commit one task: %d, %v", len(tasks), err)
	}
	control, _ := app.Control()
	if control.Mode != model.OperatingModeRunOnce || control.Batch == nil || control.Batch.Phase != model.BatchPhaseExecuting || tasks[0].RunID == nil || *tasks[0].RunID != control.Batch.ID {
		t.Fatalf("queue and RunOnce phase were not committed together: control=%+v task=%+v", control, tasks[0])
	}
	if tasks[0].SourceRevision == "" || tasks[0].DefaultRevision == "" || tasks[0].Branch == "" || tasks[0].AttemptPolicy == nil {
		t.Fatalf("planned task did not snapshot execution inputs: %+v", tasks[0])
	}
}
