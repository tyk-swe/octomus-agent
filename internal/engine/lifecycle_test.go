package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
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
