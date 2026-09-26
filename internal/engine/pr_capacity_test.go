package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// persisted inventory is evidence only; capacity is reported as remaining only
// while this process holds a fresh, error-free observation under the live policy.
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

	// A current-process observation of the persisted inventory makes it ready.
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

	// A refresh failure revokes the fresh observation's authority.
	a.runtimeMu.Lock()
	a.runtime.prRefreshError = "network refused"
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "network refused") {
		t.Fatalf("refresh failure did not revoke capacity with its reason: %+v, %v", capacity, err)
	}

	// An observation older than one housekeeping interval stays fresh until
	// the next, possibly slower, observation pass has had time to land.
	a.runtimeMu.Lock()
	a.runtime.prRefreshError = ""
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now().Add(-(observeInterval + time.Minute))}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "ready" || capacity.Remaining == nil {
		t.Fatalf("observation within its lifetime was reported stale: %+v, %v", capacity, err)
	}

	// An observation older than its lifetime is stale.
	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now().Add(-(prObservationLifetime + time.Second))}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil {
		t.Fatalf("stale observation authorized capacity: %+v, %v", capacity, err)
	}

	// An observation made under a different branch prefix is not current policy.
	otherIdentity := store.PrIdentityOf(cfg)
	otherIdentity.BranchPrefix = "other/"
	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPrObservation{identity: otherIdentity, inventory: inventory.Clone(), fetchedAt: time.Now()}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "Configuration changed") {
		t.Fatalf("observation under changed policy was not reported as such: %+v, %v", capacity, err)
	}

	// An inventory for another repository is refused and leaves the evidence.
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

// an in-flight refresh reports refreshing, a failed one reports its error, and
// the next complete observation clears that error and restores ready capacity.
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

	// The refresh finished with a failure.
	app.runtimeMu.Lock()
	app.runtime.prRefresh = nil
	app.runtime.prRefreshError = "fixture gh failure"
	app.runtimeMu.Unlock()
	capacity, err = app.PrCapacity()
	if err != nil || capacity.Status != "unavailable" || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "fixture gh failure") {
		t.Fatalf("failed refresh did not report its error: %+v, %v", capacity, err)
	}

	// A complete observation of the empty fixture inventory clears the failure.
	if err := app.RefreshPRs(context.Background()); err != nil {
		t.Fatal(err)
	}
	capacity, err = app.PrCapacity()
	if err != nil || capacity.Status != "ready" || capacity.Remaining == nil || *capacity.Remaining != 5 {
		t.Fatalf("complete observation did not clear the failure: %+v, %v", capacity, err)
	}
}

// a refresh whose own context was cancelled (pause, configuration save,
// shutdown) is obsolete: its interrupted remote capture reports "Operation
// cancelled", which must not become the capacity failure reason.
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

// while paused, a successful housekeeping refresh supersedes an earlier refresh
// failure: the failure no longer describes the remote, but the paused service
// still gains no dispatch authority from the new observation.
func TestPausedRefreshClearsEarlierFailureWithoutAuthorizingDispatch(t *testing.T) {
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	app.runtimeMu.Lock()
	app.runtime.prRefreshError = "earlier fixture failure"
	app.runtimeMu.Unlock()
	if err := app.RefreshPRs(context.Background()); err != nil {
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

// a housekeeping observation whose refresh was superseded by a concurrent one
// that saved a newer complete inventory first is not a failure: the older
// inventory is refused, no refresh failure is recorded, and the observation
// finishes with the newer saved inventory.
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

// archiving an uncertain checkpoint ends at cancelled; the durable reservation
// it left behind can only be resolved by remote inspection, so recovery must
// not resurrect a released one. Published work is never reseeded either.
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
