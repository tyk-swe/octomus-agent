package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestPublicationCheckpointReplayKeepsFrozenEvidence(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	frozen := saved.MaintenanceFootprint.Clone()

	checkpoint := saved.Clone()
	checkpoint.Status = model.StatusPublishing
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), checkpoint))
	replay := checkpoint.Clone()
	if err := app.published(&replay, observation.PR); err != nil {
		t.Fatalf("checkpoint replay failed: %v", err)
	}
	evidence := prObservation(t, f, *saved.PRNumber).AutoMerge
	if evidence == nil || evidence.Footprint == nil ||
		*evidence.Footprint.ChangedLines != *frozen.ChangedLines ||
		*evidence.Footprint.ChangedFiles != *frozen.ChangedFiles ||
		len(evidence.Footprint.Paths) != len(frozen.Paths) ||
		evidence.Status != model.AutoMergeWaiting || evidence.Head != frozen.Revision {
		t.Fatalf("replayed evidence = %+v; frozen = %+v", evidence, frozen)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("the replay issued a merge mutation without new authority gates")
	}

	moved := checkpoint
	otherHead := "c" + strings.Repeat("0", 39)
	moved.OutputCommit = &otherHead
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), moved))
	stale := saved.Clone()
	stale.Status = model.StatusPublished
	if err := app.published(&stale, observation.PR); err == nil {
		t.Fatal("a stale output commit overwrote a newer checkpoint")
	}
	current, err := store.Get(f.state, store.Tasks.Key(saved.ID))
	if err != nil || current == nil || *current.OutputCommit != otherHead ||
		current.Status != model.StatusPublishing {
		t.Fatalf("the refused write changed the checkpoint: %+v", current)
	}
}

func TestMergeAttemptFinalGateRejectsStaleState(t *testing.T) {
	f := maintenanceFixture(t)
	writePRPatch(t, f, map[string]any{"merge_queue": true})
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	if merge := observation.AutoMerge; merge == nil || merge.Status != model.AutoMergeWaiting {
		t.Fatalf("initial evidence = %+v", merge)
	}

	control, err := app.Control()
	must0(t, err)
	control.Paused = true
	must0(t, f.state.SaveControl(control))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("paused mode attempted: %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a paused attempt reached the mutation")
	}

	setContinuous(t, app)
	mutated := saved.Clone()
	otherHead := "b" + strings.Repeat("0", 39)
	mutated.OutputCommit = &otherHead
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), mutated))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a stale canonical head reached the mutation")
	}
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("the stale attempt mutated the record: %+v", merge)
	}
	mutated.OutputCommit = saved.OutputCommit
	mutated.Reviews[len(mutated.Reviews)-1].Result.Findings = []model.Finding{{Title: "Post-check defect", Detail: "A finding recorded after the remote precheck", Priority: "P1"}}
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), mutated))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a revoked canonical review reached the mutation")
	}
	mutated.Reviews[len(mutated.Reviews)-1].Result.Findings = []model.Finding{}
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), mutated))

	archived := saved.Clone()
	archived.Lifecycle.ArchivedAt = new(model.Now())
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), archived))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("an archived authorizing task reached the mutation")
	}
	archived.Lifecycle.ArchivedAt = nil
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), archived))

	revoked := saved.Clone()
	revoked.Verification[len(revoked.Verification)-1].Success = false
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), revoked))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a revoked canonical verification reached the mutation")
	}
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), saved.Clone()))

	must0(t, f.state.MarkCancel(saved.ID))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a cancellation marker at the seam reached the mutation")
	}
	must0(t, f.state.ClearCancel(saved.ID))

	f.configure(t, func(cfg *config.Config) { cfg.AutoMergeExcludedPaths = []string{"feature.txt"} })
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a tightened live exclusion reached the mutation")
	}
	f.configure(t, func(cfg *config.Config) { cfg.AutoMergeExcludedPaths = []string{} })

	original := unfinishedBranchWork
	unfinishedBranchWork = func(_ *store.Store, _, _, _ string) (bool, error) {
		return false, fmt.Errorf("injected branch check failure")
	}
	defer func() { unfinishedBranchWork = original }()
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a failed same-branch check reached the mutation")
	}
	unfinishedBranchWork = func(_ *store.Store, _, _, _ string) (bool, error) {
		return true, nil
	}
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("queued same-branch work inserted at the seam still merged")
	}
	unfinishedBranchWork = original
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("the refused attempt mutated the record: %+v", merge)
	}
}

