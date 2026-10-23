package engine

// The fleet: two registered repositories dispatch their own queued work
// independently in one Run, and neither App supervises the other's tasks.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestFleetDispatchesRepositoriesIndependently(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	repos := []string{"fixture/one", "fixture/two"}
	tasks := map[string]model.Task{}
	for i, repo := range repos {
		cfg := testConfig(t.TempDir())
		cfg.GitHubRepo = repo
		// Registration precedes settings, as in the operator flow, so the
		// primary repository's legacy settings identities are settled first
		dataDirEntry := "repos/" + store.RepositorySlug(repo)
		if i == 0 {
			dataDirEntry = "."
		}
		if err := state.RegisterRepository(store.RepositoryEntry{GitHubRepo: repo, DataDir: dataDirEntry}); err != nil {
			t.Fatal(err)
		}
		scoped := state.ForRepository(repo)
		if err := store.Put(scoped, scoped.ConfigKey(), cfg); err != nil {
			t.Fatal(err)
		}
		if err := scoped.SaveControl(model.DefaultControl()); err != nil {
			t.Fatal(err)
		}
		task := queuedTask(cfg, "task-"+repo, "tyk/work-"+repo, "tyk/work-"+repo)
		if err := store.Put(state, store.Tasks.Key(task.ID), task); err != nil {
			t.Fatal(err)
		}
		tasks[repo] = task
	}

	fleet, err := NewFleet(state, dataDir, FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	if got := len(fleet.Apps()); got != 2 {
		t.Fatalf("fleet serves %d repositories", got)
	}
	var mu sync.Mutex
	supervised := map[string][]string{}
	for _, repo := range repos {
		app := fleet.For(repo)
		if app == nil {
			t.Fatalf("no app for %s", repo)
		}
		deferHousekeeping(app)
		app.supervise = func(_ context.Context, task model.Task) error {
			mu.Lock()
			supervised[repo] = append(supervised[repo], task.ID)
			mu.Unlock()
			task.Status = model.StatusPublished
			return store.Put(state, store.Tasks.Key(task.ID), task)
		}
		if err := control(app, model.ControlCycle); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fleet.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		published := 0
		for _, task := range tasks {
			saved, err := store.Get(state, store.Tasks.Key(task.ID))
			if err == nil && saved != nil && saved.Status == model.StatusPublished {
				published++
			}
		}
		if published == len(tasks) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("fleet published %d of %d repositories' tasks", published, len(tasks))
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, repo := range repos {
		got := supervised[repo]
		if len(got) != 1 || got[0] != tasks[repo].ID {
			t.Fatalf("%s supervised %v, want only its own task %s", repo, got, tasks[repo].ID)
		}
	}
}

func TestPrimaryMeasurementExcludesNestedRepositories(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	for i, repo := range []string{"fixture/one", "fixture/two"} {
		entry := "repos/" + store.RepositorySlug(repo)
		if i == 0 {
			entry = "."
		}
		if err := state.RegisterRepository(store.RepositoryEntry{GitHubRepo: repo, DataDir: entry}); err != nil {
			t.Fatal(err)
		}
	}
	fleet, err := NewFleet(state, dataDir, FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	own := filepath.Join(dataDir, "tasks", "0b8f1c1e-2a3b-4c5d-8e9f-0123456789ab")
	nested := filepath.Join(dataDir, "repos", store.RepositorySlug("fixture/two"), "tasks", "0b8f1c1e-2a3b-4c5d-8e9f-0123456789ab")
	for dir, size := range map[string]int{own: 100, nested: 900} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "blob"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	measured, err := fleet.Primary().measure(context.Background(), filepath.Join("tasks", "0b8f1c1e-2a3b-4c5d-8e9f-0123456789ab"))
	if err != nil {
		t.Fatal(err)
	}
	if measured != 100 {
		t.Fatalf("primary measured %d bytes, want only its own 100 (nested repository excluded)", measured)
	}
	other, err := fleet.For("fixture/two").measure(context.Background(), filepath.Join("tasks", "0b8f1c1e-2a3b-4c5d-8e9f-0123456789ab"))
	if err != nil {
		t.Fatal(err)
	}
	if other != 900 {
		t.Fatalf("nested repository measured %d bytes, want its own 900", other)
	}
}

type fleetPostureBackend struct {
	sandbox.Host
	info wire.BrokerInfo
}

func (b fleetPostureBackend) Mode() sandbox.Mode { return sandbox.ModeDocker }
func (b fleetPostureBackend) Info(context.Context) (wire.BrokerInfo, error) {
	return b.info, nil
}
func (b fleetPostureBackend) RefreshInfo(ctx context.Context) (wire.BrokerInfo, error) {
	return b.Info(ctx)
}

func TestFleetKeepsStorageAndSandboxProofsPerRepository(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	info, deployment := postureFixture()
	pins := map[string]Deployment{}
	for _, repo := range []string{"fixture/one", "fixture/two"} {
		d := deployment
		d.GitHubRepo, d.Repository = repo, t.TempDir()
		pins[repo] = d
	}
	fleet, err := NewFleet(state, t.TempDir(), FleetOptions{
		Backend: fleetPostureBackend{info: info}, Deployments: pins, PrimaryRepository: "fixture/one",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	for i, app := range fleet.Apps() {
		dir := filepath.Join(app.dataDir, "tasks", "retained")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, (i+1)*100), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, app := range fleet.Apps() {
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if err := app.measureStorage(cfg); err != nil {
			t.Fatal(err)
		}
		proof := SandboxSelfTest{At: app.Repository(), ImageID: info.ImageID, Runtime: info.Runtime, Passed: true}
		if err := proof.bindPosture(info, info, app.deployment); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(app.store, app.selfTestKey(), proof); err != nil {
			t.Fatal(err)
		}
	}
	for i, app := range fleet.Apps() {
		view, err := app.StateView()
		if err != nil {
			t.Fatal(err)
		}
		want := uint64((i + 1) * 100)
		if view.Storage == nil || view.Storage.TaskBytes != want || view.Storage.ApplicationBytes != want {
			t.Fatalf("%s storage includes another repository: %+v", app.Repository(), view.Storage)
		}
		if proof := app.SandboxPosture().SelfTest; proof == nil || proof.At != app.Repository() {
			t.Fatalf("%s lost its containment proof: %+v", app.Repository(), proof)
		}
	}
	if fleet.Primary().storageKey() != store.Setting[storageUsage]("storage") || fleet.Primary().selfTestKey() != store.Setting[SandboxSelfTest]("sandbox_self_test") {
		t.Fatal("primary repository lost its legacy snapshot keys")
	}
}

func TestFleetAttributesManagedCheckoutsToTheirRepository(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	pins := map[string]Deployment{}
	for i, repo := range []string{"fixture/one", "fixture/two"} {
		checkout := filepath.Join(dataDir, "checkout")
		if i > 0 {
			checkout = filepath.Join(dataDir, "checkouts", store.RepositorySlug(repo))
		}
		if err := os.MkdirAll(checkout, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout, "data"), make([]byte, (i+1)*100), 0o600); err != nil {
			t.Fatal(err)
		}
		pins[repo] = Deployment{GitHubRepo: repo, Repository: checkout}
	}
	fleet, err := NewFleet(state, dataDir, FleetOptions{Deployments: pins, PrimaryRepository: "fixture/one"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	for i, app := range fleet.Apps() {
		want := uint64((i + 1) * 100)
		if size, err := app.measure(context.Background(), "tasks/new"); err != nil || size != want {
			t.Fatalf("%s checkout admission size = %d, %v; want %d", app.Repository(), size, err, want)
		}
		cfg, err := app.Config()
		if err != nil {
			t.Fatal(err)
		}
		if err := app.measureStorage(cfg); err != nil {
			t.Fatal(err)
		}
		stored, err := store.Get(app.store, app.storageKey())
		if err != nil || stored == nil || stored.ApplicationBytes != want {
			t.Fatalf("%s checkout snapshot = %+v, %v", app.Repository(), stored, err)
		}
	}
}

func TestFleetRefusesRemovedDeploymentPin(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	for _, repo := range []string{"acme/one", "acme/two"} {
		if err := state.RegisterRepository(store.RepositoryEntry{GitHubRepo: repo, DataDir: store.RepositorySlug(repo)}); err != nil {
			t.Fatal(err)
		}
	}
	fleet, err := NewFleet(state, t.TempDir(), FleetOptions{Deployments: map[string]Deployment{
		"acme/one": {GitHubRepo: "acme/one", Repository: t.TempDir()},
	}})
	if fleet != nil {
		fleet.Shutdown()
		t.Fatal("served a repository whose deployment pin was removed")
	}
	if err == nil || !strings.Contains(err.Error(), "acme/two") {
		t.Fatalf("removed pin error = %v", err)
	}
}

func TestFleetPreservesExplicitPrimary(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	fleet, err := NewFleet(state, dataDir, FleetOptions{
		PrimaryRepository: "acme/Z",
		Deployments: map[string]Deployment{
			"acme/a": {GitHubRepo: "acme/a", Repository: t.TempDir()},
			"acme/z": {GitHubRepo: "acme/z", Repository: t.TempDir()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	primary := fleet.Primary()
	if primary.Repository() != "acme/z" || primary.dataDir != dataDir || primary.store.ConfigKey() != store.ConfigKey || primary.store.ControlKey() != store.ControlKey {
		t.Fatalf("explicit primary lost its legacy identities: %+v", fleet.Entries())
	}
}

func TestFleetDrainsWorkersBeforePublishingRecoveredApps(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	fleet, err := NewFleet(state, dataDir, FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	retired := fleet.Primary()
	cfg := testConfig(t.TempDir())
	if err := store.Put(state, store.ConfigKey, cfg); err != nil {
		t.Fatal(err)
	}
	task := queuedTask(cfg, model.ID(), "feature", "tyk/work")
	task.Status = model.StatusExecuting
	task.ExecutionSession = new("session")
	task.Workspace = filepath.Join(dataDir, "tasks", task.ID, "work")
	if err := os.MkdirAll(filepath.Join(task.Workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(state, store.Tasks.Key(task.ID), task); err != nil {
		t.Fatal(err)
	}
	// A cancelled worker still owns the task until it has finished draining.
	release := make(chan struct{})
	defer close(release)
	retired.wg.Add(1)
	go func() { defer retired.wg.Done(); <-release }()
	registered := make(chan error, 1)
	go func() {
		_, err := fleet.AddRepository(store.RepositoryEntry{GitHubRepo: "fixture/second", DataDir: "repos/second"})
		registered <- err
	}()
	select {
	case <-retired.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("registration never started draining the old App")
	}
	visible := make(chan []*App, 1)
	go func() { visible <- fleet.Apps() }()
	select {
	case apps := <-visible:
		for _, app := range apps {
			if app != retired {
				t.Fatal("replacement became schedulable before the old worker joined")
			}
		}
	case <-time.After(50 * time.Millisecond):
		// Readers can also wait behind the registration barrier.
	}
	if got := loadTask(t, state, task.ID); got.Status != model.StatusExecuting {
		t.Fatalf("task recovered before its worker joined: %s", got.Status)
	}
	release <- struct{}{}
	select {
	case err := <-registered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registration did not finish after the worker joined")
	}
	if got := loadTask(t, state, task.ID); got.Status != model.StatusQueued {
		t.Fatalf("replacement did not recover the interrupted task: %s", got.Status)
	}
}

func TestFleetFailedInitializationKeepsPrimaryActive(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	fleet, err := NewFleet(state, dataDir, FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	primary := fleet.Primary()
	if err := store.Put(state, store.ConfigKey, testConfig(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	entry := store.RepositoryEntry{GitHubRepo: "fixture/second", DataDir: "repos/second"}
	blocked := filepath.Join(dataDir, entry.DataDir)
	if err := os.MkdirAll(filepath.Dir(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.AddRepository(entry); err == nil {
		t.Fatal("repository with a blocked data directory was accepted")
	}
	if fleet.Primary() != primary || primary.ctx.Err() != nil {
		t.Fatal("failed repository initialization retired the live primary")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.AddRepository(entry); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Apps()) != 2 || fleet.Primary().ctx.Err() != nil {
		t.Fatal("registration could not recover after fixing the data directory")
	}
}

func TestFleetFailedReplacementRecoveryStopsFleet(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	dataDir := t.TempDir()
	fleet, err := NewFleet(state, dataDir, FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	if err := store.Put(state, store.ConfigKey, testConfig(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	entry := store.RepositoryEntry{GitHubRepo: "fixture/second", DataDir: "repos/second"}
	if err := os.MkdirAll(filepath.Join(dataDir, entry.DataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dataDir, entry.DataDir, scratchDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.AddRepository(entry); err == nil {
		t.Fatal("repository with unsafe scratch storage passed recovery")
	}
	if fleet.Context().Err() == nil {
		t.Fatal("failed replacement recovery left a stopped primary behind a live fleet")
	}
}

func TestFleetCancelsAddedAppsBeforeDrainingPrimary(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	if err := state.RegisterRepository(store.RepositoryEntry{GitHubRepo: "fixture/one", DataDir: "."}); err != nil {
		t.Fatal(err)
	}
	fleet, err := NewFleet(state, t.TempDir(), FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	added, err := fleet.AddRepository(store.RepositoryEntry{GitHubRepo: "fixture/two", DataDir: "repos/two"})
	if err != nil {
		t.Fatal(err)
	}
	primary := fleet.Primary()
	primary.wg.Add(1)
	release := sync.OnceFunc(primary.wg.Done)
	defer release()
	done := make(chan struct{})
	go func() { fleet.Shutdown(); close(done) }()
	select {
	case <-added.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("added repository stayed live while the primary was draining")
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fleet did not finish draining")
	}
}
