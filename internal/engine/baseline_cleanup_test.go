package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func refusedBaselineRoot(t *testing.T, app *App, id string) string {
	t.Helper()
	parent := filepath.Join(app.dataDir, "baselines")
	must0(t, os.MkdirAll(parent, 0700))
	path := filepath.Join(parent, id)
	must0(t, os.Symlink(t.TempDir(), path))
	return path
}

func TestReviewBaselineCleanupErrorReachesCaller(t *testing.T) {
	t.Parallel()
	app, cfg := baselineApp(t)
	check := makeCheck(cfg, model.BaselineStatusPassed)
	must0(t, app.Store.Put("baseline", check.ID, check))
	path := refusedBaselineRoot(t, app, check.ID)
	err := app.removeBaselineWorkspace(&check)
	if err == nil || !strings.Contains(err.Error(), "Cleanup refuses symlink paths") {
		t.Fatalf("cleanup failure did not reach the caller: %v", err)
	}
	saved, err := store.Get[model.BaselineCheck](app.Store, "baseline", check.ID)
	must0(t, err)
	if saved.CleanupError == nil || saved.WorkspaceRemoved {
		t.Fatalf("cleanup failure was not retained: %+v", saved)
	}

	// Resolving the filesystem problem permits a later retry and clears the
	// failure without changing the baseline's verification result.
	must0(t, os.Remove(path))
	must0(t, os.Mkdir(path, 0700))
	must0(t, app.removeBaselineWorkspace(saved))
	saved, err = store.Get[model.BaselineCheck](app.Store, "baseline", check.ID)
	must0(t, err)
	if !saved.WorkspaceRemoved || saved.CleanupError != nil || saved.Status != model.BaselineStatusPassed {
		t.Fatalf("successful cleanup did not clear only its own evidence: %+v", saved)
	}

	missing := makeCheck(cfg, model.BaselineStatusPassed)
	refusedBaselineRoot(t, app, missing.ID)
	if err := app.removeBaselineWorkspace(&missing); err == nil {
		t.Fatal("a missing database record hid the filesystem failure")
	}
	if !app.claimCleanup(cleanupBaseline, missing.ID) {
		t.Fatal("cannot claim baseline cleanup")
	}
	defer app.releaseCleanup(cleanupBaseline, missing.ID)
	if err := app.removeBaselineWorkspace(&missing); err != nil {
		t.Fatalf("another cleanup's claim must remain a no-op: %v", err)
	}
}

func TestBaselineRetentionReportsOldCleanupFailure(t *testing.T) {
	t.Parallel()
	app, cfg := baselineApp(t)
	old := makeCheck(cfg, model.BaselineStatusPassed)
	must0(t, app.Store.Put("baseline", old.ID, old))
	path := refusedBaselineRoot(t, app, old.ID)
	latest := makeCheck(cfg, model.BaselineStatusPassed)
	latest.WorkspaceRemoved = true
	must0(t, app.Store.Put("baseline", latest.ID, latest))
	must0(t, app.Store.Put("settings", "baseline_latest", latest.ID))
	count := func(want int) {
		t.Helper()
		events, err := app.Store.Events(&old.ID)
		must0(t, err)
		got := 0
		for _, event := range events {
			if event.Kind == "cleanup_error" {
				got++
			}
		}
		if got != want {
			t.Fatalf("old baseline has %d cleanup events, want %d", got, want)
		}
	}
	must0(t, app.retention(cfg))
	count(1)
	must0(t, app.retention(cfg))
	count(1)
	key := cleanupKey{kind: cleanupBaseline, id: old.ID}
	app.runtimeMu.Lock()
	previous := app.runtime.cleanupReports[key]
	previous.at = time.Now().Add(-cleanupReportInterval - time.Second)
	app.runtime.cleanupReports[key] = previous
	app.runtimeMu.Unlock()
	must0(t, app.retention(cfg))
	count(2)

	must0(t, os.Remove(path))
	must0(t, app.retention(cfg))
	app.runtimeMu.Lock()
	_, reporting := app.runtime.cleanupReports[key]
	app.runtimeMu.Unlock()
	if reporting {
		t.Fatal("successful cleanup left the retry failure report active")
	}
	saved, err := app.Store.LatestBaseline()
	must0(t, err)
	if saved == nil || saved.ID != latest.ID {
		t.Fatal("cleanup changed which baseline is latest")
	}
	count(2)
}