func TestMergeAttemptRequiresAuthorizingRun(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	must0(t, err)
	control.SetMode(model.OperatingModeRunOnce)
	control.Batch = &model.RunBatch{ID: "batch-current", Phase: model.BatchPhaseMerging}
	must0(t, f.state.SaveControl(control))

	app.mergeAttempt(t.Context(), saved, observation, true)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("a task outside the run attempted: %+v", merge)
	}
	taskRun := "batch-other"
	mutated := saved.Clone()
	mutated.RunID = &taskRun
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), mutated))
	app.mergeAttempt(t.Context(), mutated, observation, true)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("a foreign-run task attempted: %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a non-authorizing run reached the mutation")
	}
}

func TestMergeAttemptPausedDuringPutSerializes(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	must0(t, os.WriteFile(filepath.Join(f.root, "merge-hold"), []byte("1"), 0o600))
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.mergeAttempt(t.Context(), saved, observation, false)
	}()
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "merge-entered"))
		return err == nil
	}) {
		t.Fatal("the merge request did not reach the hold")
	}
	paused := make(chan error, 1)
	go func() {
		_, err := app.ControlAction(model.ControlPause)
		paused <- err
	}()
	must0(t, os.Remove(filepath.Join(f.root, "merge-hold")))
	<-done
	must0(t, <-paused)
	merged := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merged.Status != model.AutoMergeMerged || merged.MergeCommit == nil {
		t.Fatalf("in-flight merge did not complete: %+v", merged)
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %d; want exactly one", len(mergeAttempts(t, f)))
	}
	app.mergeCandidate(t.Context(), f.cfg, false, true, false, nil, prObservation(t, f, *saved.PRNumber))
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatal("a paused pass wrote after the in-flight merge")
	}
}

func TestMergeLostAckRestartObservedMerged(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	must0(t, os.WriteFile(filepath.Join(f.root, "merge-after-hold"), []byte("1"), 0o600))
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.mergeAttempt(app.ctx, saved, observation, false)
	}()
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "merge-written"))
		return err == nil
	}) {
		t.Fatal("the merge request did not reach the after-write hold")
	}
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerging || recorded.AttemptID == nil {
		t.Fatalf("durable intent = %+v", recorded)
	}
	app.Shutdown()
	<-done
	must0(t, os.Remove(filepath.Join(f.root, "merge-after-hold")))
	restarted := f.pausedApp(t)
	driveMerge(t, f, restarted, *saved.PRNumber, model.AutoMergeMerged)
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.ResultSource == nil || *settled.ResultSource != store.MergeResultObserved {
		t.Fatalf("the restarted pass did not read-settle the lost acknowledgement: %+v", settled)
	}
	if prObservation(t, f, *saved.PRNumber).PR.State != "merged" {
		t.Fatal("the observed outcome did not update the saved PR state")
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %+v; want exactly the original PUT", mergeAttempts(t, f))
	}
}

func TestMergeInterruptedBeforeWriteRetriesOnce(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	merge := *observation.AutoMerge
	inflight := merge.Clone()
	inflight.Status = model.AutoMergeMerging
	inflight.AttemptID = new("attempt-1")
	inflight.AttemptedAt = new(model.Now())
	claimed, err := f.state.ClaimMerge(observation.Repository, observation.PR.Number, inflight, false)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	current := prObservation(t, f, *saved.PRNumber)
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, current)
	released := prObservation(t, f, *saved.PRNumber).AutoMerge
	if released.Status != model.AutoMergeWaiting || released.AttemptID != nil || released.AttemptedAt == nil {
		t.Fatalf("interrupted intent = %+v", released)
	}
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, prObservation(t, f, *saved.PRNumber))
	merged := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merged.Status != model.AutoMergeMerged {
		t.Fatalf("retry outcome = %+v", merged)
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %+v; want exactly one PUT", mergeAttempts(t, f))
	}
}

