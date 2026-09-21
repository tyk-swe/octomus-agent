package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func testConfig(repository string) config.Config {
	_ = os.MkdirAll(filepath.Join(repository, ".git"), 0o755)
	cfg := config.Default()
	cfg.Repository = repository
	cfg.GitHubRepo = "fixture/project"
	cfg.DefaultBranch = "main"
	cfg.BranchPrefix = "octomus/"
	for _, role := range config.Roles() {
		cfg.Roles[role] = config.NewRoute("gpt-6-astra", "medium")
	}
	for _, tier := range config.Tiers() {
		cfg.Tiers[tier] = config.NewRoute("gpt-6-astra", "medium")
	}
	cfg.RepairRoute = config.NewRoute("gpt-6-astra", "medium")
	cfg.VerificationCommands = []string{"true"}
	return cfg
}

func saveSettings(t *testing.T, state *store.Store, cfg config.Config, control model.Control) {
	t.Helper()
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
}

func proposal(id, target string) model.Proposal {
	return model.Proposal{
		ID: id, Title: "Concrete " + id, Problem: "Missing behavior " + id,
		Evidence: []string{"README.md"}, Benefit: "Useful behavior", Category: "features",
		Target: target, Tier: "M", Scope: "one file", Dependencies: []string{},
		Prompt: "Implement and verify the documented behavior", Decision: model.DecisionAccepted,
		Reason: "Grounded and worthwhile", ProblemKey: "problem-" + id,
		RelevantPaths: []string{"README.md"}, Reconsiders: []string{},
	}
}

func queuedTask(cfg config.Config, id, target, branch string) model.Task {
	p := proposal(id, target)
	return model.Task{
		ID: id, CycleID: "cycle", Proposal: p, Status: model.StatusQueued,
		Route: cfg.Tiers[p.Tier], Config: cfg.Clone(), SourceRevision: "source",
		ComparisonBase: "source", DefaultRevision: "source", Branch: branch,
		Sessions: []model.Session{}, Reviews: []model.ReviewRound{}, Verification: []model.Verification{},
		CreatedAt: model.Now(), UpdatedAt: model.Now(), SupersededBy: []string{}, Supersedes: []string{},
	}
}

func ownedPR(branch string) model.PullRequest {
	return model.PullRequest{
		Number: 7, Title: "Owned work", Branch: branch, Head: "head", Base: "main",
		URL: "https://example.test/pr/7", State: "open", Owned: true,
		HeadRepository: "fixture/project", BaseRepository: "fixture/project",
	}
}

func TestIdleDelayMatchesDurableBackoffContract(t *testing.T) {
	for _, test := range []struct {
		base   uint64
		streak uint32
		want   uint64
	}{{1800, 1, 1800}, {1800, 2, 3600}, {1800, 20, 86400}, {100000, 20, 100000}} {
		if got := IdleDelay(test.base, test.streak); got != test.want {
			t.Fatalf("IdleDelay(%d, %d) = %d, want %d", test.base, test.streak, got, test.want)
		}
	}
}

func TestPausePreservesDurableErrorEvidence(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	message := "recorded planning failure"
	control.Error = &message
	saveSettings(t, state, cfg, control)
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)

	if err := app.Pause(); err != nil {
		t.Fatal(err)
	}
	paused, err := app.Control()
	if err != nil || paused.Mode != model.OperatingModePaused || paused.Error == nil || *paused.Error != message {
		t.Fatalf("pause did not preserve durable error evidence: %+v, %v", paused, err)
	}
}

func TestProposalValidationRequiresKnownAcyclicOrderedDependencies(t *testing.T) {
	cfg := testConfig(t.TempDir())
	grounding := model.Grounding{Revision: "source", PRs: []model.PullRequest{ownedPR("octomus/existing")}}

	first := proposal("first", "octomus/existing")
	second := proposal("second", "octomus/existing")
	second.Dependencies = []string{"first"}
	if err := ValidateProposals(cfg, []model.Proposal{first, second}, grounding, nil); err != nil {
		t.Fatalf("valid same-PR chain rejected: %v", err)
	}

	cyclicFirst, cyclicSecond := first.Clone(), second.Clone()
	cyclicFirst.Dependencies = []string{"second"}
	if err := ValidateProposals(cfg, []model.Proposal{cyclicFirst, cyclicSecond}, grounding, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle was not rejected: %v", err)
	}

	fork := proposal("fork", "octomus/existing")
	fork.Dependencies = []string{"first"}
	if err := ValidateProposals(cfg, []model.Proposal{first, second, fork}, grounding, nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "complete dependency order") {
		t.Fatalf("unordered writers were not rejected: %v", err)
	}

	unknown := proposal("unknown", "octomus/existing")
	unknown.Dependencies = []string{"missing"}
	if err := ValidateProposals(cfg, []model.Proposal{unknown}, grounding, nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "accepted proposal") {
		t.Fatalf("unknown dependency was not rejected: %v", err)
	}

	defaultDependency := proposal("default-dependent", cfg.DefaultBranch)
	defaultDependency.Dependencies = []string{"first"}
	if err := ValidateProposals(cfg, []model.Proposal{first, defaultDependency}, grounding, nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "default-branch") {
		t.Fatalf("default-branch dependency was not rejected: %v", err)
	}
}

