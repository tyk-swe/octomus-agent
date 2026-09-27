package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func refreshLive(app *App) error {
	cfg, err := app.Config()
	if err != nil {
		return err
	}
	return app.refreshPRs(context.Background(), cfg)
}

func TestCapacityReportsOnlyFreshCurrentProcessObservations(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	prs := []model.PullRequest{}
	for n := uint64(1); n <= 3; n++ {
		pr := ownedPR(fmt.Sprintf("octomus/open-%d", n))
		pr.Number = n
		prs = append(prs, pr)
	}
	inventory := model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: prs}

	capacity, err := a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.OwnedOpen != nil || capacity.Remaining != nil || capacity.Reason == nil {
		t.Fatalf("capacity without any inventory was not fail-closed: %+v, %v", capacity, err)
	}
	if persisted, err := state.PersistPrInventory(inventory, nil); err != nil || !persisted {
		t.Fatalf("persist inventory: %t, %v", persisted, err)
	}
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 3 || capacity.Remaining != nil {
		t.Fatalf("persisted inventory alone authorized capacity: %+v, %v", capacity, err)
	}

	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now()}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "ready" || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 3 || capacity.Remaining == nil || *capacity.Remaining != 2 {
		t.Fatalf("fresh observation did not report (3 owned, 2 remaining): %+v, %v", capacity, err)
	}
	if capacity.ObservedAt == nil || *capacity.ObservedAt != inventory.ObservedAt {
		t.Fatalf("capacity lost the observation time: %+v", capacity)
	}

	a.runtimeMu.Lock()
	a.runtime.prRefreshError = "network refused"
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "network refused") {
		t.Fatalf("refresh failure did not revoke capacity with its reason: %+v, %v", capacity, err)
	}

	a.runtimeMu.Lock()
	a.runtime.prRefreshError = ""
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now().Add(-(observeInterval + time.Minute))}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "ready" || capacity.Remaining == nil {
		t.Fatalf("observation within its lifetime was reported stale: %+v, %v", capacity, err)
	}

	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now().Add(-(prObservationLifetime + time.Second))}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil {
		t.Fatalf("stale observation authorized capacity: %+v, %v", capacity, err)
	}

	otherIdentity := store.PrIdentityOf(cfg)
	otherIdentity.BranchPrefix = "other/"
	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPrObservation{identity: otherIdentity, inventory: inventory.Clone(), fetchedAt: time.Now()}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "Configuration changed") {
		t.Fatalf("observation under changed policy was not reported as such: %+v, %v", capacity, err)
	}

	wrongRepo := inventory.Clone()
	wrongRepo.Repository = "other/project"
	if persisted, err := state.PersistPrInventory(wrongRepo, nil); err != nil || persisted {
		t.Fatalf("foreign-repository inventory was persisted: %t, %v", persisted, err)
	}
	capacity, err = a.PrCapacity()
	if err != nil || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 3 {
		t.Fatalf("foreign-repository inventory replaced the evidence: %+v, %v", capacity, err)
	}
}

func TestCapacityReportsRefreshStateAndClearsErrorAfterObservation(t *testing.T) {
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app.runtimeMu.Lock()
	app.runtime.prRefresh = &prRefreshJob{cancel: cancel}
	app.runtimeMu.Unlock()
	capacity, err := app.PrCapacity()
	if err != nil || capacity.Status != "refreshing" {
		t.Fatalf("in-flight refresh was not reported: %+v, %v", capacity, err)
	}

	app.runtimeMu.Lock()
	app.runtime.prRefresh = nil
	app.runtime.prRefreshError = "fixture gh failure"
	app.runtimeMu.Unlock()
	capacity, err = app.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "fixture gh failure") {
		t.Fatalf("failed refresh did not report its error: %+v, %v", capacity, err)
	}

	if err := refreshLive(app); err != nil {
		t.Fatal(err)
	}
	capacity, err = app.PrCapacity()
	if err != nil || capacity.Status != "ready" || capacity.Remaining == nil || *capacity.Remaining != 5 {
		t.Fatalf("complete observation did not clear the failure: %+v, %v", capacity, err)
	}
}

func TestCancelledRefreshIsNotRecordedAsFailure(t *testing.T) {
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.refreshPRs(ctx, fixture.cfg); err == nil {
		t.Fatal("a cancelled refresh unexpectedly completed")
	}
	app.runtimeMu.Lock()
	refreshError := app.runtime.prRefreshError
	app.runtimeMu.Unlock()
	if refreshError != "" {
		t.Fatalf("cancelled refresh was recorded as a failure: %q", refreshError)
	}
	capacity, err := app.PrCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Status != "unavailable" || capacity.Reason == nil || strings.Contains(strings.ToLower(*capacity.Reason), "cancelled") {
		t.Fatalf("cancelled refresh changed the capacity reason: %+v", capacity)
	}
}

