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

// Port of tests/pr_capacity.rs capacity_reports_only_fresh_current_process_observations:
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

	// An observation older than five minutes is stale.
	a.runtimeMu.Lock()
	a.runtime.prRefreshError = ""
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now().Add(-301 * time.Second)}
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

// Port of tests/pr_capacity.rs capacity_reports_refresh_state_and_clears_error_after_observation:
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

// Port of tests/pr_capacity.rs cancelled_checkpoints_are_never_reseeded:
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