func TestRejectedInvalidTargetIsNeverExecutable(t *testing.T) {
	cfg := testConfig(t.TempDir())
	grounding := model.Grounding{Revision: "source", PRs: []model.PullRequest{ownedPR("octomus/existing")}}
	rejected := proposal("rejected", "someone-elses-branch")
	rejected.Decision = model.DecisionRejected
	if err := ValidateProposals(cfg, []model.Proposal{rejected}, grounding, nil); err != nil {
		t.Fatalf("a rejected recommendation should be accountably retained: %v", err)
	}
	rejected.Decision = model.DecisionAccepted
	if err := ValidateProposals(cfg, []model.Proposal{rejected}, grounding, nil); err == nil || !strings.Contains(strings.ToLower(err.Error()), "owned open pr") {
		t.Fatalf("accepted unowned target was not rejected: %v", err)
	}

	duplicate := proposal("duplicate", cfg.DefaultBranch)
	history := queuedTask(cfg, "old", cfg.DefaultBranch, "octomus/old")
	history.Proposal = duplicate.Clone()
	if err := ValidateProposals(cfg, []model.Proposal{duplicate}, model.Grounding{Revision: "source"}, []model.Task{history}); err == nil || !strings.Contains(err.Error(), "recorded") {
		t.Fatalf("duplicate recorded work was not rejected: %v", err)
	}
}

func TestPlanningAdmissionBudgetIsAtomicUnderConcurrency(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = 5
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results <- state.ReserveSession(0, store.NewAdmission("cycle", nil, fmt.Sprintf("role-%d", index), cfg.Roles["discovery"]))
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, model.BlockedReasonBudgetExhausted) {
			t.Fatalf("unexpected admission error: %v", err)
		}
	}
	if successes != 5 {
		t.Fatalf("got %d successful admissions, want exactly 5", successes)
	}
	if used, err := state.SessionsToday(); err != nil || used != 5 {
		t.Fatalf("durable usage = %d, %v; want 5", used, err)
	}
}

func TestRunOnceAffordabilityAndMembershipAreAtomic(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = cfg.PlanningAdmissionsRequired() - 1
	task := queuedTask(cfg, "original", cfg.DefaultBranch, "octomus/original")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	saveSettings(t, state, cfg, model.DefaultControl())
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if err := a.RunOnce(); err == nil {
		t.Fatal("unaffordable run once succeeded")
	}
	control, err := a.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Batch != nil {
		t.Fatalf("unaffordable request changed control: %+v, %v", control, err)
	}
	unchanged, _ := store.Get[model.Task](state, "task", task.ID)
	if unchanged.RunID != nil {
		t.Fatalf("unaffordable request tagged task with run %q", *unchanged.RunID)
	}

	cfg.MaxSessionsPerDay++
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	if err := a.RunOnce(); err != nil {
		t.Fatalf("affordable run once failed: %v", err)
	}
	started, err := a.Control()
	if err != nil || started.Batch == nil {
		t.Fatalf("affordable run once did not persist a batch: %+v, %v", started, err)
	}
	member, _ := store.Get[model.Task](state, "task", task.ID)
	if member.RunID == nil || *member.RunID != started.Batch.ID {
		t.Fatalf("original queued task is not a batch member: %+v", member.RunID)
	}
	later := queuedTask(cfg, "later", cfg.DefaultBranch, "octomus/later")
	if err := state.Put("task", later.ID, later); err != nil {
		t.Fatal(err)
	}
	laterSaved, _ := store.Get[model.Task](state, "task", later.ID)
	if laterSaved.RunID != nil {
		t.Fatal("task queued after RunOnce start joined the batch")
	}

	member.Status = model.StatusBlocked
	if err := state.Put("task", member.ID, *member); err != nil {
		t.Fatal(err)
	}
	member.Status = model.StatusQueued
	member.RunID = nil
	if err := state.Put("task", member.ID, *member); err != nil {
		t.Fatal(err)
	}
	pending, unresolved, err := state.BatchCounts(started.Batch.ID)
	if err != nil || unresolved != 1 {
		t.Fatalf("late retry erased batch failure: pending=%d unresolved=%d, %v", pending, unresolved, err)
	}
}

func TestUnaffordableAuditHasNoSideEffects(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = cfg.PlanningAdmissionsRequired() - 1
	original := model.DefaultControl()
	saveSettings(t, state, cfg, original)
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if _, err := a.StartAudit(context.Background()); err == nil {
		t.Fatal("unaffordable audit started")
	}
	control, err := a.Control()
	if err != nil || !controlsEqual(control, original) {
		t.Fatalf("unaffordable audit changed control: %+v, %v", control, err)
	}
	cycles, err := store.List[model.Cycle](state, "cycle")
	if err != nil || len(cycles) != 0 {
		t.Fatalf("unaffordable audit created a cycle: %d, %v", len(cycles), err)
	}
	used, err := state.SessionsToday()
	if err != nil || used != 0 {
		t.Fatalf("unaffordable audit consumed admissions: %d, %v", used, err)
	}
}

