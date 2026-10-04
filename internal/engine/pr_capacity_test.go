package engine

// Open-PR capacity: only a fresh, complete inventory from this process authorizes new-PR admissions.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestPRCapacityFreshness(t *testing.T) {
	t.Parallel()
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
	inventory := model.OpenPRInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: prs}

	capacity, err := a.prCapacity(cfg)
	if err != nil || capacity.Status != "unavailable" || capacity.OwnedOpen != nil || capacity.Remaining != nil || capacity.Reason == nil {
		t.Fatalf("capacity without any inventory was not fail-closed: %+v, %v", capacity, err)
	}
	if persisted, err := state.PersistPRInventory(inventory, nil); err != nil || !persisted {
		t.Fatalf("persist inventory: %t, %v", persisted, err)
	}
	capacity, err = a.prCapacity(cfg)
	if err != nil || capacity.Status != "unavailable" || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 3 || capacity.Remaining != nil {
		t.Fatalf("persisted inventory alone authorized capacity: %+v, %v", capacity, err)
	}

	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPRs{identity: store.PRIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now()}
	a.runtimeMu.Unlock()
	capacity, err = a.prCapacity(cfg)
	if err != nil || capacity.Status != "ready" || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 3 || capacity.Remaining == nil || *capacity.Remaining != 2 {
		t.Fatalf("fresh observation did not report (3 owned, 2 remaining): %+v, %v", capacity, err)
	}
	if capacity.ObservedAt == nil || *capacity.ObservedAt != inventory.ObservedAt {
		t.Fatalf("capacity lost the observation time: %+v", capacity)
	}

	a.runtimeMu.Lock()
	a.runtime.prRefreshError = "network refused"
	a.runtimeMu.Unlock()
	capacity, err = a.prCapacity(cfg)
	if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "network refused") {
		t.Fatalf("refresh failure did not revoke capacity with its reason: %+v, %v", capacity, err)
	}

	a.runtimeMu.Lock()
	a.runtime.prRefreshError = ""
	a.runtime.prObservation = &freshPRs{identity: store.PRIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now().Add(-(observeInterval + time.Minute))}
	a.runtimeMu.Unlock()
	capacity, err = a.prCapacity(cfg)
	if err != nil || capacity.Status != "ready" || capacity.Remaining == nil {
		t.Fatalf("observation within its lifetime was reported stale: %+v, %v", capacity, err)
	}

	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPRs{identity: store.PRIdentityOf(cfg), inventory: inventory.Clone(), fetchedAt: time.Now().Add(-(observationLifetime + time.Second))}
	a.runtimeMu.Unlock()
	capacity, err = a.prCapacity(cfg)
	if err != nil || capacity.Status != "unavailable" || capacity.Remaining != nil {
		t.Fatalf("stale observation authorized capacity: %+v, %v", capacity, err)
	}

	otherIdentity := store.PRIdentityOf(cfg)
	otherIdentity.BranchPrefix = "other/"
	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPRs{identity: otherIdentity, inventory: inventory.Clone(), fetchedAt: time.Now()}
	a.runtimeMu.Unlock()
	capacity, err = a.prCapacity(cfg)
	if err != nil || capacity.Status != "unavailable" || capacity.Reason == nil || !strings.Contains(*capacity.Reason, "Configuration changed") {
		t.Fatalf("observation under changed policy was not reported as such: %+v, %v", capacity, err)
	}

	wrongRepo := inventory.Clone()
	wrongRepo.Repository = "other/project"
	if persisted, err := state.PersistPRInventory(wrongRepo, nil); err != nil || persisted {
		t.Fatalf("foreign-repository inventory was persisted: %t, %v", persisted, err)
	}
	capacity, err = a.prCapacity(cfg)
	if err != nil || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 3 {
		t.Fatalf("foreign-repository inventory replaced the evidence: %+v, %v", capacity, err)
	}
}