func TestPausedRefreshClearsEarlierFailureWithoutAuthorizingDispatch(t *testing.T) {
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	app.runtimeMu.Lock()
	app.runtime.prRefreshError = "earlier fixture failure"
	app.runtimeMu.Unlock()
	if err := refreshLive(app); err != nil {
		t.Fatal(err)
	}
	app.runtimeMu.Lock()
	refreshError, observation := app.runtime.prRefreshError, app.runtime.prObservation
	app.runtimeMu.Unlock()
	if refreshError != "" {
		t.Fatalf("successful paused refresh kept the earlier failure: %q", refreshError)
	}
	if observation != nil {
		t.Fatal("paused refresh gained dispatch authority")
	}
	capacity, err := app.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil || capacity.Reason == nil || strings.Contains(*capacity.Reason, "earlier fixture failure") {
		t.Fatalf("paused capacity after a successful refresh: %+v, %v", capacity, err)
	}
}

func TestObservationContinuesWithASupersedingInventory(t *testing.T) {
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	newer := model.OpenPrInventory{Repository: fixture.cfg.GitHubRepo, ObservedAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), PRs: []model.PullRequest{}}
	if persisted, err := fixture.state.PersistPrInventory(newer, nil); err != nil || !persisted {
		t.Fatalf("persist newer inventory: %t, %v", persisted, err)
	}
	if err := app.observeRemote(context.Background(), fixture.cfg); err != nil {
		t.Fatalf("superseded refresh failed the observation: %v", err)
	}
	control, err := app.Control()
	if err != nil || control.ContextFingerprint == "" {
		t.Fatalf("observation did not record the context fingerprint: %+v, %v", control, err)
	}
	app.runtimeMu.Lock()
	refreshError := app.runtime.prRefreshError
	app.runtimeMu.Unlock()
	if refreshError != "" {
		t.Fatalf("superseded refresh was recorded as a failure: %q", refreshError)
	}
	stored, err := fixture.state.OpenPrInventory()
	if err != nil || stored == nil || stored.ObservedAt != newer.ObservedAt {
		t.Fatalf("older refresh replaced the newer inventory: %+v, %v", stored, err)
	}
}

func TestCancelledCheckpointsAreNeverReseeded(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	archived := queuedTask(cfg, "archived-uncertain", cfg.DefaultBranch, cfg.BranchPrefix+"archived-uncertain")
	archived.Status = model.StatusCancelled
	archivedCommit := strings.Repeat("e", 40)
	archived.OutputCommit = &archivedCommit
	archivedAt := model.Now()
	archived.Lifecycle.ArchivedAt = &archivedAt
	delivered := queuedTask(cfg, "delivered", cfg.DefaultBranch, cfg.BranchPrefix+"delivered")
	delivered.Status = model.StatusPublished
	deliveredCommit := strings.Repeat("f", 40)
	delivered.OutputCommit = &deliveredCommit
	for _, task := range []model.Task{archived, delivered} {
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if err := a.Recover(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{archived.ID, delivered.ID} {
		if has, err := state.HasPrReservation(id); err != nil || has {
			t.Fatalf("recovery reseeded a reservation for %s: %t, %v", id, has, err)
		}
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
	deferHousekeeping(app)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	waitForFixtureFile(t, filepath.Join(fixture.root, "reconcile-processes.jsonl"), "capacity refresh did not reach the deterministic barrier")
	if err := app.Pause(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
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
	if err := refreshLive(app); err != nil {
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
	if err := refreshLive(app); err == nil {
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
	if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refreshLive(app); err != nil {
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
	if err := refreshLive(app); err != nil {
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
	deferHousekeeping(app)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
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

	git(t, fixture.root, "--git-dir", filepath.Join(fixture.root, "remote.git"), "branch", "tyk/another-session", "main")
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

func TestRefusedAdmissionPacesInventoryRefreshes(t *testing.T) {
	for _, test := range []struct {
		name   string
		stale  func(*config.Config)
		live   func(*config.Config)
		admits bool
	}{
		{name: "respelled repository path", live: func(cfg *config.Config) { cfg.Repository += "/" }, admits: true},
		{name: "persistently refused task", stale: func(cfg *config.Config) { cfg.BranchPrefix = "stale/" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPlanningFixture(t)
			cfg := fixture.cfg.Clone()
			cfg.BranchPrefix = "tyk/"
			planned := cfg.Clone()
			if test.stale != nil {
				test.stale(&planned)
			}
			task := queuedTask(planned, "planned", planned.DefaultBranch, planned.BranchPrefix+"planned")
			if err := fixture.state.Put("task", task.ID, task); err != nil {
				t.Fatal(err)
			}
			if test.live != nil {
				test.live(&cfg)
			}
			saveSettings(t, fixture.state, cfg, model.DefaultControl())
			started := make(chan string, 1)
			app := New(fixture.state, fixture.dataDir, WithTaskRunner(TaskRunnerFunc(func(_ context.Context, task model.Task) error {
				started <- task.ID
				task.Status = model.StatusPublished
				return fixture.state.Put("task", task.ID, task)
			})))
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			if err := app.Resume(); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8 && len(started) == 0; i++ {
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
				app.wg.Wait()
			}
			if admitted := len(started) == 1; admitted != test.admits {
				t.Fatalf("task admitted = %t; want %t", admitted, test.admits)
			}
			log, err := os.ReadFile(filepath.Join(fixture.root, "gh-api.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if reads := strings.Count(string(log), "state=open"); reads > 2 {
				t.Fatalf("eight ticks read the open-PR inventory %d times; want the retry delay to pace refreshes", reads)
			}
		})
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
		{name: "older than dashboard lifetime", age: prObservationLifetime + time.Minute},
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