func TestExternalContextIsBoundedAndReportsCoverage(t *testing.T) {
	open := make([]model.PullRequest, 0, 105)
	for i := 105; i >= 1; i-- {
		open = append(open, model.PullRequest{Number: uint64(i), Title: strings.Repeat("t", 250), Body: strings.Repeat("b", 2200), Branch: fmt.Sprintf("branch-%d", i), Base: "main", State: "open"})
	}
	context, coverage, err := ExternalContext(model.OpenPrInventory{PRs: open})
	if err != nil {
		t.Fatal(err)
	}
	if len(context) != 100 || coverage.TotalOpen != 105 || coverage.IncludedExternal != 100 || coverage.OmittedExternal != 5 || !coverage.Complete {
		t.Fatalf("unexpected bounded coverage: %d %+v", len(context), coverage)
	}
	if context[0].Number != 1 || len([]rune(context[0].Title)) != 200 || len([]rune(context[0].Body)) != 2000 {
		t.Fatalf("context was not sorted/truncated: %+v", context[0])
	}
}

func TestPersistedPrInventoryIsNotProcessAuthority(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	inventory := model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: []model.PullRequest{ownedPR("octomus/existing")}}
	if persisted, err := state.PersistPrInventory(inventory, nil); err != nil || !persisted {
		t.Fatalf("persist inventory: %v, persisted=%t", err, persisted)
	}
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	capacity, err := a.PrCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Status != "unavailable" {
		t.Fatalf("restart trusted persisted capacity: %+v", capacity)
	}
	if capacity.OwnedOpen == nil || *capacity.OwnedOpen != 1 || capacity.Remaining != nil || capacity.ObservedAt == nil {
		t.Fatalf("unavailable report lost persisted observation evidence: %+v", capacity)
	}
	_, cancel := context.WithCancel(context.Background())
	a.runtimeMu.Lock()
	a.runtime.prRefresh = &prRefreshJob{cancel: cancel}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	cancel()
	a.runtimeMu.Lock()
	a.runtime.prRefresh = nil
	a.runtimeMu.Unlock()
	if err != nil || capacity.Status != "refreshing" || capacity.Remaining != nil {
		t.Fatalf("in-flight refresh was not reported fail-closed: %+v, %v", capacity, err)
	}
	a.runtimeMu.Lock()
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory, fetchedAt: time.Now().Add(-prObservationLifetime - time.Second)}
	a.runtimeMu.Unlock()
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" {
		t.Fatalf("stale process observation authorized capacity: %+v, %v", capacity, err)
	}
}

func TestCommittedRunOncePlanSurvivesRecovery(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = cfg.PlanningAdmissionsRequired()
	saveSettings(t, state, cfg, model.DefaultControl())
	a := New(state, t.TempDir())
	if err := a.RunOnce(); err != nil {
		t.Fatal(err)
	}
	control, err := a.Control()
	if err != nil || control.Batch == nil {
		t.Fatalf("missing run once batch: %+v, %v", control, err)
	}
	cycleID := model.ID()
	control.Batch.Phase = model.BatchPhasePlanning
	control.Batch.CycleID = &cycleID
	if err := state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	runID := control.Batch.ID
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: cycleID, Number: 1, Status: model.CycleCompleted,
		StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: cfg.GitHubRepo, DecisionMemory: []any{}, RunID: &runID,
	}
	planned := queuedTask(cfg, "planned", cfg.DefaultBranch, "octomus/planned")
	planned.CycleID = cycleID
	planned.RunID = &runID
	cycle.Proposals = []model.Proposal{planned.Proposal}
	if err := state.CommitPlan(cycle, []model.Task{planned}); err != nil {
		t.Fatal(err)
	}
	committed, _ := a.Control()
	if committed.Batch == nil || committed.Batch.Phase != model.BatchPhaseExecuting {
		t.Fatalf("plan did not atomically advance the batch: %+v", committed)
	}

	restarted := New(state, t.TempDir())
	t.Cleanup(restarted.Shutdown)
	if err := restarted.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered, _ := restarted.Control()
	if recovered.Mode != model.OperatingModeRunOnce || recovered.Batch == nil || recovered.Batch.Phase != model.BatchPhaseExecuting {
		t.Fatalf("recovery discarded committed executing phase: %+v", recovered)
	}
}

