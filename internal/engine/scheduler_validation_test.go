package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Dispatch-time revalidation blocks every queued member of a plan whose
// writers to one PR are unordered, and the same Tick dispatches none of them
// although execution slots are free. Active members are left alone, and the
// validated-cycle cache forgets the cycle once none of its members is visible.
func TestTickBlocksInvalidQueuedPlan(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.ExecutionConcurrency = 4
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = int64(^uint64(0) >> 1)
	saveSettings(t, state, cfg, control)
	first := queuedTask(cfg, "first", "octomus/existing", "octomus/existing")
	second := queuedTask(cfg, "second", "octomus/existing", "octomus/existing")
	active := queuedTask(cfg, "active", "octomus/other", "octomus/other")
	active.Status = model.StatusExecuting
	for _, task := range []model.Task{first, second, active} {
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	var dispatched atomic.Int32
	app := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
		dispatched.Add(1)
		return nil
	})))
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()

	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		task, err := store.Get[model.Task](state, "task", id)
		if err != nil || task == nil {
			t.Fatalf("load %s: %+v, %v", id, task, err)
		}
		if task.Status != model.StatusBlocked || task.BlockedReason == nil || *task.BlockedReason != model.BlockedReasonInvalidPlan || task.Error == nil || !strings.Contains(*task.Error, "total dependency order") {
			t.Fatalf("queued member %s was not blocked as an invalid plan: status=%s reason=%v error=%v", id, task.Status, task.BlockedReason, task.Error)
		}
	}
	running, err := store.Get[model.Task](state, "task", active.ID)
	if err != nil || running == nil || running.Status != model.StatusExecuting || running.BlockedReason != nil || running.Error != nil {
		t.Fatalf("revalidation touched the active member: %+v, %v", running, err)
	}
	app.runtimeMu.Lock()
	_, cached := app.runtime.checkedCycles[first.CycleID]
	app.runtimeMu.Unlock()
	if !cached {
		t.Fatal("validated cycle was not cached")
	}

	// Once no member of the cycle is visible to scheduling, the cache drops it.
	running.Status = model.StatusPublished
	if err := state.Put("task", running.ID, *running); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	app.runtimeMu.Lock()
	_, cached = app.runtime.checkedCycles[first.CycleID]
	app.runtimeMu.Unlock()
	if cached {
		t.Fatal("the cache kept a cycle with no visible member")
	}
	app.Shutdown() // Waits for any task a Tick started.
	if count := dispatched.Load(); count != 0 {
		t.Fatalf("Ticks dispatched %d members of an invalid plan", count)
	}
}
