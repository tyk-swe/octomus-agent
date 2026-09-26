package engine

// These tests use deterministic local Git/GitHub/Codex peers. They make no
// network requests and perform no model calls.
import (
	"context"
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
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	pythonFixtureShim(t, filepath.Join(root, "bin", "git"), root, "git.py")
	codex := filepath.Join(root, "codex")
	pythonFixtureShim(t, codex, root, "codex.py")
	return newFixture(t, root, func(cfg *config.Config) {
		cfg.CodexBinary = codex
		cfg.OpencodeBinary = "/no-opencode-installed"
	})
}

// newFixture builds the shared local fixture under root: a bare remote at
// remote.git, a pushed clone at repository, the gh peer in root/bin (which may
// already hold other shims) at the front of PATH, an isolated environment, and
// a store holding the test settings once configure has adjusted them.
func newFixture(t *testing.T, root string, configure func(*config.Config)) *planningFixture {
	t.Helper()
	bin := filepath.Join(root, "bin")
	repo := filepath.Join(root, "repository")
	remote := filepath.Join(root, "remote.git")
	dataDir := filepath.Join(root, "data")
	for _, directory := range []string{bin, repo, dataDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pythonFixtureShim(t, filepath.Join(bin, "gh"), root, "gh.py")
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
	cfg.SessionTimeoutSeconds = 15
	cfg.CommandTimeoutSeconds = 5
	cfg.MaxSessionsPerDay = 30
	configure(&cfg)
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
	// The pause made the held refresh obsolete; its cancellation is not a
	// remote inventory failure to report for the rest of the pause.
	app.runtimeMu.Lock()
	refreshError := app.runtime.prRefreshError
	app.runtimeMu.Unlock()
	if refreshError != "" {
		t.Fatalf("cancelled refresh was recorded as a failure: %q", refreshError)
	}
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
	// A later complete observation clears the failure and restores ready
	// capacity — capacity_reports_refresh_state_and_clears_error_after_observation.
	if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.RefreshPRs(context.Background()); err != nil {
		t.Fatalf("restored inventory did not refresh: %v", err)
	}
	recovered, err := app.PrCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "ready" || recovered.Reason != nil || recovered.Remaining == nil || *recovered.Remaining != fixture.cfg.MaxOpenPRs {
		t.Fatalf("successful refresh did not clear the failure: %+v", recovered)
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
