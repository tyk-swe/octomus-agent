package engine

// Concurrent task initialization against the shared trusted checkout.

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// TestParallelTaskInitializationSharesCheckout runs two independent tasks on
// distinct branches through real gitops.Fetch on the same trusted checkout at
// once. The shared fetch must serialize without starving either task: both
// publish from their own workspace rather than one being blocked by local ref
// contention.
func TestParallelTaskInitializationSharesCheckout(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	cfg := f.cfg.Clone()
	cfg.ExecutionConcurrency = 2
	saveSettings(t, f.state, cfg, model.DefaultControl())
	review := maintenanceReview(true, false)
	first := maintenanceTask(t, f, review, writeFile("feature.txt", "fixed\n"))
	second := maintenanceTask(t, f, review, writeFile("feature.txt", "fixed\n"))
	if first.Branch == second.Branch {
		t.Fatalf("fixture tasks share a branch: %s", first.Branch)
	}
	app := f.newApp(t)
	driveTask(t, f, app, first.ID)
	for _, task := range []model.Task{first, second} {
		saved := loadTask(t, f.state, task.ID)
		if saved.Status != model.StatusPublished {
			t.Fatalf("task %s = %s (%v); want published", saved.ID, saved.Status, saved.Error)
		}
	}
	if firstSaved, secondSaved := loadTask(t, f.state, first.ID), loadTask(t, f.state, second.ID); firstSaved.Workspace == secondSaved.Workspace {
		t.Fatal("concurrent tasks shared a workspace")
	}
}
