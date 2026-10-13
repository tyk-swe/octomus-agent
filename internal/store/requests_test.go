// Operator request records: the per-repository pending limit, listing order, guarded updates and atomic plan settlement.

package store_test

import (
	"errors"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func pendingRequest(repo, title string) model.OperatorRequest {
	now := model.Now()
	return model.OperatorRequest{
		ID: model.ID(), Repository: repo, Title: title, Description: "Details for " + title,
		Target: "main", Status: model.RequestPending, CreatedAt: now, UpdatedAt: now,
	}
}

func TestRequestLimitAndOrdering(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	for i := 0; i < model.RequestPendingLimit; i++ {
		must(t, s.CreateRequest(pendingRequest("fixture/project", "work"), model.RequestPendingLimit))
	}
	if err := s.CreateRequest(pendingRequest("Fixture/Project", "over"), model.RequestPendingLimit); !errors.Is(err, store.ErrRequestLimit) {
		t.Fatalf("case-insensitive repository count did not apply the limit: %v", err)
	}
	must(t, s.CreateRequest(pendingRequest("another/repo", "elsewhere"), model.RequestPendingLimit))
	pending, err := s.PendingRequests("FIXTURE/PROJECT", model.RequestPendingLimit)
	must(t, err)
	if len(pending) != model.RequestPendingLimit {
		t.Fatalf("pending: %d", len(pending))
	}
	for i := 1; i < len(pending); i++ {
		if pending[i-1].CreatedAt > pending[i].CreatedAt {
			t.Fatal("pending requests are not oldest first")
		}
	}
	count, err := s.CountPendingRequests("fixture/project")
	must(t, err)
	if count != model.RequestPendingLimit {
		t.Fatalf("count: %d", count)
	}
	recent, err := s.RecentRequests("fixture/project", 50)
	must(t, err)
	if len(recent) != 0 {
		t.Fatalf("recent: %+v", recent)
	}
}

func TestUpdateRequestGuardsStaleExpected(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	original := pendingRequest("fixture/project", "withdraw me")
	must(t, s.CreateRequest(original, model.RequestPendingLimit))
	next := original.Clone()
	next.Status = model.RequestWithdrawn
	stale := original.Clone()
	stale.Title = "drifted"
	if err := s.UpdateRequest(store.RequestUpdate{Expected: stale, Next: next}); !errors.Is(err, store.ErrRequestChanged) {
		t.Fatalf("stale expected applied: %v", err)
	}
	if err := s.UpdateRequest(store.RequestUpdate{Expected: pendingRequest("fixture/project", "ghost"), Next: next}); !errors.Is(err, store.ErrRequestChanged) {
		t.Fatalf("missing request updated: %v", err)
	}
	must(t, s.UpdateRequest(store.RequestUpdate{Expected: original, Next: next}))
	saved, err := store.Get(s, store.Requests.Key(original.ID))
	must(t, err)
	if saved == nil || saved.Status != model.RequestWithdrawn {
		t.Fatalf("%+v", saved)
	}
	pending, err := s.PendingRequests("fixture/project", 10)
	must(t, err)
	if len(pending) != 0 {
		t.Fatalf("withdrawn request still pending: %+v", pending)
	}
	recent, err := s.RecentRequests("fixture/project", 10)
	must(t, err)
	if len(recent) != 1 || recent[0].ID != original.ID {
		t.Fatalf("recent: %+v", recent)
	}
}

func TestRecentRequestsOrdersBySettlement(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	old := pendingRequest("fixture/project", "long-pending work")
	must(t, s.CreateRequest(old, model.RequestPendingLimit))
	var newest model.OperatorRequest
	for range 50 {
		request := pendingRequest("fixture/project", "newer work")
		must(t, s.CreateRequest(request, model.RequestPendingLimit))
		settled := request.Clone()
		settled.Status = model.RequestWithdrawn
		settled.UpdatedAt = "2026-10-13T12:00:00+00:00"
		must(t, s.UpdateRequest(store.RequestUpdate{Expected: request, Next: settled}))
		newest = settled
	}
	settled := old.Clone()
	settled.Status = model.RequestRejected
	settled.UpdatedAt = "2026-10-13T13:00:00+00:00"
	must(t, s.UpdateRequest(store.RequestUpdate{Expected: old, Next: settled}))
	recent, err := s.RecentRequests("FIXTURE/PROJECT", 50)
	must(t, err)
	if len(recent) != 50 || recent[0].ID != old.ID || recent[1].ID != newest.ID {
		t.Fatalf("recent decisions lost settlement order or stable tie-breaker: %+v", recent)
	}
}

func TestCommitPlanRequestsAtomic(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	control := map[string]any{
		"paused": false, "mode": "run_once", "cycle_number": 1, "next_cycle_at": 0,
		"error": nil, "idle_streak": 3, "context_fingerprint": "",
		"batch": map[string]any{"id": "run-1", "phase": "planning", "cycle_id": "cycle-1"},
	}
	must(t, putMap(s, store.ControlKey.Raw(), control))
	planned := reviewTask()
	planned.CycleID = "cycle-1"
	plan := cycleFor(planned)
	plan.ID = "cycle-1"
	plan.RunID = str("run-1")

	seeded := pendingRequest("Fixture/Project", "accepted work")
	must(t, s.CreateRequest(seeded, model.RequestPendingLimit))
	settled := seeded.Clone()
	settled.Status = model.RequestAccepted
	settled.TaskID = str(planned.ID)
	settled.UpdatedAt = model.Now()
	settled.Assessment = &model.RequestAssessment{
		CycleID: plan.ID, Mode: model.CycleModeExecution, Decision: model.DecisionAccepted,
		Reason: "Evidence supports it", At: model.Now(),
	}
	update := store.RequestUpdate{Expected: seeded, Next: settled}
	must(t, s.CommitPlan(t.Context(), plan, []model.Task{planned}, []store.RequestUpdate{update}))
	saved, err := store.Get(s, store.Requests.Key(seeded.ID))
	must(t, err)
	if saved == nil || saved.Status != model.RequestAccepted || saved.TaskID == nil || *saved.TaskID != planned.ID {
		t.Fatalf("%+v", saved)
	}

	second := pendingRequest("Fixture/Project", "rolled back")
	must(t, s.CreateRequest(second, model.RequestPendingLimit))
	other := reviewTask()
	other.CycleID = "cycle-2"
	failedPlan := cycleFor(other)
	failedPlan.ID = "cycle-2"
	failedPlan.RunID = str("run-1")
	stale := second.Clone()
	stale.Title = "drifted"
	change := second.Clone()
	change.Status = model.RequestRejected
	if err := s.CommitPlan(t.Context(), failedPlan, []model.Task{other}, []store.RequestUpdate{{Expected: stale, Next: change}}); !errors.Is(err, store.ErrRequestChanged) {
		t.Fatalf("stale settlement committed: %v", err)
	}
	if c, _ := store.Get(s, store.Cycles.Key("cycle-2")); c != nil {
		t.Fatal("cycle survived the rolled-back commit")
	}
	if tk, _ := store.Get(s, store.Tasks.Key(other.ID)); tk != nil {
		t.Fatal("task survived the rolled-back commit")
	}
	saved, err = store.Get(s, store.Requests.Key(second.ID))
	must(t, err)
	if saved == nil || saved.Status != model.RequestPending {
		t.Fatalf("request changed by the rolled-back commit: %+v", saved)
	}
}
