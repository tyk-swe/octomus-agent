package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	return openStore(t, t.TempDir())
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	state, err := store.Open(filepath.Join(dir, "state.db"))
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

func deferHousekeeping(app *App) {
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
}

func TestProposalValidationRequiresKnownAcyclicOrderedDependencies(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestUnaffordableAuditHasNoSideEffects(t *testing.T) {
	t.Parallel()
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
	if err != nil || !reflect.DeepEqual(control, original) {
		t.Fatalf("unaffordable audit changed control: %+v, %v", control, err)
	}
	cycles, err := store.List[model.Cycle](state, "cycle")
	if err != nil || len(cycles) != 0 {
		t.Fatalf("unaffordable audit created a cycle: %d, %v", len(cycles), err)
	}
	assertAdmissions(t, state, 0, "unaffordable audit")
}

func TestSchedulerSerializesWritersOnOneBranch(t *testing.T) {
	t.Parallel()
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
	deferHousekeeping(a)
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

func TestDecisionMemoryAbsorbsOnlySameCycleAlternativesAndRequiresRediscovery(t *testing.T) {
	t.Parallel()
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
		t.Fatalf("decision record changed: %+v", stored)
	}
	recorded := decisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeExecution,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionRejected, Reason: "Current decision", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	}
	if err := validateDecisionMemory([]model.Proposal{accepted}, decisionMemory{decisions: []decisionRecord{recorded}}); err == nil {
		t.Fatal("unchanged rejected work became executable without rediscovery")
	}
	auditRecommendation := decisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeAudit,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionAccepted, Reason: "Audit recommendation", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	}
	if err := validateDecisionMemory([]model.Proposal{accepted}, decisionMemory{decisions: []decisionRecord{auditRecommendation}}); err != nil {
		t.Fatalf("audit recommendation incorrectly vetoed execution: %v", err)
	}
	requestID := model.ID()
	request := rediscoveryRequest{ID: requestID, Target: cfg.DefaultBranch}
	reconsidered := accepted.Clone()
	reconsidered.Reconsiders = []string{requestID}
	if err := validateDecisionMemory([]model.Proposal{reconsidered}, decisionMemory{decisions: []decisionRecord{recorded}, requests: []rediscoveryRequest{request}}); err != nil {
		t.Fatalf("matching explicit rediscovery was rejected: %v", err)
	}
	wrong := reconsidered.Clone()
	wrong.Target = "other"
	if err := validateDecisionMemory([]model.Proposal{wrong}, decisionMemory{requests: []rediscoveryRequest{request}}); err == nil {
		t.Fatal("rediscovery with the wrong target was accepted")
	}
	oversized := accepted.Clone()
	oversized.ProblemKey = strings.Repeat("x", 201)
	if err := validateDecisionMemory([]model.Proposal{oversized}, decisionMemory{}); err == nil {
		t.Fatal("oversized decision metadata was accepted")
	}
}

func TestSameCycleProposalsSharingAProblemKeyAreDuplicates(t *testing.T) {
	t.Parallel()
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