func TestRecoverySeedsOnlyResumableAndCheckpointedPrReservations(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	session := "execution-session"
	commit := "output-commit"

	initialized := queuedTask(cfg, "initialized", cfg.DefaultBranch, cfg.BranchPrefix+"initialized")
	initialized.ExecutionSession = &session
	initialized.Workspace = filepath.Join(t.TempDir(), "initialized")
	if err := os.MkdirAll(filepath.Join(initialized.Workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	invalidQueued := queuedTask(cfg, "invalid-queued", cfg.DefaultBranch, cfg.BranchPrefix+"invalid")
	invalidQueued.ExecutionSession = &session
	invalidQueued.Workspace = filepath.Join(t.TempDir(), "missing-checkout")
	doomedActive := queuedTask(cfg, "doomed-active", cfg.DefaultBranch, cfg.BranchPrefix+"doomed")
	doomedActive.Status = model.StatusExecuting
	cancelledCheckpoint := queuedTask(cfg, "cancelled-checkpoint", cfg.DefaultBranch, cfg.BranchPrefix+"cancelled")
	cancelledCheckpoint.Status = model.StatusCancelled
	cancelledCheckpoint.OutputCommit = &commit
	unresolvedCheckpoint := queuedTask(cfg, "unresolved-checkpoint", cfg.DefaultBranch, cfg.BranchPrefix+"checkpoint")
	unresolvedCheckpoint.Status = model.StatusBlocked
	unresolvedCheckpoint.OutputCommit = &commit
	resumableCheckpoint := queuedTask(cfg, "resumable-checkpoint", cfg.DefaultBranch, cfg.BranchPrefix+"resumable")
	resumableCheckpoint.Status = model.StatusExecuting
	resumableCheckpoint.OutputCommit = &commit
	resumableCheckpoint.ExecutionSession = &session
	resumableCheckpoint.Workspace = filepath.Join(t.TempDir(), "resumable")
	resumableCheckpoint.Sessions = []model.Session{model.NewSession("running", "executor", resumableCheckpoint.Route)}
	if err := os.MkdirAll(filepath.Join(resumableCheckpoint.Workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, task := range []model.Task{initialized, invalidQueued, doomedActive, cancelledCheckpoint, unresolvedCheckpoint, resumableCheckpoint} {
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.MarkCancel(resumableCheckpoint.ID); err != nil {
		t.Fatal(err)
	}

	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if err := a.Recover(); err != nil {
		t.Fatal(err)
	}
	reservations, err := state.PrReservations(cfg.GitHubRepo)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, reservation := range reservations {
		got[reservation.TaskID] = true
	}
	if !got[initialized.ID] || !got[unresolvedCheckpoint.ID] || !got[resumableCheckpoint.ID] || got[invalidQueued.ID] || got[doomedActive.ID] || got[cancelledCheckpoint.ID] {
		t.Fatalf("unexpected recovery reservations: %+v", got)
	}
	recovered, err := store.Get[model.Task](state, "task", resumableCheckpoint.ID)
	if err != nil || recovered == nil || recovered.Status != model.StatusQueued || recovered.Attempts != 1 || recovered.Sessions[0].Status != model.SessionInterrupted {
		t.Fatalf("checkpoint recovery lost resumable evidence: %+v, %v", recovered, err)
	}
}

func TestRecoveryReservationSeedingIsNotBoundedByDashboardWindows(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	for i := 0; i < 505; i++ {
		task := queuedTask(cfg, fmt.Sprintf("checkpoint-%03d", i), cfg.DefaultBranch, fmt.Sprintf("%scheckpoint-%03d", cfg.BranchPrefix, i))
		task.Status = model.StatusBlocked
		commit := fmt.Sprintf("commit-%03d", i)
		task.OutputCommit = &commit
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if err := a.Recover(); err != nil {
		t.Fatal(err)
	}
	reservations, err := state.PrReservations(cfg.GitHubRepo)
	if err != nil || len(reservations) != 505 {
		t.Fatalf("recovery seeded %d checkpoint reservations: %v", len(reservations), err)
	}
}

func TestInterruptedPlanningRunOncePauses(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = cfg.PlanningAdmissionsRequired()
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeRunOnce)
	cycleID := model.ID()
	control.Batch = &model.RunBatch{ID: model.ID(), Phase: model.BatchPhasePlanning, CycleID: &cycleID}
	saveSettings(t, state, cfg, control)
	cycle := model.Cycle{Mode: model.CycleModeExecution, ID: cycleID, Number: 1, Status: model.CycleRunning, StartedAt: model.Now(), Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{}, Repository: cfg.GitHubRepo}
	if err := state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if err := a.Recover(); err != nil {
		t.Fatal(err)
	}
	recovered, _ := a.Control()
	if recovered.Mode != model.OperatingModePaused || recovered.Batch != nil || recovered.Error == nil {
		t.Fatalf("interrupted planning was not paused explicitly: %+v", recovered)
	}
	savedCycle, _ := store.Get[model.Cycle](state, "cycle", cycleID)
	if savedCycle == nil || savedCycle.Status != model.CycleInterrupted {
		t.Fatalf("running cycle was not marked interrupted: %+v", savedCycle)
	}
}

func TestPlanningCapacityAfterDrainPausesRunOnceButContinuousWaits(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = cfg.PlanningAdmissionsRequired()
	saveSettings(t, state, cfg, model.DefaultControl())
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if err := a.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := state.ReserveSession(0, store.NewAdmission("earlier", nil, "discovery", cfg.Roles["discovery"])); err != nil {
		t.Fatal(err)
	}
	runControl, _ := a.Control()
	if err := a.maybePlan(cfg, runControl); err != nil {
		t.Fatal(err)
	}
	paused, _ := a.Control()
	if paused.Mode != model.OperatingModePaused || paused.Batch != nil || paused.Error == nil {
		t.Fatalf("drained unaffordable run once did not pause: %+v", paused)
	}
	cycles, err := store.List[model.Cycle](state, "cycle")
	if err != nil || len(cycles) != 0 {
		t.Fatalf("unaffordable run once created a cycle: %d, %v", len(cycles), err)
	}

	continuous := model.DefaultControl()
	continuous.SetMode(model.OperatingModeContinuous)
	if err := state.SaveControl(continuous); err != nil {
		t.Fatal(err)
	}
	if err := a.maybePlan(cfg, continuous); err != nil {
		t.Fatal(err)
	}
	waiting, _ := a.Control()
	if waiting.Mode != model.OperatingModeContinuous || waiting.NextCycleAt <= time.Now().Unix() || waiting.Error != nil {
		t.Fatalf("continuous mode did not wait for UTC budget reset: %+v", waiting)
	}
	cycles, _ = store.List[model.Cycle](state, "cycle")
	if len(cycles) != 0 {
		t.Fatalf("continuous budget wait created %d failed cycles", len(cycles))
	}
}

func TestRunOnceStopsWhenDrainBecomesUnresolvedDuringTick(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	task := queuedTask(cfg, "drain-failure", cfg.DefaultBranch, cfg.BranchPrefix+"failure")
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if err := a.RunOnce(); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil {
		t.Fatal(err)
	}
	task = *saved
	task.Proposal.Dependencies = []string{"missing"}
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	a.runtime.lastRetention = time.Now()
	a.runtime.lastObserve = time.Now()
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	control, err := a.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Batch != nil || control.Error == nil {
		t.Fatalf("failed drain proceeded to planning: %+v, %v", control, err)
	}
	cycles, err := store.List[model.Cycle](state, "cycle")
	if err != nil || len(cycles) != 0 {
		t.Fatalf("failed drain created a planning cycle: %+v, %v", cycles, err)
	}
	blocked, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || blocked == nil || blocked.Status != model.StatusBlocked {
		t.Fatalf("invalid draining task was not durably blocked: %+v, %v", blocked, err)
	}
}

func TestRunOnceBlocksDependencyRetriedOutsideBatch(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	runID := model.ID()
	dependency := queuedTask(cfg, "dependency", "octomus/existing", "octomus/existing")
	dependent := queuedTask(cfg, "dependent", "octomus/existing", "octomus/existing")
	dependent.Proposal.Dependencies = []string{dependency.ID}
	dependent.RunID = &runID
	if err := state.Put("task", dependency.ID, dependency); err != nil {
		t.Fatal(err)
	}
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeRunOnce)
	control.Batch = &model.RunBatch{ID: runID, Phase: model.BatchPhaseExecuting}
	saveSettings(t, state, cfg, control)
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	ready, blocked, err := a.dependenciesReady(dependent, control)
	if err != nil || ready || blocked == nil || !errors.Is(blocked, model.BlockedReasonDependencyBlocked) {
		t.Fatalf("out-of-batch retry did not block dependent: ready=%t blocked=%v err=%v", ready, blocked, err)
	}
}

func TestSchedulingKeepsExistingAndActiveWritersPastQueuedWindow(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	for i := 0; i < 501; i++ {
		task := queuedTask(cfg, fmt.Sprintf("new-%03d", i), cfg.DefaultBranch, fmt.Sprintf("octomus/new-%03d", i))
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	existing := queuedTask(cfg, "existing", "octomus/existing", "octomus/existing")
	active := queuedTask(cfg, "active", "octomus/active", "octomus/active")
	active.Status = model.StatusExecuting
	if err := state.Put("task", existing.ID, existing); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("task", active.ID, active); err != nil {
		t.Fatal(err)
	}
	tasks, err := state.SchedulingTasks(nil)
	if err != nil {
		t.Fatal(err)
	}
	foundExisting, foundActive, defaultQueued := false, false, 0
	for _, task := range tasks {
		foundExisting = foundExisting || task.ID == existing.ID
		foundActive = foundActive || task.ID == active.ID
		if task.Status == model.StatusQueued && task.Proposal.Target == cfg.DefaultBranch {
			defaultQueued++
		}
	}
	if !foundExisting || !foundActive || defaultQueued != 500 {
		t.Fatalf("bounded queue hid required work: existing=%t active=%t default=%d total=%d", foundExisting, foundActive, defaultQueued, len(tasks))
	}
}

func TestSchedulerSerializesWritersOnOneBranch(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.ExecutionConcurrency = 2
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = int64(^uint64(0) >> 1)
	saveSettings(t, state, cfg, control)
	first := queuedTask(cfg, "first", "octomus/existing", "octomus/existing")
	second := queuedTask(cfg, "second", "octomus/existing", "octomus/existing")
	second.Proposal.Dependencies = []string{first.ID}
	if err := state.Put("task", first.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("task", second.ID, second); err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 2)
	release := make(chan struct{})
	published := make(chan string, 2)
	runner := TaskRunnerFunc(func(_ context.Context, task model.Task) error {
		started <- task.ID
		current, err := store.Get[model.Task](state, "task", task.ID)
		if err != nil || current == nil {
			return fmt.Errorf("reload task: %v", err)
		}
		current.Status = model.StatusPublished
		current.UpdatedAt = model.Now()
		if err := state.Put("task", current.ID, *current); err != nil {
			return err
		}
		published <- task.ID
		<-release
		return nil
	})
	a := New(state, t.TempDir(), WithTaskRunner(runner))
	t.Cleanup(a.Shutdown)
	a.runtime.lastRetention = time.Now()
	a.runtime.lastObserve = time.Now()
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	if id := <-started; id != first.ID {
		t.Fatalf("started %s before prerequisite %s", id, first.ID)
	}
	select {
	case id := <-started:
		t.Fatalf("started same-branch writer concurrently: %s", id)
	default:
	}
	if id := <-published; id != first.ID {
		t.Fatalf("published unexpected task %s", id)
	}
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		t.Fatalf("started same-branch writer before the first runner exited: %s", id)
	default:
	}
	close(release)
	a.wg.Wait()
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	if id := <-started; id != second.ID {
		t.Fatalf("did not start dependent after publication: %s", id)
	}
	<-published
	a.wg.Wait()
}

func TestSchedulerCountsRunnerAfterTaskBecomesTerminal(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.ExecutionConcurrency = 1
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = int64(^uint64(0) >> 1)
	saveSettings(t, state, cfg, control)
	first := queuedTask(cfg, "first", "octomus/first", "octomus/first")
	second := queuedTask(cfg, "second", "octomus/second", "octomus/second")
	for _, task := range []model.Task{first, second} {
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan string, 2)
	published := make(chan string, 2)
	releaseFirst := make(chan struct{})
	runner := TaskRunnerFunc(func(_ context.Context, task model.Task) error {
		started <- task.ID
		current, err := store.Get[model.Task](state, "task", task.ID)
		if err != nil || current == nil {
			return fmt.Errorf("reload task: %v", err)
		}
		current.Status = model.StatusPublished
		current.UpdatedAt = model.Now()
		if err := state.Put("task", current.ID, *current); err != nil {
			return err
		}
		published <- task.ID
		if task.ID == first.ID {
			<-releaseFirst
		}
		return nil
	})
	a := New(state, t.TempDir(), WithTaskRunner(runner))
	t.Cleanup(a.Shutdown)
	a.runtime.lastRetention = time.Now()
	a.runtime.lastObserve = time.Now()
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	if id := <-started; id != first.ID {
		t.Fatalf("started %s before %s", id, first.ID)
	}
	if id := <-published; id != first.ID {
		t.Fatalf("published unexpected task %s", id)
	}
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-started:
		t.Fatalf("started %s while the first runner still owned the only slot", id)
	default:
	}
	close(releaseFirst)
	a.wg.Wait()
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	if id := <-started; id != second.ID {
		t.Fatalf("started %s after releasing %s", id, first.ID)
	}
	<-published
	a.wg.Wait()
}

func TestRunnerExitBlocksStillActiveTask(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	task := queuedTask(cfg, "unfinished", "octomus/existing", "octomus/existing")
	task.Status = model.StatusExecuting
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	a := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
		return nil
	})))
	t.Cleanup(a.Shutdown)
	a.runTask(task)
	a.wg.Wait()

	saved, err := store.Get[model.Task](state, "task", task.ID)
	if err != nil || saved == nil {
		t.Fatalf("reload task: %+v, %v", saved, err)
	}
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonUnknown {
		t.Fatalf("runner exit did not block active task: %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, "exited unexpectedly") {
		t.Fatalf("runner exit did not preserve a useful error: %+v", saved.Error)
	}
	a.runtimeMu.Lock()
	running := len(a.runtime.tasks)
	a.runtimeMu.Unlock()
	if running != 0 {
		t.Fatalf("runner retained %d runtime slot(s) after fallback", running)
	}
}