func TestMergeHeadMovedDuringHoldRejectedByServer(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	initial := remoteHead(t, f, "main")
	must0(t, os.WriteFile(filepath.Join(f.root, "merge-hold"), []byte("1"), 0o600))
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.mergeAttempt(t.Context(), saved, observation, false)
	}()
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "merge-entered"))
		return err == nil
	}) {
		t.Fatal("the merge request did not reach the hold")
	}
	pushCommit(t, f, "push-move", "octomus/moved", "moved.txt", "x\n", "moved head", "octomus/moved:"+saved.Branch, true)
	must0(t, os.Remove(filepath.Join(f.root, "merge-hold")))
	<-done
	if head := remoteHead(t, f, "main"); head != initial {
		t.Fatalf("the remote merged despite the moved head: %s -> %s", initial, head)
	}
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeManual || recorded.Authorized || recorded.MergeCommit != nil {
		t.Fatalf("a moved-head write was not refused: %+v", recorded)
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 1 {
		t.Fatalf("attempts = %+v; want exactly one refused request", attempts)
	}
}

func TestMergeReconcileExternallyMerged(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)

	patchPRs(t, f, map[string]any{"state": "merged", "merged_at": "2026-10-01T00:00:00Z", "merge_commit_sha": "c" + strings.Repeat("0", 39)})
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerged || recorded.ResultSource == nil || *recorded.ResultSource != store.MergeResultObserved {
		t.Fatalf("externally merged outcome = %+v", recorded)
	}
	if prObservation(t, f, *saved.PRNumber).PR.State != "merged" {
		t.Fatal("PR state did not record the merged outcome")
	}

	patchPRs(t, f, map[string]any{"state": "closed", "merged_at": nil, "merge_commit_sha": nil})
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, prObservation(t, f, *saved.PRNumber))
	recorded = prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerged {
		t.Fatalf("a terminal merged record was downgraded: %+v", recorded)
	}
}

func TestMergeCandidateForeignBatchSkipped(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, new("other-run"), observation)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("foreign-batch candidate was processed: %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("foreign-batch candidate reached the mutation")
	}
}

func TestMergeWaitingSettlesManualWhenModeOff(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	standard := f.cfg.Clone()
	standard.DeliveryMode = config.DeliveryModeStandard
	app.mergeCandidate(t.Context(), standard, true, true, false, nil, observation)
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge.Status != model.AutoMergeManual || merge.Authorized {
		t.Fatalf("disabled-mode evidence = %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a disabled mode reached the mutation")
	}
}

func TestMergePausedReadSettlesIntentOnce(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	merge := observation.AutoMerge
	intent := merge.Clone()
	intent.Status = model.AutoMergeMerging
	intent.AttemptID = new("attempt-paused")
	intent.AttemptedAt = new(model.Now())
	claimed, err := f.state.ClaimMerge(observation.Repository, observation.PR.Number, intent, false)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	applied, err := f.state.SettleMerge(observation.Repository, observation.PR.Number, intent,
		model.AutoMergeUncertain, "The merge request outcome is unconfirmed", "", nil, false)
	if err != nil || !applied {
		t.Fatalf("uncertain settle = %v, %v", applied, err)
	}
	control, err := app.Control()
	must0(t, err)
	forceMergeCheck(app)
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	if reads := statusReads(t, f); reads != 1 {
		t.Fatalf("paused reconciliation used %d reads; want exactly 1", reads)
	}
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeWaiting || settled.AttemptID != nil {
		t.Fatalf("paused reconciliation = %+v", settled)
	}
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	if reads := statusReads(t, f); reads != 1 {
		t.Fatalf("an immediate paused tick re-read the remote: %d reads", reads)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a paused pass reached the mutation")
	}
}

