package store_test

import (
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func mergeFixture(number uint64, withMerge bool) (model.Task, model.PullRequest, *model.AutoMergeState) {
	t := task()
	t.PRNumber = new(number)
	output := "output-" + string(rune('a'+number))
	t.OutputCommit = &output
	p := model.PullRequest{
		Number: number, Title: "Reviewed maintenance", Branch: t.Branch, Head: output, Base: "main",
		URL: "https://github.com/fixture/project/pull/x", State: "open", Owned: true,
		HeadRepository: "fixture/project", BaseRepository: "fixture/project",
	}
	var merge *model.AutoMergeState
	if withMerge {
		merge = &model.AutoMergeState{
			TaskID: t.ID, Head: output, ComparisonBase: t.ComparisonBase,
			HeadBranch: t.Branch, BaseBranch: t.Config.DefaultBranch,
			PolicyRevision: "policy", Authorized: true, Status: model.AutoMergeWaiting,
			Reason: "Waiting for GitHub checks and protections", ObservedAt: model.Now(),
		}
	}
	return t, p, merge
}

func prMerge(t *testing.T, s *store.Store, number uint64) *model.AutoMergeState {
	t.Helper()
	prs, err := store.List[model.PRObservation](s, "pr")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range prs {
		if o.PR.Number == number {
			return o.AutoMerge
		}
	}
	return nil
}

func publishAtCheckpoint(t *testing.T, s *store.Store, task model.Task, p model.PullRequest, merge *model.AutoMergeState) {
	t.Helper()
	checkpoint := task.Clone()
	checkpoint.Status = model.StatusPublishing
	must(t, s.Put("task", task.ID, checkpoint))
	published := task.Clone()
	published.Status = model.StatusPublished
	must(t, s.CompletePublication(published, p, merge))
}

func intentFrom(merge model.AutoMergeState, attempt string) model.AutoMergeState {
	intent := merge.Clone()
	intent.Status = model.AutoMergeMerging
	intent.AttemptID = new(attempt)
	intent.AttemptedAt = new(model.Now())
	intent.Reason = "Squash merge in flight"
	return intent
}

func TestCompletePublicationRecordsMergeEvidence(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(7, true)
	publishAtCheckpoint(t, s, task, p, merge)

	saved, err := store.Get[model.Task](s, "task", task.ID)
	if err != nil || saved == nil || saved.Status != model.StatusPublished {
		t.Fatalf("published task = %+v, %v", saved, err)
	}
	recorded := prMerge(t, s, 7)
	if recorded == nil || !recorded.Authorized || recorded.Status != model.AutoMergeWaiting ||
		recorded.TaskID != task.ID || recorded.Head != merge.Head || recorded.ComparisonBase != merge.ComparisonBase ||
		recorded.PolicyRevision != merge.PolicyRevision {
		t.Fatalf("recorded merge evidence = %+v", recorded)
	}
	prs, _ := store.List[model.PRObservation](s, "pr")
	if prs[0].DeliveredHead == nil || *prs[0].DeliveredHead != p.Head {
		t.Fatalf("delivered head = %+v", prs[0].DeliveredHead)
	}
	candidates, err := s.MergeCandidates(0, 10)
	must(t, err)
	if len(candidates) != 1 || candidates[0].Observation.AutoMerge.TaskID != task.ID {
		t.Fatalf("merge candidates = %+v", candidates)
	}
	counts, err := s.MergeCounts("fixture/project")
	must(t, err)
	if counts["waiting"] != 1 || len(counts) != 1 {
		t.Fatalf("merge counts = %+v", counts)
	}
}

func TestCompletePublicationRollsBackTogether(t *testing.T) {
	t.Parallel()
	s, err := store.OpenPlan(statePath(t), store.SchemaDDL()+`
		CREATE TRIGGER fail_pr_write BEFORE INSERT ON records
		WHEN NEW.kind='pr' BEGIN SELECT RAISE(FAIL, 'injected pr write failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, p, merge := mergeFixture(8, true)
	checkpoint := task.Clone()
	checkpoint.Status = model.StatusPublishing
	must(t, s.Put("task", task.ID, checkpoint))
	published := task.Clone()
	published.Status = model.StatusPublished
	if err := s.CompletePublication(published, p, merge); err == nil {
		t.Fatal("the injected failure did not roll the publication back")
	}
	saved, err := store.Get[model.Task](s, "task", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.Status != model.StatusPublishing {
		t.Fatalf("the rollback overwrote the durable checkpoint: %+v", saved)
	}
	prs, err := store.List[model.PRObservation](s, "pr")
	must(t, err)
	if len(prs) != 0 {
		t.Fatalf("pr record persisted despite the rollback: %+v", prs)
	}
}

func TestCompletePublicationRejectsStaleCheckpoints(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(13, true)
	queued := task.Clone()
	queued.Status = model.StatusQueued
	queued.OutputCommit = nil
	must(t, s.Put("task", task.ID, queued))
	if err := s.CompletePublication(task, p, merge); err == nil {
		t.Fatal("a task that never checkpointed accepted a publication")
	}
	if saved, _ := store.Get[model.Task](s, "task", task.ID); saved == nil || saved.Status != model.StatusQueued {
		t.Fatalf("the refused publication overwrote the task: %+v", saved)
	}
	checkpoint := task.Clone()
	checkpoint.Status = model.StatusPublishing
	must(t, s.Put("task", task.ID, checkpoint))
	other := task.Clone()
	other.Status = model.StatusPublished
	otherHead := "b" + strings.Repeat("0", 39)
	other.OutputCommit = &otherHead
	if err := s.CompletePublication(other, p, merge); err == nil {
		t.Fatal("a publication with a different output overwrote the checkpoint")
	}
	if saved, _ := store.Get[model.Task](s, "task", task.ID); saved == nil || saved.Status != model.StatusPublishing {
		t.Fatalf("the refused publication overwrote the checkpoint: %+v", saved)
	}
}

func TestClaimMergeOnlyPendingWaitingOrContinuousManual(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(9, true)
	publishAtCheckpoint(t, s, task, p, merge)

	intent := intentFrom(*merge, "attempt-1")
	claimed, err := s.ClaimMerge("fixture/project", 9, intent, false)
	must(t, err)
	if !claimed {
		t.Fatal("the first intent did not claim the pending delivery")
	}
	events, err := s.Events(new(task.ID))
	must(t, err)
	found := false
	for _, event := range events {
		if event.Kind == "automerge" && strings.Contains(event.Message, "attempt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no atomic attempt event was recorded: %+v", events)
	}
	claimed, err = s.ClaimMerge("fixture/project", 9, intentFrom(*merge, "attempt-2"), false)
	must(t, err)
	if claimed {
		t.Fatal("a second intent claimed an in-flight delivery")
	}
	if recorded := prMerge(t, s, 9); recorded.Status != model.AutoMergeMerging || *recorded.AttemptID != "attempt-1" {
		t.Fatalf("in-flight intent = %+v", recorded)
	}
	bad := intentFrom(*merge, "attempt-3")
	bad.Head = "different-head"
	claimed, err = s.ClaimMerge("fixture/project", 9, bad, true)
	must(t, err)
	if claimed {
		t.Fatal("a head-mismatched intent claimed the delivery")
	}
	missing := intentFrom(*merge, "")
	claimed, err = s.ClaimMerge("fixture/project", 9, missing, false)
	must(t, err)
	if claimed {
		t.Fatal("an intent without an attempt identity claimed the delivery")
	}
	unboundHead := intentFrom(*merge, "attempt-4")
	unboundHead.HeadBranch = ""
	claimed, err = s.ClaimMerge("fixture/project", 9, unboundHead, false)
	must(t, err)
	if claimed {
		t.Fatal("an intent without the immutable head branch claimed the delivery")
	}
	unboundBase := intentFrom(*merge, "attempt-5")
	unboundBase.BaseBranch = ""
	claimed, err = s.ClaimMerge("fixture/project", 9, unboundBase, false)
	must(t, err)
	if claimed {
		t.Fatal("an intent without the immutable base branch claimed the delivery")
	}
	attempts := 0
	events, err = s.Events(new(task.ID))
	must(t, err)
	for _, event := range events {
		if event.Kind == "automerge" && strings.Contains(event.Message, "attempt") {
			attempts++
		}
	}
	if attempts != 1 {
		t.Fatalf("refused claims still recorded %d attempt events", attempts)
	}
}

func TestClaimMergeAuthorizedManualUnderContinuousOnly(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(10, true)
	publishAtCheckpoint(t, s, task, p, merge)
	applied, err := s.SettleMerge("fixture/project", 10, *merge, model.AutoMergeManual, "Checks failed on the reviewed head", "", nil, false)
	must(t, err)
	if !applied {
		t.Fatal("conditional manual did not settle")
	}
	if recorded := prMerge(t, s, 10); recorded == nil || !recorded.Authorized {
		t.Fatalf("conditional manual lost its recorded authorization: %+v", recorded)
	}
	candidates, err := s.MergeCandidates(0, 10)
	must(t, err)
	if len(candidates) != 1 {
		t.Fatalf("authorized manual record is not a recheck candidate: %+v", candidates)
	}
	claimed, err := s.ClaimMerge("fixture/project", 10, intentFrom(*merge, "attempt-c"), false)
	must(t, err)
	if claimed {
		t.Fatal("run-once scope claimed a conditional manual record")
	}
	claimed, err = s.ClaimMerge("fixture/project", 10, intentFrom(*merge, "attempt-c"), true)
	must(t, err)
	if !claimed {
		t.Fatal("continuous recheck could not claim an authorized manual record")
	}
}

func TestSettleMergeExactExpected(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(11, true)
	publishAtCheckpoint(t, s, task, p, merge)
	must(t, func() error {
		claimed, err := s.ClaimMerge("fixture/project", 11, intentFrom(*merge, "attempt-1"), false)
		if err != nil || !claimed {
			t.Fatalf("claim = %v, %v", claimed, err)
		}
		return nil
	}())

	noAttempt := merge.Clone()
	applied, err := s.SettleMerge("fixture/project", 11, noAttempt, model.AutoMergeManual, "nil attempt", "", nil, true)
	must(t, err)
	if applied {
		t.Fatal("a nil-attempt expectation settled an in-flight intent")
	}
	stale := merge.Clone()
	stale.TaskID = "other-task"
	inflight := intentFrom(*merge, "attempt-1")
	applied, err = s.SettleMerge("fixture/project", 11, stale, model.AutoMergeMerged, "stale", "", nil, false)
	must(t, err)
	if applied {
		t.Fatal("a foreign-task expectation settled the record")
	}
	wrong := intentFrom(*merge, "attempt-9")
	applied, err = s.SettleMerge("fixture/project", 11, wrong, model.AutoMergeMerged, "late attempt", "", nil, false)
	must(t, err)
	if applied {
		t.Fatal("a late attempt settled over the recorded intent")
	}
	applied, err = s.SettleMerge("fixture/project", 11, inflight, model.AutoMergeMerged, "Squash merged by Octomus", store.MergeResultConfirmed, new("d"+strings.Repeat("0", 39)), false)
	must(t, err)
	if !applied {
		t.Fatal("the recorded attempt's outcome did not settle")
	}
	recorded := prMerge(t, s, 11)
	if recorded.Status != model.AutoMergeMerged || recorded.MergeCommit == nil ||
		recorded.ResultSource == nil || *recorded.ResultSource != store.MergeResultConfirmed {
		t.Fatalf("settled merge = %+v", recorded)
	}
	prs, _ := store.List[model.PRObservation](s, "pr")
	if prs[0].PR.State != "merged" {
		t.Fatalf("PR state not updated atomically: %+v", prs[0].PR)
	}
	applied, err = s.SettleMerge("fixture/project", 11, inflight, model.AutoMergeManual, "late manual", "", nil, true)
	must(t, err)
	if applied {
		t.Fatal("a terminal record accepted a downgrade")
	}
	inflight2 := intentFrom(*merge, "attempt-1")
	applied, err = s.SettleMerge("fixture/project", 11, inflight2, model.AutoMergeMerged, "same", "", new("e"+strings.Repeat("0", 39)), false)
	must(t, err)
	if applied {
		t.Fatal("a conflicting commit overwrote the recorded merge commit")
	}
	newTask, newPR, newMerge := mergeFixture(11, true)
	newTask.ID = task.ID
	newHead := "f" + strings.Repeat("0", 39)
	newTask.OutputCommit = &newHead
	newMerge.Head = newHead
	newPR.Head = newHead
	publishAtCheckpoint(t, s, newTask, newPR, newMerge)
	oldExpected := merge.Clone()
	oldExpected.AttemptID = new("attempt-1")
	oldExpected.Status = model.AutoMergeMerging
	applied, err = s.SettleMerge("fixture/project", 11, oldExpected, model.AutoMergeMerged, "stale old attempt", "", nil, false)
	must(t, err)
	if applied {
		t.Fatal("an old-delivery expectation settled the newer reviewed delivery")
	}
	if recorded := prMerge(t, s, 11); recorded.Head != newHead || recorded.Status != model.AutoMergeWaiting {
		t.Fatalf("new delivery evidence = %+v", recorded)
	}
}

func TestSettleMergeWaitingClearsAttemptOnlyWithExactAttempt(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(12, true)
	publishAtCheckpoint(t, s, task, p, merge)
	inflight := intentFrom(*merge, "attempt-1")
	claimed, err := s.ClaimMerge("fixture/project", 12, inflight, false)
	must(t, err)
	if !claimed {
		t.Fatal("claim failed")
	}
	applied, err := s.SettleMerge("fixture/project", 12, inflight, model.AutoMergeWaiting, "interrupted; fresh checks required", "", nil, false)
	must(t, err)
	if !applied {
		t.Fatal("the read-confirmed open head did not release the attempt")
	}
	recorded := prMerge(t, s, 12)
	if recorded.Status != model.AutoMergeWaiting || recorded.AttemptID != nil || recorded.AttemptedAt == nil {
		t.Fatalf("waiting release = %+v", recorded)
	}
}

func TestRevokeTaskMergesAndCandidatePaging(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	var owner model.Task
	for i := range 12 {
		task, p, merge := mergeFixture(uint64(20+i), true)
		if i == 0 {
			owner = task
		} else {
			merge.TaskID = task.ID
		}
		publishAtCheckpoint(t, s, task, p, merge)
	}
	seen := map[uint64]bool{}
	var cursor int64
	for range 3 {
		page, err := s.MergeCandidates(cursor, 5)
		must(t, err)
		for _, candidate := range page {
			seen[candidate.Observation.PR.Number] = true
			cursor = candidate.Seq
		}
	}
	if len(seen) != 12 {
		t.Fatalf("paged candidates = %d of 12", len(seen))
	}
	must(t, s.RevokeTaskMerges(owner.ID, "The authorizing task was archived"))
	candidates, err := s.MergeCandidates(0, 100)
	must(t, err)
	if len(candidates) != 11 {
		t.Fatalf("revocation left %d pending records", len(candidates))
	}
	prs, _ := store.List[model.PRObservation](s, "pr")
	for _, observation := range prs {
		if observation.AutoMerge != nil && observation.AutoMerge.TaskID == owner.ID &&
			observation.AutoMerge.Status != model.AutoMergeManual {
			t.Fatalf("archived task kept authority: %+v", observation.AutoMerge)
		}
	}
}

func TestBatchMergeWaitScopesToAuthorizingRun(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(30, true)
	task.RunID = new("run-a")
	publishAtCheckpoint(t, s, task, p, merge)
	waiting, err := s.BatchMergeWait("run-a")
	must(t, err)
	if waiting != 1 {
		t.Fatalf("run merge wait = %d", waiting)
	}
	waiting, err = s.BatchMergeWait("run-b")
	must(t, err)
	if waiting != 0 {
		t.Fatalf("foreign run counted this delivery: %d", waiting)
	}
	applied, err := s.SettleMerge("fixture/project", 30, *merge, model.AutoMergeManual, "manual outcome", "", nil, true)
	must(t, err)
	if !applied {
		t.Fatal("manual outcome did not settle")
	}
	waiting, err = s.BatchMergeWait("run-a")
	must(t, err)
	if waiting != 0 {
		t.Fatalf("a manual outcome still held the batch: %d", waiting)
	}
	other, p2, merge2 := mergeFixture(31, true)
	other.RunID = new("run-b")
	p2.Head = "other-head"
	merge2.TaskID = other.ID
	merge2.Head = p2.Head
	publishAtCheckpoint(t, s, other, p2, merge2)
	waiting, err = s.BatchMergeWait("run-a")
	must(t, err)
	if waiting != 0 {
		t.Fatalf("another run's delivery held this batch: %d", waiting)
	}
	waiting, err = s.BatchMergeWait("run-b")
	must(t, err)
	if waiting != 1 {
		t.Fatalf("the other run's delivery did not count: %d", waiting)
	}
}

func TestUnfinishedBranchWorkRepositoryScope(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	pending, err := s.UnfinishedBranchWork("fixture/project", "octomus/work", "nothing")
	must(t, err)
	if pending {
		t.Fatal("an empty branch reported unfinished work")
	}
	blocked := task()
	blocked.Branch = "octomus/work"
	blocked.Status = model.StatusBlocked
	must(t, s.Put("task", blocked.ID, blocked))
	pending, err = s.UnfinishedBranchWork("fixture/project", "octomus/work", blocked.ID)
	must(t, err)
	if pending {
		t.Fatal("the authorizing task counted against itself")
	}
	pending, err = s.UnfinishedBranchWork("fixture/project", "octomus/work", "other")
	must(t, err)
	if !pending {
		t.Fatal("a blocked sibling did not hold the branch")
	}
	pending, err = s.UnfinishedBranchWork("other/repo", "octomus/work", "other")
	must(t, err)
	if pending {
		t.Fatal("a foreign-repository sibling held the branch")
	}
	archived := task()
	archived.Branch = "octomus/work"
	archived.Status = model.StatusBlocked
	archived.Lifecycle.ArchivedAt = new(model.Now())
	must(t, s.Put("task", archived.ID, archived))
	pending, err = s.UnfinishedBranchWork("fixture/project", "octomus/work", "other")
	must(t, err)
	if !pending {
		t.Fatal("an unarchived blocked sibling should hold the branch")
	}
	blocked.Lifecycle.ArchivedAt = new(model.Now())
	must(t, s.Put("task", blocked.ID, blocked))
	pending, err = s.UnfinishedBranchWork("fixture/project", "octomus/work", "other")
	must(t, err)
	if pending {
		t.Fatal("fully archived siblings still held the branch")
	}
	standard, p, _ := mergeFixture(40, false)
	publishAtCheckpoint(t, s, standard, p, nil)
	if merge := prMerge(t, s, 40); merge != nil {
		t.Fatalf("standard delivery recorded merge evidence: %+v", merge)
	}
}

func TestMergeObservationPreservesAndRevokesEvidence(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(50, true)
	publishAtCheckpoint(t, s, task, p, merge)
	must(t, s.RecordPRObservation("fixture/project", p, false))
	if recorded := prMerge(t, s, 50); recorded == nil || recorded.Status != model.AutoMergeWaiting {
		t.Fatalf("refresh lost the pending merge evidence: %+v", recorded)
	}
	moved := p
	moved.Head = "someone-elses-head"
	must(t, s.RecordPRObservation("fixture/project", moved, false))
	recorded := prMerge(t, s, 50)
	if recorded == nil || recorded.Authorized || recorded.Status != model.AutoMergeManual || recorded.Reason == "" {
		t.Fatalf("external head movement did not revoke authority: %+v", recorded)
	}
	movedMerged := moved
	movedMerged.State = "merged"
	must(t, s.RecordPRObservation("fixture/project", movedMerged, false))
	recorded = prMerge(t, s, 50)
	if recorded == nil || recorded.Authorized || recorded.Status != model.AutoMergeManual {
		t.Fatalf("a moved-head merge claimed the reviewed delivery: %+v", recorded)
	}
	prs, _ := store.List[model.PRObservation](s, "pr")
	if prs[0].PR.State != "merged" {
		t.Fatalf("observation PR state = %+v", prs[0].PR)
	}
	inflightTask, p2, merge2 := mergeFixture(51, true)
	publishAtCheckpoint(t, s, inflightTask, p2, merge2)
	inflight := intentFrom(*merge2, "attempt-1")
	claimed, err := s.ClaimMerge("fixture/project", 51, inflight, false)
	must(t, err)
	if !claimed {
		t.Fatal("claim failed")
	}
	moved2 := p2
	moved2.Head = "someone-elses-head"
	must(t, s.RecordPRObservation("fixture/project", moved2, false))
	recorded = prMerge(t, s, 51)
	if recorded == nil || recorded.Status != model.AutoMergeMerging || recorded.AttemptID == nil || recorded.Authorized {
		t.Fatalf("routine observation erased an in-flight intent: %+v", recorded)
	}
	moved2.State = "merged"
	must(t, s.RecordPRObservation("fixture/project", moved2, false))
	recorded = prMerge(t, s, 51)
	if recorded == nil || recorded.Status != model.AutoMergeMerging || recorded.AttemptID == nil || recorded.Authorized {
		t.Fatalf("a routine merged observation resolved the in-flight intent without a status read: %+v", recorded)
	}
	exact := p2
	exact.State = "merged"
	must(t, s.RecordPRObservation("fixture/project", exact, false))
	recorded = prMerge(t, s, 51)
	if recorded == nil || recorded.Status != model.AutoMergeMerging || recorded.AttemptID == nil {
		t.Fatalf("a routine merged observation settled an in-flight intent: %+v", recorded)
	}
}

func TestCompletePublicationRefusesUnresolvedIntent(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(60, true)
	publishAtCheckpoint(t, s, task, p, merge)
	claimed, err := s.ClaimMerge("fixture/project", 60, intentFrom(*merge, "attempt-1"), false)
	must(t, err)
	if !claimed {
		t.Fatal("claim failed")
	}
	checkpoint := task.Clone()
	checkpoint.Status = model.StatusPublishing
	must(t, s.Put("task", task.ID, checkpoint))
	published := task.Clone()
	published.Status = model.StatusPublished
	if err := s.CompletePublication(published, p, merge); err == nil {
		t.Fatal("a republished task overwrote an unresolved merge intent")
	}
	recorded := prMerge(t, s, 60)
	if recorded == nil || recorded.Status != model.AutoMergeMerging || *recorded.AttemptID != "attempt-1" {
		t.Fatalf("the in-flight intent was overwritten: %+v", recorded)
	}
	if saved, _ := store.Get[model.Task](s, "task", task.ID); saved == nil || saved.Status != model.StatusPublishing {
		t.Fatalf("the refused publication overwrote the checkpoint: %+v", saved)
	}
	observed := p
	observed.Number = 64
	raw := merge.Clone()
	raw.Status = model.AutoMergeMerging
	must(t, s.Put("pr", "fixture/project:64", model.PRObservation{
		Repository: "fixture/project", ObservedAt: model.Now(), PR: observed, AutoMerge: &raw,
	}))
	if err := s.CompletePublication(published, observed, merge); err == nil {
		t.Fatal("a republished task overwrote an uncommitted intent")
	}
}

func TestRevokeTaskMergesRetainsInFlightIntent(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(61, true)
	publishAtCheckpoint(t, s, task, p, merge)
	claimed, err := s.ClaimMerge("fixture/project", 61, intentFrom(*merge, "attempt-1"), false)
	must(t, err)
	if !claimed {
		t.Fatal("claim failed")
	}
	must(t, s.RevokeTaskMerges(task.ID, "The authorizing task was archived"))
	recorded := prMerge(t, s, 61)
	if recorded == nil || recorded.Authorized || recorded.Status != model.AutoMergeMerging ||
		recorded.AttemptID == nil || *recorded.AttemptID != "attempt-1" {
		t.Fatalf("revocation erased the recorded intent: %+v", recorded)
	}
}

func TestSettleMergePromotesObservedToConfirmed(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(62, true)
	publishAtCheckpoint(t, s, task, p, merge)
	inflight := intentFrom(*merge, "attempt-1")
	claimed, err := s.ClaimMerge("fixture/project", 62, inflight, false)
	must(t, err)
	if !claimed {
		t.Fatal("claim failed")
	}
	applied, err := s.SettleMerge("fixture/project", 62, inflight, model.AutoMergeMerged, "The pull request is merged on the remote", store.MergeResultObserved, new("d"+strings.Repeat("0", 39)), false)
	must(t, err)
	if !applied {
		t.Fatal("the observed outcome did not settle the intent")
	}
	applied, err = s.SettleMerge("fixture/project", 62, inflight, model.AutoMergeMerged, "Squash merged by Octomus", store.MergeResultConfirmed, new("d"+strings.Repeat("0", 39)), false)
	must(t, err)
	if !applied {
		t.Fatal("the confirmed checkpoint did not promote the observed outcome")
	}
	recorded := prMerge(t, s, 62)
	if recorded.ResultSource == nil || *recorded.ResultSource != store.MergeResultConfirmed ||
		recorded.MergeCommit == nil || recorded.Reason != "Squash merged by Octomus" {
		t.Fatalf("promotion = %+v", recorded)
	}
	applied, err = s.SettleMerge("fixture/project", 62, inflight, model.AutoMergeMerged, "observed again", store.MergeResultObserved, new("d"+strings.Repeat("0", 39)), false)
	must(t, err)
	if !applied {
		t.Fatal("a same-outcome observation could not re-record")
	}
	recorded = prMerge(t, s, 62)
	if *recorded.ResultSource != store.MergeResultConfirmed || recorded.Reason != "Squash merged by Octomus" {
		t.Fatalf("a later observation downgraded confirmed provenance: %+v", recorded)
	}
}

func TestMergeEvidenceRequiresImmutableBinding(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	task, p, merge := mergeFixture(63, true)
	merge.HeadBranch = ""
	merge.BaseBranch = ""
	publishAtCheckpoint(t, s, task, p, merge)
	moved := p
	moved.Head = "someone-elses-head"
	must(t, s.RecordPRObservation("fixture/project", moved, false))
	recorded := prMerge(t, s, 63)
	if recorded == nil || recorded.Status != model.AutoMergeManual || recorded.Authorized {
		t.Fatalf("an unbound intent kept authority: %+v", recorded)
	}
}