func TestSchedulerWaitsForExecutionSlotBeforeRefreshingCapacity(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.ExecutionConcurrency = 1
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	saveSettings(t, state, cfg, control)
	active := queuedTask(cfg, "active", cfg.DefaultBranch, cfg.BranchPrefix+"active")
	active.Status = model.StatusExecuting
	queued := queuedTask(cfg, "queued", cfg.DefaultBranch, cfg.BranchPrefix+"queued")
	for _, task := range []model.Task{active, queued} {
		if err := state.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	calls := make(chan struct{}, 1)
	a := New(state, t.TempDir(), WithTaskRunner(TaskRunnerFunc(func(context.Context, model.Task) error {
		calls <- struct{}{}
		return nil
	})))
	t.Cleanup(a.Shutdown)
	a.runtime.lastRetention = time.Now()
	a.runtime.lastObserve = time.Now()
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	a.runtimeMu.Lock()
	refreshing := a.runtime.prRefresh != nil
	a.runtimeMu.Unlock()
	called := false
	select {
	case <-calls:
		called = true
	default:
	}
	if refreshing || called {
		t.Fatalf("capacity refresh or execution started without a free slot: refreshing=%t called=%t", refreshing, called)
	}
	saved, err := store.Get[model.Task](state, "task", queued.ID)
	if err != nil || saved.Status != model.StatusQueued {
		t.Fatalf("queued work changed while execution was full: %+v, %v", saved, err)
	}
}

func TestDecisionMemoryAbsorbsOnlySameCycleAlternativesAndRequiresRediscovery(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	accepted := proposal("accepted", cfg.DefaultBranch)
	accepted.ProblemKey = "stable-problem"
	accepted.RelevantPaths = nil
	rejected := accepted
	rejected.ID = "alternative"
	rejected.Decision = model.DecisionRejected
	rejected.Reason = "Absorbed by the accepted scope"
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: model.ID(), Grounding: &model.Grounding{Revision: "revision"},
		Proposals: []model.Proposal{accepted, rejected}, Repository: cfg.GitHubRepo,
	}
	records, err := a.recordDecisions(context.Background(), cfg, cycle)
	if err != nil || len(records) != 1 {
		t.Fatalf("same-cycle alternative was not absorbed: %d, %v", len(records), err)
	}
	stored := records[0].(map[string]any)
	paths, pathsOK := stored["relevant_paths"].([]string)
	if stored["mode"] != model.CycleModeExecution || stored["cycle_mode"] != nil || stored["kind"] != nil || stored["reconsideration_due"] != nil || !pathsOK || paths == nil {
		t.Fatalf("decision record is not Rust-compatible: %+v", stored)
	}
	recorded := recordToMap(decisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeExecution,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionRejected, Reason: "Current decision", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	})
	if err := ValidateDecisionMemory([]model.Proposal{accepted}, []any{recorded}); err == nil {
		t.Fatal("unchanged rejected work became executable without rediscovery")
	}
	auditRecommendation := recordToMap(decisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeAudit,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionAccepted, Reason: "Audit recommendation", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	})
	if err := ValidateDecisionMemory([]model.Proposal{accepted}, []any{auditRecommendation}); err != nil {
		t.Fatalf("audit recommendation incorrectly vetoed execution: %v", err)
	}
	requestID := model.ID()
	request := map[string]any{"kind": "rediscovery", "id": requestID, "target": cfg.DefaultBranch}
	reconsidered := accepted.Clone()
	reconsidered.Reconsiders = []string{requestID}
	if err := ValidateDecisionMemory([]model.Proposal{reconsidered}, []any{recorded, request}); err != nil {
		t.Fatalf("matching explicit rediscovery was rejected: %v", err)
	}
	wrong := reconsidered.Clone()
	wrong.Target = "other"
	if err := ValidateDecisionMemory([]model.Proposal{wrong}, []any{request}); err == nil {
		t.Fatal("rediscovery with the wrong target was accepted")
	}
	oversized := accepted.Clone()
	oversized.ProblemKey = strings.Repeat("x", 201)
	if err := ValidateDecisionMemory([]model.Proposal{oversized}, nil); err == nil {
		t.Fatal("oversized decision metadata was accepted")
	}
}

