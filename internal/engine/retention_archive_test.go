package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestRetentionRechecksArchivalTimeBeforeDiscarding(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"task", "cycle"} {
		t.Run(kind, func(t *testing.T) {
			state := testStore(t)
			dataDir := t.TempDir()
			cfg := testConfig(t.TempDir())
			cfg.RetainCompletedDays = 1
			saveSettings(t, state, cfg, model.DefaultControl())
			var held, kept string
			var archive func(*App) error
			var lifecycle func() model.WorkspaceLifecycle
			if kind == "task" {
				first := discardableTask(t, cfg, dataDir, "held-task")
				second := discardableTask(t, cfg, dataDir, "newly-archived-task")
				second.Status = model.StatusPublished
				second.Lifecycle.ArchivedAt = nil
				for _, task := range []model.Task{first, second} {
					if err := state.Put("task", task.ID, task); err != nil {
						t.Fatal(err)
					}
				}
				held, kept = filepath.Dir(first.Workspace), second.Workspace
				archive = func(app *App) error { return app.TaskAction(context.Background(), second.ID, "archive") }
				lifecycle = func() model.WorkspaceLifecycle {
					saved, err := store.Get[model.Task](state, "task", second.ID)
					if err != nil || saved == nil {
						t.Fatalf("reload task: %+v, %v", saved, err)
					}
					return saved.Lifecycle
				}
			} else {
				first := discardableCycle(t, dataDir)
				second := discardableCycle(t, dataDir)
				second.Lifecycle.ArchivedAt = nil
				for _, cycle := range []model.Cycle{first, second} {
					if err := state.Put("cycle", cycle.ID, cycle); err != nil {
						t.Fatal(err)
					}
				}
				held, kept = filepath.Join(dataDir, "cycles", first.ID), filepath.Join(dataDir, "cycles", second.ID)
				archive = func(app *App) error { return app.CycleAction(second.ID, "archive") }
				lifecycle = func() model.WorkspaceLifecycle {
					saved, err := store.Get[model.Cycle](state, "cycle", second.ID)
					if err != nil || saved == nil {
						t.Fatalf("reload cycle: %+v, %v", saved, err)
					}
					return saved.Lifecycle
				}
			}
			barrier := newRemovalBarrier(t, held)
			app := New(state, dataDir, WithWorkspaceRemoval(barrier.remove))
			t.Cleanup(app.Shutdown)
			defer barrier.Release()
			done := make(chan error, 1)
			go func() { done <- app.retention(cfg) }()
			barrier.wait(t)
			if err := completesDuring(t, "operator archive", func() error { return archive(app) }); err != nil {
				t.Fatalf("operator archive during retention: %v", err)
			}
			barrier.Release()
			if err := <-done; err != nil {
				t.Fatalf("retention: %v", err)
			}
			if saved := lifecycle(); saved.ArchivedAt == nil || saved.DiscardedAt != nil {
				t.Errorf("new archive must restart retention: %+v", saved)
			}
			if _, err := os.Stat(kept); err != nil {
				t.Errorf("newly archived workspace was not retained: %v", err)
			}
			if _, err := os.Stat(held); !os.IsNotExist(err) {
				t.Errorf("expired workspace was not removed: %v", err)
			}
		})
	}
}
