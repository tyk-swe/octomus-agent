package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestBaselineCancellationRecoversTerminalWriteRefusal(t *testing.T) {
	t.Parallel()
	f := newPlanningFixture(t)
	a := New(f.state, f.dataDir)
	t.Cleanup(a.Shutdown)
	fingerprint, err := f.cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	schedulerSQL(t, f.state, `CREATE TEMP TRIGGER refuse_terminal BEFORE UPDATE ON records
		WHEN NEW.kind='baseline' AND json_extract(NEW.data,'$.status')!='running'
		BEGIN SELECT RAISE(ABORT, 'synthetic terminal refusal'); END`)
	check, err := a.StartBaseline(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	schedulerSQL(t, f.state, "DROP TRIGGER refuse_terminal")
	view, err := a.BaselineView(nil)
	if err != nil {
		t.Fatal(err)
	}
	before := view["check"].(*model.BaselineCheck)
	if before.Status != model.BaselineStatusRunning || len(before.Commands) != 1 || !before.Commands[0].Success || view["eligible"] != true {
		t.Fatalf("expected retained running evidence without a worker: %+v", view)
	}
	if !hasEvent(t, f.state, check.ID, "baseline_error", "synthetic terminal refusal") {
		t.Fatal("terminal write refusal missing from activity")
	}
	if hasEvent(t, f.state, check.ID, "baseline", "Passed") {
		t.Fatalf("refused terminal write recorded Passed while saved status is %s", before.Status)
	}
	if err := a.CancelBaseline(check.ID); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get[model.BaselineCheck](f.state, "baseline", check.ID)
	if err != nil || saved == nil {
		t.Fatalf("cancelled baseline: %v", err)
	}
	if saved.Status != model.BaselineStatusCancelled || saved.CompletedAt == nil || saved.Error == nil || !strings.Contains(*saved.Error, "after the check worker exited") {
		t.Fatalf("orphan cancellation result: %+v", saved)
	}
	expected := *before
	expected.Status, expected.CompletedAt, expected.Error = saved.Status, saved.CompletedAt, saved.Error
	if !wirejson.Equal(saved, expected) {
		t.Fatal("cancellation changed recorded command, revision, configuration or cleanup evidence")
	}
	if !hasEvent(t, f.state, check.ID, "baseline", "Cancelled") {
		t.Fatal("recovered cancellation missing from activity")
	}
	if hasEvent(t, f.state, check.ID, "baseline", "Passed") {
		t.Fatal("cancelled check has a conflicting Passed outcome in activity")
	}
	if err := a.CancelBaseline(check.ID); err == nil || !IsActionConflict(err) {
		t.Fatalf("finished check accepted another cancellation: %v", err)
	}
	next, err := a.StartBaseline(fingerprint)
	if err != nil {
		t.Fatalf("storage recovery did not allow the next baseline check: %v", err)
	}
	a.wg.Wait()
	latest, err := f.state.LatestBaseline()
	if err != nil || latest == nil || latest.ID != next.ID || latest.Status != model.BaselineStatusPassed {
		t.Fatalf("next baseline result: %+v, %v", latest, err)
	}
	if !hasEvent(t, f.state, next.ID, "baseline", "Passed") {
		t.Fatal("durable passing result missing from activity")
	}
}

func TestBaselineWorkerPreservesPassedStatusWhenActivityFails(t *testing.T) {
	t.Parallel()
	f := newPlanningFixture(t)
	a := New(f.state, f.dataDir)
	t.Cleanup(a.Shutdown)
	fingerprint, err := f.cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	schedulerSQL(t, f.state, `CREATE TEMP TRIGGER refuse_activity BEFORE INSERT ON events
		WHEN NEW.kind='baseline' AND NEW.message='Passed'
		BEGIN SELECT RAISE(ABORT, 'synthetic activity refusal'); END`)
	check, err := a.StartBaseline(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	a.wg.Wait()
	saved, err := store.Get[model.BaselineCheck](f.state, "baseline", check.ID)
	if err != nil || saved == nil || saved.Status != model.BaselineStatusPassed || saved.CompletedAt == nil || !saved.WorkspaceRemoved {
		t.Fatalf("durable passing result: %+v, %v", saved, err)
	}
	if hasEvent(t, f.state, check.ID, "baseline", "Passed") {
		t.Fatal("refused passing activity was stored")
	}
	if hasEvent(t, f.state, check.ID, "baseline_error", "") {
		t.Fatal("activity refusal reported as a terminal state write failure")
	}
}

func TestOrphanBaselineCancellationPreservesAnotherWorker(t *testing.T) {
	t.Parallel()
	a, cfg := baselineApp(t)
	t.Cleanup(a.Shutdown)
	orphan, live := makeCheck(cfg, model.BaselineStatusRunning), makeCheck(cfg, model.BaselineStatusRunning)
	for _, check := range []model.BaselineCheck{orphan, live} {
		if err := a.Store.Put("baseline", check.ID, check); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(a.Context())
	defer cancel()
	a.runtimeMu.Lock()
	a.runtime.baseline = &baselineJob{id: live.ID, cancel: cancel}
	a.runtimeMu.Unlock()
	if err := a.CancelBaseline(orphan.ID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("cancelling an old record cancelled the current worker")
	}
	saved, err := store.Get[model.BaselineCheck](a.Store, "baseline", live.ID)
	if err != nil || saved == nil || !wirejson.Equal(saved, live) {
		t.Fatalf("current baseline changed: %+v, %v", saved, err)
	}
	if err := a.CancelBaseline(live.ID); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("normal cancellation no longer signals its worker")
	}
}

func TestOrphanBaselineCancellationCanRetryStorageRefusals(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"marker", "status"} {
		t.Run(stage, func(t *testing.T) {
			a, cfg := baselineApp(t)
			t.Cleanup(a.Shutdown)
			check := makeCheck(cfg, model.BaselineStatusRunning)
			if err := a.Store.Put("baseline", check.ID, check); err != nil {
				t.Fatal(err)
			}
			operation, predicate := "INSERT", "NEW.kind='baseline_cancel'"
			if stage == "status" {
				operation, predicate = "UPDATE", "NEW.kind='baseline' AND json_extract(NEW.data,'$.status')='cancelled'"
			}
			schedulerSQL(t, a.Store, fmt.Sprintf("CREATE TEMP TRIGGER refuse_cancel BEFORE %s ON records WHEN %s BEGIN SELECT RAISE(ABORT, 'synthetic cancellation refusal'); END", operation, predicate))
			if err := a.CancelBaseline(check.ID); err == nil || !strings.Contains(err.Error(), "synthetic cancellation refusal") {
				t.Fatalf("%s refusal: %v", stage, err)
			}
			saved, err := store.Get[model.BaselineCheck](a.Store, "baseline", check.ID)
			if err != nil || saved == nil || !wirejson.Equal(saved, check) {
				t.Fatalf("failed cancellation changed baseline: %+v, %v", saved, err)
			}
			schedulerSQL(t, a.Store, "DROP TRIGGER refuse_cancel")
			if err := a.CancelBaseline(check.ID); err != nil {
				t.Fatal(err)
			}
			saved, err = store.Get[model.BaselineCheck](a.Store, "baseline", check.ID)
			if err != nil || saved == nil || saved.Status != model.BaselineStatusCancelled {
				t.Fatalf("retry result: %+v, %v", saved, err)
			}
		})
	}
}

func TestOrphanBaselineCancellationAcknowledgesStatusWhenActivityFails(t *testing.T) {
	t.Parallel()
	a, cfg := baselineApp(t)
	t.Cleanup(a.Shutdown)
	check := makeCheck(cfg, model.BaselineStatusRunning)
	if err := a.Store.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}
	schedulerSQL(t, a.Store, `CREATE TEMP TRIGGER refuse_activity BEFORE INSERT ON events
		WHEN NEW.kind='baseline' AND NEW.message='Cancelled'
		BEGIN SELECT RAISE(ABORT, 'synthetic activity refusal'); END`)
	if err := a.CancelBaseline(check.ID); err != nil {
		t.Fatalf("durable cancellation reported failure when only activity failed: %v", err)
	}
	saved, err := store.Get[model.BaselineCheck](a.Store, "baseline", check.ID)
	if err != nil || saved == nil || saved.Status != model.BaselineStatusCancelled || saved.CompletedAt == nil {
		t.Fatalf("durable cancellation result: %+v, %v", saved, err)
	}
	if events, err := a.Store.Events(&check.ID); err != nil || len(events) != 0 {
		t.Fatalf("refused activity was stored: %+v, %v", events, err)
	}
	schedulerSQL(t, a.Store, "DROP TRIGGER refuse_activity")
	if err := a.CancelBaseline(check.ID); !IsActionConflict(err) {
		t.Fatalf("acknowledged cancellation was replayed: %v", err)
	}
}