func TestDecisionMemoryUsesPrRevisionAndNormalizesLegacyAlternatives(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	pr := ownedPR("octomus/existing")
	pr.Head = "pr-head"
	accepted := proposal("accepted", pr.Branch)
	accepted.ProblemKey = "stable-problem"
	accepted.RelevantPaths = nil
	cycleID := model.ID()
	cycle := model.Cycle{Mode: model.CycleModeExecution, ID: cycleID, Grounding: &model.Grounding{Revision: "main-head", PRs: []model.PullRequest{pr}}, Proposals: []model.Proposal{accepted}, Repository: cfg.GitHubRepo}
	records, err := a.recordDecisions(context.Background(), cfg, cycle)
	if err != nil || len(records) != 1 || records[0].(map[string]any)["source_revision"] != pr.Head || records[0].(map[string]any)["context_fingerprint"] != pr.Head {
		t.Fatalf("existing-PR decision used the wrong revision: %+v, %v", records, err)
	}
	acceptedRecord := decisionRecord{Kind: "decision", ID: "legacy-accepted", CycleMode: model.CycleModeAudit, Repository: cfg.GitHubRepo, Target: pr.Branch, ProblemKey: accepted.ProblemIdentity(), Decision: model.DecisionAccepted, Reason: "scope", SourceRevision: pr.Head, ContextFingerprint: pr.Head, ReconsiderAfter: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), CycleID: cycleID}
	rejectedRecord := acceptedRecord
	rejectedRecord.ID = "legacy-rejected"
	rejectedRecord.Decision = model.DecisionRejected
	for _, record := range []decisionRecord{acceptedRecord, rejectedRecord} {
		if err := state.Put("decision", record.ID, recordToMap(record)); err != nil {
			t.Fatal(err)
		}
	}
	memory, err := a.planningMemory(context.Background(), cfg, *cycle.Grounding)
	if err != nil || len(memory) != 1 {
		t.Fatalf("legacy absorbed decision was retained: %+v, %v", memory, err)
	}
	if got := memory[0].(map[string]any)["mode"]; got != model.CycleModeAudit {
		t.Fatalf("Rust decision mode was not preserved: %v", got)
	}
}

