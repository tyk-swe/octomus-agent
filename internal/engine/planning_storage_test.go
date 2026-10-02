package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// Hold an actual trusted git command, with one entry marker per process. The file barrier only releases on demand;
// no ordering assertion depends on the polling interval.
func holdPlanningGit(t *testing.T, f *scriptedFixture, command string) (entered string, release func()) {
	t.Helper()
	hold := filepath.Join(f.root, "hold-planning-git")
	entered = filepath.Join(f.root, "planning-git-entered")
	if err := os.Mkdir(entered, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
for arg; do
	if [ "$arg" = %[1]q ] && [ -e %[2]q ]; then
		: > %[3]q/$$
		while [ -e %[2]q ]; do sleep 0.01; done
		break
	fi
done
exec /bin/sh %[4]q "$@"
`, command, hold, entered, filepath.Join(repositoryRoot(t), "tests", "fixtures", "git.sh"))
	if err := os.WriteFile(filepath.Join(f.root, "bin", "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(func() { _ = os.Remove(hold) })
	t.Cleanup(release)
	return entered, release
}

func waitPlanningGit(t *testing.T, entered string, count int) {
	t.Helper()
	if !testutil.WaitUntil(10*time.Second, func() bool {
		entries, err := os.ReadDir(entered)
		return err == nil && len(entries) == count
	}) {
		t.Fatalf("trusted git commands did not overlap: want %d entry markers", count)
	}
}

func waitPlanningStorage[T any](t *testing.T, done <-chan T) T {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("planning storage operation did not finish")
		var zero T
		return zero
	}
}

func startPlanningStorageRole(t *testing.T, f *scriptedFixture, app *App, cycleID, label string) <-chan roleOutcome {
	t.Helper()
	revision := git(t, f.repo, "rev-parse", "HEAD")
	done := make(chan roleOutcome, 1)
	app.wg.Go(func() {
		done <- app.role(app.ctx, f.cfg, cycleID, revision, label, "discovery", "Inspect the source snapshot", nil)
	})
	return done
}

func assertPlanningStorageReader(t *testing.T, app *App) {
	t.Helper()
	if app.planningStorage.TryLock() {
		app.planningStorage.Unlock()
		t.Fatal("trusted planning filesystem work did not exclude an admission scan")
	}
	if !app.planningStorage.TryRLock() {
		t.Fatal("trusted planning filesystem work exclusively locked out independent setup")
	}
	app.planningStorage.RUnlock()
}

// The caller keeps a reader held. TryRLock can fail only after a writer queues, so no sleep is needed to prove the
// scan has reached its lock wait before cancellation or assertions about reservations.
func waitPlanningStorageWriter(t *testing.T, app *App) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for app.planningStorage.TryRLock() {
		app.planningStorage.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("admission scan did not queue behind trusted planning filesystem work")
		}
		runtime.Gosched()
	}
}

func startPlanningStorageAdmission(app *App, f *scriptedFixture, cycleID string) <-chan error {
	done := make(chan error, 1)
	app.wg.Go(func() { done <- app.admit(app.ctx, cycleID, nil, "sibling", f.routes.Discovery) })
	return done
}

func TestPlanningGitWorkExcludesAdmissionScans(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"clone", "status"} {
		t.Run(command, func(t *testing.T) {
			f := newScriptedPlanningFixture(t)
			app := f.pausedApp(t)
			cycle := groundingCycle(t, f, model.CycleModeAudit)
			entered, release := holdPlanningGit(t, f, command)
			f.script.Answer(f.routes.Discovery, "Inspected")
			role := startPlanningStorageRole(t, f, app, cycle.ID, "discovery-0")
			waitPlanningGit(t, entered, 1)
			assertPlanningStorageReader(t, app)
			admission := startPlanningStorageAdmission(app, f, cycle.ID)
			waitPlanningStorageWriter(t, app)
			assertAdmissions(t, f.state, 1, "the scan waits for the actual git operation")
			release()
			if err := waitPlanningStorage(t, admission); err != nil {
				t.Fatal(err)
			}
			if outcome := waitPlanningStorage(t, role); outcome.err != nil {
				t.Fatal(outcome.err)
			}
			assertAdmissions(t, f.state, 2, "the sibling admits after trusted git finishes")
		})
	}
}

func TestPlanningCleanupExcludesAdmissionScans(t *testing.T) {
	t.Parallel()
	f := newScriptedPlanningFixture(t)
	app := f.pausedApp(t)
	cycle := groundingCycle(t, f, model.CycleModeAudit)
	entered, released := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	t.Cleanup(release)
	remove := app.removeDir
	app.removeDir = func(root, path string) error {
		close(entered)
		<-released
		return remove(root, path)
	}
	f.script.Answer(f.routes.Discovery, "Inspected")
	role := startPlanningStorageRole(t, f, app, cycle.ID, "discovery-0")
	waitPlanningStorage(t, entered)
	assertPlanningStorageReader(t, app)
	admission := startPlanningStorageAdmission(app, f, cycle.ID)
	waitPlanningStorageWriter(t, app)
	assertAdmissions(t, f.state, 1, "owned cleanup is still removing files")
	release()
	if err := waitPlanningStorage(t, admission); err != nil {
		t.Fatal(err)
	}
	if outcome := waitPlanningStorage(t, role); outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if _, err := os.Stat(filepath.Dir(roleWorkspace(f, cycle.ID, "discovery-0"))); !os.IsNotExist(err) {
		t.Fatalf("successful role root remains after cleanup: %v", err)
	}
}

func TestPlanningSetupsRemainConcurrent(t *testing.T) {
	t.Parallel()
	f := newScriptedPlanningFixture(t)
	app := f.pausedApp(t)
	cycle := groundingCycle(t, f, model.CycleModeAudit)
	// Admit both before the setup rendezvous: a queued RWMutex writer correctly blocks later readers.
	for _, label := range []string{"discovery-0", "discovery-1"} {
		if err := app.admit(app.ctx, cycle.ID, nil, label, f.routes.Discovery); err != nil {
			t.Fatal(err)
		}
	}
	entered, release := holdPlanningGit(t, f, "clone")
	revision := git(t, f.repo, "rev-parse", "HEAD")
	done := make(chan error, 2)
	for _, label := range []string{"discovery-0", "discovery-1"} {
		app.wg.Go(func() {
			done <- app.withPlanningWorkspace(app.ctx, func() error {
				return gitops.CloneAt(app.ctx, f.cfg, roleWorkspace(f, cycle.ID, label), revision)
			})
		})
	}
	waitPlanningGit(t, entered, 2)
	release()
	for range 2 {
		if err := waitPlanningStorage(t, done); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanningTurnsDoNotBlockSiblingAdmissions(t *testing.T) {
	t.Parallel()
	f := newScriptedPlanningFixture(t)
	app := f.pausedApp(t)
	cycle := groundingCycle(t, f, model.CycleModeAudit)
	first, second := runnertest.NewGate(), runnertest.NewGate()
	t.Cleanup(first.Release)
	t.Cleanup(second.Release)
	f.script.Queue(f.routes.Discovery,
		runnertest.Reply{Answer: "First", Gate: first},
		runnertest.Reply{Answer: "Second", Gate: second})
	one := startPlanningStorageRole(t, f, app, cycle.ID, "discovery-0")
	waitPlanningStorage(t, first.Entered())
	two := startPlanningStorageRole(t, f, app, cycle.ID, "discovery-1")
	waitPlanningStorage(t, second.Entered())
	assertAdmissions(t, f.state, 2, "both model turns overlap")
	first.Release()
	second.Release()
	for _, done := range []<-chan roleOutcome{one, two} {
		if outcome := waitPlanningStorage(t, done); outcome.err != nil {
			t.Fatal(outcome.err)
		}
	}
}

func TestCancelledAdmissionAfterStorageWaitSpendsNothing(t *testing.T) {
	t.Parallel()
	f := newScriptedPlanningFixture(t)
	app := f.pausedApp(t)
	ctx, cancel := context.WithCancel(app.ctx)
	defer cancel()
	app.planningStorage.RLock()
	release := sync.OnceFunc(app.planningStorage.RUnlock)
	t.Cleanup(release)
	prepared := false
	done := make(chan error, 1)
	app.wg.Go(func() {
		_, err := app.invoke(ctx, app.runners(ctx, f.cfg, "cycle"), invocation{
			cycleID: "cycle", role: "discovery-0", route: f.routes.Discovery,
			workspace: roleWorkspace(f, "cycle", "discovery-0"), ownsClients: true,
			prepare: func() error { prepared = true; return nil },
		})
		done <- err
	})
	waitPlanningStorageWriter(t, app)
	cancel()
	release()
	if err := waitPlanningStorage(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued admission error = %v; want cancellation", err)
	}
	if prepared || len(f.script.Calls()) != 0 {
		t.Fatalf("cancelled admission prepared=%v or started a runner: %+v", prepared, f.script.Calls())
	}
	assertAdmissions(t, f.state, 0, "cancelled lock wait spends no reservation")
}

func TestCancelledPlanningWorkspaceWaitSkipsNewWork(t *testing.T) {
	t.Parallel()
	f := newScriptedPlanningFixture(t)
	app := f.pausedApp(t)
	app.planningStorage.Lock()
	release := sync.OnceFunc(app.planningStorage.Unlock)
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(app.ctx)
	defer cancel()
	worked := false
	done := make(chan error, 1)
	app.wg.Go(func() {
		done <- app.withPlanningWorkspace(ctx, func() error { worked = true; return nil })
	})
	cancel()
	release()
	if err := waitPlanningStorage(t, done); !errors.Is(err, context.Canceled) || worked {
		t.Fatalf("cancelled setup/status wait: worked=%v, error=%v", worked, err)
	}
}

func TestPlanningCleanupFinishesAfterCancelledStorageWait(t *testing.T) {
	t.Parallel()
	f := newScriptedPlanningFixture(t)
	app := f.pausedApp(t)
	root := filepath.Join(f.dataDir, "cycles", "cycle", "discovery-0")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	app.planningStorage.Lock()
	release := sync.OnceFunc(app.planningStorage.Unlock)
	t.Cleanup(release)
	done := make(chan error, 1)
	app.wg.Go(func() { done <- app.removePlanningWorkspace(root) })
	app.cancel()
	release()
	if err := waitPlanningStorage(t, done); err != nil {
		t.Fatalf("cancellation prevented already-owned cleanup: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("cancelled cleanup left its role root: %v", err)
	}
}
