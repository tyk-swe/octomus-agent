package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestRepeatedLifecycleLeavesNoLeaks(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("requires /proc")
	}
	fds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	lifecycle := func(t *testing.T, iteration int) {
		dir := t.TempDir()
		state, err := store.Open(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := testConfig(filepath.Join(dir, "checkout"))
		control := model.DefaultControl()
		control.SetMode(model.OperatingModeContinuous)
		control.NextCycleAt = time.Now().Unix() + 3600
		saveSettings(t, state, cfg, control)
		started := make(chan string, 1)
		app := New(state, dir, WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, task model.Task) error {
			started <- task.ID
			<-ctx.Done()
			return ctx.Err()
		})))
		task := queuedTask(cfg, "leak-check", "octomus/existing", "octomus/existing")
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- app.Run(ctx) }()
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Fatal("the queued task never dispatched")
		}
		if iteration%2 == 0 {
			cancel()
		} else {
			app.Shutdown()
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("iteration %d: run returned %v", iteration, err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("iteration %d: the service never shut down", iteration)
		}
		cancel()
		if !app.Drained() {
			t.Fatalf("iteration %d: shutdown returned before workers finished", iteration)
		}
		if err := state.Close(); err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
	}
	for i := 0; i < 3; i++ {
		lifecycle(t, i)
	}
	runtime.GC()
	beforeFDs, beforeG := fds(), runtime.NumGoroutine()
	for i := 3; i < 18; i++ {
		lifecycle(t, i)
	}
	var extraFDs, extraG int
	if !testutil.WaitUntil(10*time.Second, func() bool {
		runtime.GC()
		extraFDs, extraG = fds()-beforeFDs, runtime.NumGoroutine()-beforeG
		return extraFDs <= 0 && extraG <= 2
	}) {
		t.Fatalf("lifecycle leak: %+d descriptors and %+d goroutines after 15 cycles (baseline %d/%d)",
			extraFDs, extraG, beforeFDs, beforeG)
	}
}

// heldHealthBackend waits for cancellation like a stalled broker request, while release
// lets a failing regression test join the scheduler without waiting for its health timeout.
type heldHealthBackend struct {
	sandbox.Host
	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	lateSuccess bool
}

func (b *heldHealthBackend) Healthy(ctx context.Context) error {
	b.once.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		if b.lateSuccess {
			return nil
		}
		return ctx.Err()
	case <-b.release:
		return nil
	}
}

func TestShutdownCancelsInFlightSchedulerHealthCheck(t *testing.T) {
	t.Parallel()
	for _, stop := range []string{"shutdown", "run context"} {
		t.Run(stop, func(t *testing.T) {
			state := testStore(t)
			cfg := testConfig(t.TempDir())
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			control.NextCycleAt = time.Now().Unix() + 3600
			saveSettings(t, state, cfg, control)
			backend := &heldHealthBackend{started: make(chan struct{}), release: make(chan struct{})}
			app := New(state, t.TempDir(), WithSandbox(backend))
			t.Cleanup(app.Shutdown)
			deferHousekeeping(app)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- app.Run(ctx) }()
			select {
			case <-backend.started:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("scheduler did not begin its health check")
			}
			stopped := make(chan struct{})
			if stop == "shutdown" {
				go func() { app.Shutdown(); close(stopped) }()
			} else {
				cancel()
				close(stopped)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(time.Second):
				t.Error("shutdown did not cancel the in-flight scheduler health check")
				close(backend.release)
				<-done
			}
			<-stopped
			live, err := app.Control()
			if err != nil || live.Mode != model.OperatingModeContinuous || live.Error != nil {
				t.Fatalf("normal shutdown persisted a health failure: %+v, %v", live, err)
			}
			system := "system"
			events, err := state.Events(&system)
			if err != nil || len(events) != 0 {
				t.Fatalf("normal shutdown recorded a service error: %+v, %v", events, err)
			}
			if !app.Drained() {
				t.Fatal("shutdown returned before scheduler work drained")
			}
		})
	}
}

func TestRunWithCancelledContextDoesNotDispatch(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = time.Now().Unix() + 3600
	saveSettings(t, state, cfg, control)
	task := queuedTask(cfg, "cancelled-run", "tyk/existing", "tyk/existing")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, _ model.Task) error {
		started <- struct{}{}
		return ctx.Err()
	})))
	t.Cleanup(app.Shutdown)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(started) != 0 {
		t.Error("a cancelled service context dispatched a task")
	}
	if saved := loadTask(t, state, task.ID); saved.Status != model.StatusQueued {
		t.Errorf("cancelled run changed queued task to %s", saved.Status)
	}
	if !app.runtime.lastRetention.IsZero() || !app.runtime.lastObserve.IsZero() || !app.runtime.lastPrAttempt.IsZero() {
		t.Error("a cancelled service context started background maintenance")
	}
	if !app.Drained() || app.Context().Err() != context.Canceled {
		t.Fatal("cancelled run did not shut down cleanly")
	}
}

func TestCancelledHealthCheckDoesNotDispatchLateSuccess(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = time.Now().Unix() + 3600
	saveSettings(t, state, cfg, control)
	task := queuedTask(cfg, "late-health", "tyk/existing", "tyk/existing")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	backend := &heldHealthBackend{started: make(chan struct{}), release: make(chan struct{}), lateSuccess: true}
	started := make(chan struct{}, 1)
	app := New(state, t.TempDir(), WithSandbox(backend), WithTaskRunner(TaskRunnerFunc(func(ctx context.Context, _ model.Task) error {
		started <- struct{}{}
		return ctx.Err()
	})))
	t.Cleanup(app.Shutdown)
	deferHousekeeping(app)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	select {
	case <-backend.started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not begin its health check")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled scheduler did not stop")
	}
	if len(started) != 0 {
		t.Error("a late successful health check dispatched work after cancellation")
	}
	if saved := loadTask(t, state, task.ID); saved.Status != model.StatusQueued {
		t.Errorf("cancelled health check changed queued task to %s", saved.Status)
	}
}