func TestPrCapacityUsesCompleteInventoryAndUnrepresentedReservations(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxOpenPRs = 3
	saveSettings(t, state, cfg, model.DefaultControl())
	reserved := queuedTask(cfg, "reserved", cfg.DefaultBranch, "octomus/reserved")
	if err := state.Put("task", reserved.ID, reserved); err != nil {
		t.Fatal(err)
	}
	if err := state.SeedPrReservation(reserved); err != nil {
		t.Fatal(err)
	}
	inventory := model.OpenPrInventory{Repository: cfg.GitHubRepo, ObservedAt: model.Now(), PRs: []model.PullRequest{ownedPR("octomus/existing")}}
	if persisted, err := state.PersistPrInventory(inventory, nil); err != nil || !persisted {
		t.Fatalf("persist inventory: %t, %v", persisted, err)
	}
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory, fetchedAt: time.Now()}
	capacity, err := a.PrCapacity()
	if err != nil || capacity.Status != "ready" || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 1 || capacity.Reserved != 1 || capacity.Remaining == nil || *capacity.Remaining != 1 {
		t.Fatalf("inventory/reservation union was wrong: %+v, %v", capacity, err)
	}
	represented := ownedPR(reserved.Branch)
	represented.Number = 2
	inventory.ObservedAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	inventory.PRs = append(inventory.PRs, represented)
	if persisted, err := state.PersistPrInventory(inventory, nil); err != nil || !persisted {
		t.Fatalf("persist represented inventory: %t, %v", persisted, err)
	}
	a.runtime.prObservation = &freshPrObservation{identity: store.PrIdentityOf(cfg), inventory: inventory, fetchedAt: time.Now()}
	capacity, err = a.PrCapacity()
	if err != nil || capacity.OwnedOpen == nil || *capacity.OwnedOpen != 2 || capacity.Reserved != 0 || capacity.Remaining == nil || *capacity.Remaining != 1 {
		t.Fatalf("represented reservation was double-counted: %+v, %v", capacity, err)
	}
	changed := cfg.Clone()
	changed.GitHubRepo = "fixture/changed"
	if err := state.Put("settings", "config", changed); err != nil {
		t.Fatal(err)
	}
	capacity, err = a.PrCapacity()
	if err != nil || capacity.Status != "unavailable" {
		t.Fatalf("changed policy retained stale capacity authority: %+v, %v", capacity, err)
	}
}

