// Operational views served from the indexed projections.

package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestIndexedViews(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))

	queued := task()
	queued.ID = "queued"
	active := task()
	active.ID = "active"
	active.Status = model.StatusReviewing
	active.ExecutionSession = str("exec-active")
	reserved := task()
	reserved.ID = "reserved"
	reserved.Proposal.Target = "topic/other"
	published := task()
	published.ID = "published"
	published.Status = model.StatusPublished
	published.OutputCommit = str("out00001")
	published.PRNumber = new(uint64(9))
	published.Lifecycle.ArchivedAt = str("2020-01-01T00:00:00Z")
	blocked := task()
	blocked.ID = "blocked"
	blocked.Status = model.StatusBlocked
	blocked.OutputCommit = str("out00002")
	for _, tk := range []model.Task{queued, active, reserved, published, blocked} {
		must(t, s.Put("task", tk.ID, tk))
	}

	c := cycleFor(queued)
	c.ID = "cycle-running"
	c.Lifecycle.DiscardedAt = str("2020-01-01T00:00:00Z")
	must(t, s.Put("cycle", c.ID, c))
	done := cycleFor(queued)
	done.ID = "cycle-done"
	done.Status = model.CycleCompleted
	done.CompletedAt = str(model.Now())
	done.Proposals = []model.Proposal{queued.Proposal, func() model.Proposal {
		p := queued.Proposal
		p.ID = "b"
		p.Decision = model.DecisionRejected
		return p
	}()}
	must(t, s.Put("cycle", done.ID, done))

	baseline := func(id string, status model.BaselineStatus, removed bool) model.BaselineCheck {
		return model.BaselineCheck{ID: id, Status: status, Config: config.Default(), ConfigFingerprint: "fp",
			StartedAt: model.Now(), Commands: []model.BaselineCommand{}, WorkspaceRemoved: removed}
	}
	must(t, s.Put("baseline", "base-running", baseline("base-running", model.BaselineStatusRunning, false)))
	must(t, s.Put("baseline", "base-done", baseline("base-done", model.BaselineStatusPassed, false)))
	must(t, s.Put("baseline", "base-clean", baseline("base-clean", model.BaselineStatusFailed, true)))
	must(t, s.Put("settings", "baseline_latest", "base-done"))

	scheduling, err := s.SchedulingTasks(nil)
	must(t, err)
	ids := func(tasks []model.Task) []string {
		out := []string{}
		for _, tk := range tasks {
			out = append(out, tk.ID)
		}
		return out
	}
	if got := ids(scheduling); len(got) != 3 || !slices.Contains(got, "active") || !slices.Contains(got, "queued") || !slices.Contains(got, "reserved") {
		t.Fatalf("scheduling: %v", got)
	}
	activeTasks, err := s.ActiveTasks()
	must(t, err)
	if got := ids(activeTasks); len(got) != 1 || got[0] != "active" {
		t.Fatalf("active tasks: %v", got)
	}
	forCycle, err := s.TasksForCycle("cycle")
	must(t, err)
	if len(forCycle) != 5 {
		t.Fatalf("tasks for cycle: %v", ids(forCycle))
	}
	unresolved, err := s.HasUnresolvedTasks()
	must(t, err)
	if !unresolved {
		t.Fatal("blocked work should count as unresolved")
	}

	for _, selected := range []string{"cycle-running", "missing", "all"} {
		result, err := s.HistoryPage("cycle", store.HistoryQuery{Cycle: &selected})
		must(t, err)
		want := map[string]int{"cycle-running": 1, "missing": 0, "all": 2}[selected]
		if len(result.Items) != want || result.NextCursor != nil {
			t.Fatalf("cycle summary %q: %+v", selected, result)
		}
		if selected == "cycle-running" {
			summary := decodeMap(t, result.Items[0])
			if summary["id"] != selected || summary["session_count"] != float64(0) {
				t.Fatalf("cycle summary: %v", summary)
			}
		}
	}

	running, err := s.RunningCycles("")
	must(t, err)
	if len(running) != 1 || running[0].ID != "cycle-running" {
		t.Fatalf("running cycles: %+v", running)
	}
	if others, err := s.RunningCycles("cycle-running"); err != nil || len(others) != 0 {
		t.Fatalf("running cycles except the live one: %+v, %v", others, err)
	}
	runningBaselines, err := s.RunningBaselines()
	must(t, err)
	if len(runningBaselines) != 1 || runningBaselines[0].ID != "base-running" {
		t.Fatalf("running baselines: %+v", runningBaselines)
	}
	cleanup, err := s.StaleBaselines("")
	must(t, err)
	if len(cleanup) != 1 || cleanup[0].ID != "base-done" {
		t.Fatalf("baseline cleanup: %+v", cleanup)
	}
	latest, err := s.LatestBaseline()
	must(t, err)
	if latest == nil || latest.ID != "base-done" {
		t.Fatalf("latest baseline: %+v", latest)
	}
	old, err := s.CleanupCandidates("task", "2021-01-01T00:00:00Z", "")
	must(t, err)
	if len(old) != 1 || old[0] != "published" {
		t.Fatalf("cleanup candidates: %v", old)
	}

	proposals, err := s.ProposalPage(store.HistoryQuery{})
	must(t, err)
	if len(proposals.Items) != 3 || proposals.Counts["accepted"] != 2 || proposals.Counts["rejected"] != 1 {
		t.Fatalf("proposal page: %d items, counts %v", len(proposals.Items), proposals.Counts)
	}
	rejected := "rejected"
	filtered, err := s.ProposalPage(store.HistoryQuery{Status: &rejected, Cycle: str("cycle-done")})
	must(t, err)
	if len(filtered.Items) != 1 {
		t.Fatalf("filtered proposals: %d", len(filtered.Items))
	}
	detail, err := s.ProposalDetail("cycle-done", "b")
	must(t, err)
	view := decodeMap(t, detail)
	if view["prompt"] != "Implement the documented behavior" || view["content_revision"] != float64(1) {
		t.Fatalf("proposal detail: %v", view)
	}
	page := decodeMap(t, filtered.Items[0])
	if page["cycle_id"] != "cycle-done" || page["prompt"] != "" || page["decision"] != "rejected" {
		t.Fatalf("proposal page item: %v", page)
	}
	if missing, err := s.ProposalDetail("cycle-done", "missing"); err != nil || missing != nil {
		t.Fatalf("missing proposal: %s %v", missing, err)
	}

	candidates, err := s.ReservableTasks()
	must(t, err)
	if got := ids(candidates); len(got) != 2 {
		t.Fatalf("reservation candidates: %v", got)
	}

	control := startBatch(t, s)
	if control.Batch == nil || control.Mode != model.OperatingModeRunOnce || control.Batch.Phase != model.BatchPhaseDraining {
		t.Fatalf("batch control: %+v", control)
	}
	assigned, err := s.SchedulingTasks(&control.Batch.ID)
	must(t, err)
	if got := ids(assigned); len(got) != 3 {
		t.Fatalf("run-scoped scheduling: %v", got)
	}
	pending, unresolvedCount, err := s.BatchCounts(control.Batch.ID)
	must(t, err)
	if pending != 2 || unresolvedCount != 0 {
		t.Fatalf("batch counts: pending %d unresolved %d", pending, unresolvedCount)
	}
	next := cycleFor(queued)
	next.ID = "cycle-next"
	expected := control.Clone()
	control.Batch.CycleID = str(next.ID)
	fingerprint, err := config.Default().Fingerprint()
	must(t, err)
	_, started, err := s.BeginCycle(next, control, expected, fingerprint, time.Now())
	must(t, err)
	if !started {
		t.Fatal("begin cycle refused an affordable cycle under the saved control")
	}
	reloaded, err := store.Get[model.Control](s, "settings", "control")
	must(t, err)
	if reloaded == nil || reloaded.Batch == nil || reloaded.Batch.CycleID == nil || *reloaded.Batch.CycleID != "cycle-next" {
		t.Fatalf("begin cycle control: %+v", reloaded)
	}

	must(t, s.Event("queued", "status", "Queued"))
	must(t, s.Event("active", "status", "Reviewing"))
	scoped, err := s.Events(str("active"))
	must(t, err)
	if len(scoped) != 1 || scoped[0].Message != "Reviewing" {
		t.Fatalf("scoped events: %+v", scoped)
	}
	must(t, s.PruneEvents(1))
	all, err := s.Events(nil)
	must(t, err)
	if len(all) != 1 {
		t.Fatalf("pruned events: %+v", all)
	}
}