func TestMergeReconcileClosedExactAndMovedBase(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)

	patchPRs(t, f, map[string]any{"state": "closed"})
	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeClosed || recorded.ResultSource == nil ||
		*recorded.ResultSource != store.MergeResultObserved {
		t.Fatalf("closed exact-head outcome = %+v", recorded)
	}
	if prObservation(t, f, *saved.PRNumber).PR.State != "closed" {
		t.Fatal("PR state did not record the closed outcome")
	}

	f2 := maintenanceFixture(t)
	saved2, observation2 := publishedMaintenanceTask(t, f2)
	patchPRs(t, f2, map[string]any{"base": map[string]any{"ref": "release", "repo": map[string]any{"full_name": "fixture/project"}}})
	app2 := f2.pausedApp(t)
	app2.mergeCandidate(t.Context(), f2.cfg, false, false, true, nil, observation2)
	recorded = prObservation(t, f2, *saved2.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeManual || recorded.Authorized {
		t.Fatalf("a retargeted base kept authority: %+v", recorded)
	}
	if len(mergeAttempts(t, f2)) != 0 {
		t.Fatal("a moved base reached the mutation")
	}
}

func TestMergeInFlightReadsOriginalRepository(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	merge := *observation.AutoMerge
	intent := merge.Clone()
	intent.Status = model.AutoMergeUncertain
	intent.AttemptID = new("attempt-old")
	intent.AttemptedAt = new(model.Now())
	claimed, err := f.state.ClaimMerge(observation.Repository, observation.PR.Number,
		func() model.AutoMergeState { m := intent; m.Status = model.AutoMergeMerging; return m }(), false)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	applied, err := f.state.SettleMerge(observation.Repository, observation.PR.Number,
		func() model.AutoMergeState { m := intent; m.Status = model.AutoMergeMerging; return m }(),
		model.AutoMergeUncertain, "unconfirmed", "", nil, false)
	if err != nil || !applied {
		t.Fatalf("uncertain = %v, %v", applied, err)
	}
	moved := f.cfg.Clone()
	moved.GitHubRepo = "other/repo"
	before := statusReads(t, f)
	app.mergeCandidate(t.Context(), moved, false, true, false, nil, prObservation(t, f, *saved.PRNumber))
	if reads := statusReads(t, f); reads != before+1 {
		t.Fatalf("the in-flight intent used %d reads against the original repository", reads-before)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a foreign-repository configuration reached the mutation")
	}
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeWaiting || settled.AttemptID != nil {
		t.Fatalf("the original-repository read did not resolve the intent: %+v", settled)
	}
}

func TestMergeBlockedBranchCheckFailureDoesNotMutate(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	original := unfinishedBranchWork
	unfinishedBranchWork = func(_ *store.Store, _, _, _ string) (bool, error) {
		return false, fmt.Errorf("injected branch check failure")
	}
	defer func() { unfinishedBranchWork = original }()
	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeWaiting || recorded.Reason == "" {
		t.Fatalf("a failed precheck settled or stayed silent: %+v", recorded)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a failed precheck reached the mutation")
	}
}