func TestRefreshFailureRevokesCapacity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	queued := queuedTask(f.cfg, "waiting-for-capacity", f.cfg.DefaultBranch, f.cfg.BranchPrefix+"waiting")
	putTask(t, f, queued)
	app := New(f.state, f.dataDir)
	t.Cleanup(app.Shutdown)
	if err := control(app, "resume"); err != nil {
		t.Fatal(err)
	}
	if err := refreshLive(app); err != nil {
		t.Fatal(err)
	}
	before, err := app.prCapacity(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != "ready" {
		t.Fatalf("capacity after complete refresh = %s: %v", before.Status, before.Reason)
	}
	if err := os.WriteFile(filepath.Join(f.root, "prs.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refreshLive(app); err == nil {
		t.Fatal("malformed remote inventory unexpectedly refreshed")
	}
	after, err := app.prCapacity(f.cfg)
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
	saved, err := store.Get[model.Task](f.state, "task", queued.ID)
	if err != nil || saved.Status != model.StatusQueued {
		t.Fatalf("refresh failure changed queued work: %+v, %v", saved, err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "prs.json"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refreshLive(app); err != nil {
		t.Fatalf("restored inventory did not refresh: %v", err)
	}
	recovered, err := app.prCapacity(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != "ready" || recovered.Reason != nil || recovered.Remaining == nil || *recovered.Remaining != f.cfg.MaxOpenPRs {
		t.Fatalf("successful refresh did not clear the failure: %+v", recovered)
	}
}

func TestInventoryAdmitsOneBatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := f.cfg.Clone()
	cfg.BranchPrefix = "tyk/"
	cfg.MaxOpenPRs = 3
	cfg.ExecutionConcurrency = 2
	saveSettings(t, f.state, cfg, model.DefaultControl())
	for _, id := range []string{"first", "second", "third"} {
		putTask(t, f, queuedTask(cfg, id, cfg.DefaultBranch, cfg.BranchPrefix+id))
	}
	started := make(chan string, 3)
	app := New(f.state, f.dataDir)
	app.supervise = func(_ context.Context, task model.Task) error {
		started <- task.ID
		task.Status = model.StatusPublished
		return f.state.Put("task", task.ID, task)
	}
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	if err := control(app, "resume"); err != nil {
		t.Fatal(err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if len(started) != 0 {
		t.Fatal("tasks started before a complete inventory was available")
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if len(started) != 2 {
		t.Fatalf("one inventory admitted %d tasks; want both slots in the same batch", len(started))
	}
	capacity, err := app.prCapacity(cfg)
	if err != nil || capacity.Status != "ready" || capacity.Remaining == nil || *capacity.Remaining != 1 {
		t.Fatalf("consuming admission evidence lost dashboard capacity: %+v, %v", capacity, err)
	}

	git(t, f.root, "--git-dir", filepath.Join(f.root, "remote.git"), "branch", "tyk/another-session", "main")
	pr := `[{"number":7,"title":"Other owned work","body":"<!-- octomus:task:other -->","head":{"ref":"tyk/another-session","sha":"","repo":{"full_name":"fixture/project"}},"base":{"ref":"main","repo":{"full_name":"fixture/project"}},"html_url":"https://github.com/fixture/project/pull/7","state":"open","merged_at":null,"additions":1,"deletions":0,"created_at":"2026-09-07T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(f.root, "prs.json"), []byte(pr), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	if len(started) != 2 {
		t.Fatal("the previous inventory authorized a second admission batch")
	}
	capacity, err = app.prCapacity(cfg)
	if err != nil || capacity.Status != "full" || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 1 || capacity.Reserved != 2 {
		t.Fatalf("next batch did not refresh immediately and observe the full capacity: %+v, %v", capacity, err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	third, err := store.Get[model.Task](f.state, "task", "third")
	if err != nil || third == nil || third.Status != model.StatusQueued || len(started) != 2 {
		t.Fatalf("full inventory admitted a new PR: %+v, %v; started=%d", third, err, len(started))
	}
}