func TestPausedHousekeepingPreservesUnresolvedEvidenceAndRejectsSymlink(t *testing.T) {
	state := testStore(t)
	dataDir := t.TempDir()
	cfg := testConfig(t.TempDir())
	cfg.RetainCompletedDays = 1
	saveSettings(t, state, cfg, model.DefaultControl())
	tasksRoot := filepath.Join(dataDir, "tasks")
	if err := os.MkdirAll(tasksRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "2020-01-01T00:00:00Z"
	unresolved := queuedTask(cfg, "unresolved", cfg.DefaultBranch, "octomus/unresolved")
	unresolved.Status = model.StatusBlocked
	unresolved.UpdatedAt = old
	unresolvedOwner := filepath.Join(tasksRoot, unresolved.ID)
	unresolved.Workspace = filepath.Join(unresolvedOwner, "workspace")
	if err := os.MkdirAll(unresolved.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unresolved.Workspace, "evidence.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("task", unresolved.ID, unresolved); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "evidence.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	published := queuedTask(cfg, "published", cfg.DefaultBranch, "octomus/published")
	published.Status = model.StatusPublished
	published.UpdatedAt = old
	publishedOwner := filepath.Join(tasksRoot, published.ID)
	published.Workspace = filepath.Join(publishedOwner, "workspace")
	if err := os.Symlink(external, publishedOwner); err != nil {
		t.Fatal(err)
	}
	if err := state.Put("task", published.ID, published); err != nil {
		t.Fatal(err)
	}
	a := New(state, dataDir)
	t.Cleanup(a.Shutdown)
	a.runtime.lastObserve = time.Now()
	if err := a.Tick(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.runtimeMu.Lock()
		running := a.runtime.housekeeping
		a.runtimeMu.Unlock()
		if !running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(unresolved.Workspace, "evidence.txt")); err != nil {
		t.Fatalf("paused housekeeping removed unresolved evidence: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(external, "evidence.txt")); err != nil || string(data) != "outside" {
		t.Fatalf("symlink cleanup escaped owned root: %q, %v", data, err)
	}
	saved, err := store.Get[model.Task](state, "task", published.ID)
	if err != nil || saved == nil || saved.Lifecycle.DiscardedAt != nil {
		t.Fatalf("rejected cleanup was recorded as successful: %+v, %v", saved, err)
	}
}

// F3 (review_findings.rs): same-cycle proposals sharing a problem key are
// duplicates regardless of wording.
func TestSameCycleProposalsSharingAProblemKeyAreDuplicates(t *testing.T) {
	cfg := testConfig(t.TempDir())
	grounding := model.Grounding{Revision: "rev"}
	first := proposal("a", cfg.DefaultBranch)
	first.ProblemKey = "parser:truncated-frame"
	second := proposal("b", cfg.DefaultBranch)
	second.Title = "Validate the complete frame length"
	second.ProblemKey = "parser:truncated-frame"
	if err := ValidateProposals(cfg, []model.Proposal{first, second}, grounding, nil); err == nil || !strings.Contains(err.Error(), "Duplicate accepted") {
		t.Fatalf("shared problem key was not rejected: %v", err)
	}
	other := second.Clone()
	other.ProblemKey = "parser:length-header"
	if err := ValidateProposals(cfg, []model.Proposal{first, other}, grounding, nil); err != nil {
		t.Fatalf("distinct problem keys rejected: %v", err)
	}
}

// F4 (review_findings.rs): target resolution binds the owned PR regardless of
// order, rejects unowned and ambiguous matches, and never binds the default
// branch as a PR.
func TestTargetResolutionBindsTheOwnedPRRegardlessOfOrder(t *testing.T) {
	cfg := testConfig(t.TempDir())
	fork := ownedPR("octomus/fix")
	fork.Number, fork.Head, fork.Owned, fork.HeadRepository = 202, "fork-head", false, "fork/project"
	owned := ownedPR("octomus/fix")
	owned.Number, owned.Head = 101, "repo-head"
	prs := []model.PullRequest{fork, owned}
	bound, err := ResolveTarget(cfg, prs, "octomus/fix")
	if err != nil || bound == nil || bound.Number != 101 || bound.Head != "repo-head" {
		t.Fatalf("bound = %+v, %v; want owned PR 101", bound, err)
	}
	if target, err := ResolveTarget(cfg, prs, cfg.DefaultBranch); err != nil || target != nil {
		t.Fatalf("default branch resolved to a PR: %+v, %v", target, err)
	}
	if _, err := ResolveTarget(cfg, prs[:1], "octomus/fix"); err == nil {
		t.Fatal("fork-only target resolved")
	}
	if _, err := ResolveTarget(cfg, []model.PullRequest{owned, owned}, "octomus/fix"); err == nil {
		t.Fatal("ambiguous owned match resolved")
	}
	p := proposal("a", "octomus/fix")
	grounding := model.Grounding{Revision: "rev", PRs: prs}
	if err := ValidateProposals(cfg, []model.Proposal{p}, grounding, nil); err != nil {
		t.Fatalf("owned-PR target rejected: %v", err)
	}
}