func TestMergeSettlementFailureBlocksUntilReconciled(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	original := settleMergeRecord
	settleMergeRecord = func(s *store.Store, repo string, number uint64, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) (bool, error) {
		if status == model.AutoMergeMerged && repo == observation.Repository && number == observation.PR.Number {
			return false, fmt.Errorf("injected settlement failure")
		}
		return s.SettleMerge(repo, number, expected, status, reason, source, commit, revoke)
	}
	defer func() { settleMergeRecord = original }()

	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("the write did not happen: %+v", mergeAttempts(t, f))
	}
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerging || recorded.AttemptID == nil {
		t.Fatalf("the failed settlement did not keep the durable intent: %+v", recorded)
	}
	if a := app.recoveryConflict(); a == nil {
		t.Fatal("a failed settlement did not raise the recovery barrier")
	}
	app.mergeAttempt(t.Context(), saved, prObservation(t, f, *saved.PRNumber), false)
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatal("a claim proceeded while the merge barrier held")
	}
	if err := app.tick(); err != nil {
		t.Fatalf("tick during merge barrier = %v", err)
	}
	app.wg.Wait()
	app.runtimeMu.Lock()
	if len(app.runtime.mergeRecoveryErrors) != 1 {
		t.Fatalf("an unrelated tick cleared the merge barrier: %+v", app.runtime.mergeRecoveryErrors)
	}
	app.runtimeMu.Unlock()
	blocked := queuedTask(f.cfg, "blocked-dispatch", "main", "octomus/blocked-dispatch")
	putTask(t, f, blocked)
	if err := app.tick(); err != nil {
		t.Fatalf("tick during merge barrier = %v", err)
	}
	app.wg.Wait()
	if queued, _ := store.Get(f.state, store.Tasks.Key(blocked.ID)); queued == nil || queued.Status != model.StatusQueued {
		t.Fatalf("dispatch proceeded while the merge barrier held: %+v", queued)
	}
	must0(t, store.Put(f.state, store.Tasks.Key(blocked.ID), func() model.Task { dead := blocked; dead.Status = model.StatusFailed; return dead }()))

	settleMergeRecord = original
	app.mergeCandidate(t.Context(), f.cfg, false, true, false, nil, prObservation(t, f, *saved.PRNumber))
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeMerged || settled.ResultSource == nil ||
		*settled.ResultSource != store.MergeResultObserved {
		t.Fatalf("reconciliation after the fault = %+v", settled)
	}
	app.runtimeMu.Lock()
	if len(app.runtime.mergeRecoveryErrors) != 0 {
		t.Fatalf("the barrier survived a successful settlement: %+v", app.runtime.mergeRecoveryErrors)
	}
	app.runtimeMu.Unlock()
	if app.recoveryConflict() != nil {
		t.Fatal("the same process stayed blocked after a successful settlement")
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("a second PUT happened: %+v", mergeAttempts(t, f))
	}
}

func TestMergeRecoveryBarriersRemainIndependent(t *testing.T) {
	app := New(testStore(t), t.TempDir())
	defer app.Shutdown()

	app.setRecoveryError(errors.New("ordinary recovery write failed"))
	app.setMergeRecoveryError("fixture/project:1", errors.New("merge settlement failed"))
	app.setMergeRecoveryError("fixture/project:2", errors.New("merge settlement failed"))
	app.clearMergeRecoveryError("fixture/project:1")
	app.clearMergeRecoveryError("fixture/project:2")
	if err := app.recoveryConflict(); err == nil {
		t.Fatal("successful merge settlement cleared the generic recovery barrier")
	}
	app.runtimeMu.Lock()
	app.runtime.activeRecoveryError = nil
	app.runtimeMu.Unlock()
	if err := app.recoveryConflict(); err != nil {
		t.Fatalf("the recovered barriers still block: %v", err)
	}

	app.setMergeRecoveryError("fixture/project:3", errors.New("merge settlement failed"))
	app.setMergeRecoveryError("fixture/project:4", errors.New("merge settlement failed"))
	app.clearMergeRecoveryError("fixture/project:3")
	if err := app.recoveryConflict(); err == nil {
		t.Fatal("clearing one merge key dropped the remaining barrier")
	}
	app.clearMergeRecoveryError("fixture/project:4")
	if err := app.recoveryConflict(); err != nil {
		t.Fatalf("the last merge key did not release the barrier: %v", err)
	}
}

func TestMergeRecoveryErrorShownInStateView(t *testing.T) {
	app := New(testStore(t), t.TempDir())
	defer app.Shutdown()

	app.setMergeRecoveryError("fixture/project:7", errors.New("merge settlement failed"))
	view, err := app.StateView()
	must0(t, err)
	if view.Status != "unhealthy" {
		t.Fatalf("merge-only barrier status = %v", view.Status)
	}
	if view.RecoveryError == nil || *view.RecoveryError == "" {
		t.Fatalf("the view hid the merge barrier: %+v", view.RecoveryError)
	}
	app.clearMergeRecoveryError("fixture/project:7")
	view, err = app.StateView()
	must0(t, err)
	if view.RecoveryError != nil {
		t.Fatalf("a settled key still reports: %v", *view.RecoveryError)
	}
	if view.Status == "unhealthy" {
		t.Fatal("a settled key still reports unhealthy")
	}
}

// failWaitingSettlement injects one waiting-state settlement failure and returns
// the restore, so a later pass can exercise the real store function.
func failWaitingSettlement(t *testing.T) func() {
	t.Helper()
	original := settleMergeRecord
	settleMergeRecord = func(s *store.Store, repo string, number uint64, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) (bool, error) {
		if status == model.AutoMergeWaiting {
			return false, fmt.Errorf("injected waiting settlement failure")
		}
		return s.SettleMerge(repo, number, expected, status, reason, source, commit, revoke)
	}
	return func() { settleMergeRecord = original }
}

// runMergePass runs one recovery and candidate pass, forcing the pacing gate.
func runMergePass(t *testing.T, app *App) {
	t.Helper()
	forceMergeCheck(app)
	if err := app.tick(); err != nil {
		t.Fatalf("merge tick: %v", err)
	}
	app.wg.Wait()
}

func TestMergePrecheckFailureRecoversToMerge(t *testing.T) {
	f := maintenanceFixture(t)
	saved, _ := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)
	patchPRs(t, f, map[string]any{"check_status": "EXPECTED"})

	restore := failWaitingSettlement(t)
	defer restore()
	runMergePass(t, app)
	restore()
	if app.recoveryConflict() == nil {
		t.Fatal("a failed precheck settlement did not raise a recovery barrier")
	}
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("pending-check evidence = %+v", merge)
	}

	patchPRs(t, f, map[string]any{"check_status": "SUCCESS"})
	runMergePass(t, app)
	merged := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merged.Status != model.AutoMergeMerged || merged.MergeCommit == nil {
		t.Fatalf("the recovered precheck did not permit the merge: %+v", merged)
	}
	if app.recoveryConflict() != nil {
		t.Fatal("the barrier survived the recovered precheck")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 1 {
		t.Fatalf("attempts = %+v; want exactly one merge", attempts)
	}
}

func TestMergePrecheckRecoveryRetriesSameStatusSettlement(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	expected := *observation.AutoMerge
	reason := "Waiting for checks to complete on the reviewed head"
	restore := failWaitingSettlement(t)
	defer restore()
	app.settleMerge(observation, expected, model.AutoMergeWaiting, reason, "", nil, false)
	if app.recoveryConflict() == nil {
		t.Fatal("a failed same-status settlement did not raise a recovery barrier")
	}
	if app.recoverMergePrechecks(t.Context()) || app.recoveryConflict() == nil {
		t.Fatal("recovery cleared the barrier while the intended settlement was still refused")
	}
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Reason != expected.Reason || merge.ObservedAt != expected.ObservedAt {
		t.Fatalf("a refused settlement changed its saved evidence: %+v", merge)
	}
	restore()
	if !app.recoverMergePrechecks(t.Context()) || app.recoveryConflict() != nil {
		t.Fatal("recovery did not release the barrier after committing the settlement")
	}
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge.Status != model.AutoMergeWaiting || merge.Reason != reason || merge.ObservedAt == expected.ObservedAt {
		t.Fatalf("recovery lost the intended reason or timestamp: %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("precheck recovery issued a merge request")
	}
}

func TestMergePrecheckRecoveryPreservesNewerSameStatusDecision(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	expected := *observation.AutoMerge
	restore := failWaitingSettlement(t)
	defer restore()
	app.settleMerge(observation, expected, model.AutoMergeWaiting, "Older waiting decision", "", nil, false)
	restore()
	applied, err := f.state.SettleMerge(observation.Repository, *saved.PRNumber, expected, model.AutoMergeWaiting, "Newer waiting decision", "", nil, false)
	must0(t, err)
	if !applied {
		t.Fatal("the newer waiting decision was not saved")
	}
	before := prObservation(t, f, *saved.PRNumber).AutoMerge
	if !app.recoverMergePrechecks(t.Context()) || app.recoveryConflict() != nil {
		t.Fatal("a newer decision did not release the stale precheck barrier")
	}
	after := prObservation(t, f, *saved.PRNumber).AutoMerge
	if after.Status != before.Status || after.Reason != before.Reason || after.ObservedAt != before.ObservedAt {
		t.Fatalf("recovery overwrote a newer decision: before=%+v, after=%+v", before, after)
	}
}

func TestMergePrecheckFailureArchivedLeavesNoBarrier(t *testing.T) {
	f := maintenanceFixture(t)
	saved, _ := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)
	patchPRs(t, f, map[string]any{"check_status": "EXPECTED"})

	restore := failWaitingSettlement(t)
	defer restore()
	runMergePass(t, app)
	restore()
	if app.recoveryConflict() == nil {
		t.Fatal("a failed precheck settlement did not raise a recovery barrier")
	}

	must0(t, app.TaskAction(saved.ID, model.TaskActionArchive))
	runMergePass(t, app)
	if app.recoveryConflict() != nil {
		t.Fatal("a resolved archival left a stale barrier")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 0 {
		t.Fatalf("an archived delivery still merged: %+v", attempts)
	}
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge.Status != model.AutoMergeManual || merge.Authorized {
		t.Fatalf("archived merge evidence = %+v", merge)
	}
	candidates, err := f.state.MergeCandidates(0, 10)
	must0(t, err)
	if len(candidates) != 0 {
		t.Fatalf("the revoked delivery is still a merge candidate: %+v", candidates)
	}
}

func TestMergePrecheckFailureTerminalObservationResolves(t *testing.T) {
	f := maintenanceFixture(t)
	saved, _ := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)
	patchPRs(t, f, map[string]any{"check_status": "EXPECTED"})

	restore := failWaitingSettlement(t)
	defer restore()
	runMergePass(t, app)
	restore()
	if app.recoveryConflict() == nil {
		t.Fatal("a failed precheck settlement did not raise a recovery barrier")
	}

	patchPRs(t, f, map[string]any{"state": "merged", "merged_at": "2026-10-01T00:00:00Z", "merge_commit_sha": "c" + strings.Repeat("0", 39)})
	runMergePass(t, app)
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeMerged || settled.ResultSource == nil ||
		*settled.ResultSource != store.MergeResultObserved {
		t.Fatalf("terminal observation = %+v", settled)
	}
	if app.recoveryConflict() != nil {
		t.Fatal("a terminal observation left the precheck barrier")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 0 {
		t.Fatalf("a terminal delivery issued a merge request: %+v", attempts)
	}
}

func TestMergePrecheckRecoveryPreservesUnresolvedBarriers(t *testing.T) {
	f := maintenanceFixture(t)
	_, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)

	pre := mergePrecheck{
		message:    "injected precheck failure",
		repository: observation.Repository,
		number:     observation.PR.Number,
		expected:   *observation.AutoMerge,
		status:     model.AutoMergeWaiting,
		reason:     "Waiting for checks to complete on the reviewed head",
	}
	app.setMergePrecheckError(mergeBarrierKey(pre.repository, pre.number), pre)
	app.setMergeRecoveryError(mergeBarrierKey(observation.Repository, 9999), errors.New("unresolved mutation outcome"))
	if app.recoveryConflict() == nil {
		t.Fatal("the seeded barriers did not block")
	}

	if err := app.tick(); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	app.wg.Wait()

	app.runtimeMu.Lock()
	prechecks := len(app.runtime.mergePrecheckErrors)
	mutations := len(app.runtime.mergeRecoveryErrors)
	app.runtimeMu.Unlock()
	if prechecks != 0 {
		t.Fatalf("the recovered precheck survived: %d", prechecks)
	}
	if mutations != 1 {
		t.Fatalf("recovery dropped an unrelated mutation barrier: %d", mutations)
	}
	if app.recoveryConflict() == nil {
		t.Fatal("the unresolved mutation barrier no longer blocks")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 0 {
		t.Fatalf("a mutation barrier did not block the merge: %+v", attempts)
	}
}
