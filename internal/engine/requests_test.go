package engine

// Operator work requests: validation, guarded withdrawal, planning seeding and settlement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func requestFixture(t *testing.T, f *fixture, repo, target, title string) model.OperatorRequest {
	t.Helper()
	now := model.Now()
	r := model.OperatorRequest{
		ID: model.ID(), Repository: repo, Title: title, Description: "Describe " + title,
		Target: target, Status: model.RequestPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.state.CreateRequest(r, model.RequestPendingLimit); err != nil {
		t.Fatal(err)
	}
	return r
}

func requestCandidate(r model.OperatorRequest, decision string) model.Proposal {
	p := proposal(r.ProposalID(), r.Target)
	p.Title = r.Title
	p.Decision = decision
	p.Reason = "Repository evidence supports the operator request."
	return p
}

// withRequests scripts the reviewers to assess and the consolidation to return each
// given request candidate under its own decision.
func withRequests(t *testing.T, plan scriptedPlan, candidates []model.Proposal) scriptedPlan {
	t.Helper()
	for i := range plan.reviews {
		var doc map[string]any
		must0(t, json.Unmarshal([]byte(plan.reviews[i].Answer), &doc))
		assessments := doc["assessments"].([]any)
		for _, c := range candidates {
			assessments = append(assessments, map[string]any{"id": c.ID, "decision": c.Decision, "reason": "Assessed " + c.Decision + "."})
		}
		doc["assessments"] = assessments
		plan.reviews[i].Answer = mustJSON(t, doc)
	}
	var doc map[string]any
	must0(t, json.Unmarshal([]byte(plan.consolidation.Answer), &doc))
	proposals := doc["proposals"].([]any)
	for _, c := range candidates {
		proposals = append(proposals, c)
	}
	doc["proposals"] = proposals
	plan.consolidation.Answer = mustJSON(t, doc)
	return plan
}

func savedRequest(t *testing.T, f *fixture, id string) model.OperatorRequest {
	t.Helper()
	r, err := store.Get(f.state, store.Requests.Key(id))
	if err != nil || r == nil {
		t.Fatalf("request %s: %v, %v", id, r, err)
	}
	return *r
}

func consolidationTurn(t *testing.T, f *fixture) string {
	t.Helper()
	for _, turn := range f.script.Turns(f.routes.Orchestrator) {
		if strings.HasPrefix(turn.Prompt, "Act as final orchestrator") {
			return turn.Prompt
		}
	}
	t.Fatal("no consolidation turn")
	return ""
}

func TestOperatorRequestPromptsAndContext(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Add the requested feature")
	candidate := requestCandidate(r, model.DecisionDeferred)
	candidate.Reason = "Wait for more evidence."
	plan := withRequests(t, completePlan(t, f), []model.Proposal{candidate})
	plan.queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, app, id)
	if cycle.Status != model.CycleCompleted {
		t.Fatalf("cycle %+v", cycle)
	}
	reviewers := f.script.Turns(f.routes.ProposalReviewer)
	if len(reviewers) != 2 {
		t.Fatalf("reviewer turns: %d", len(reviewers))
	}
	for _, turn := range reviewers {
		if !strings.Contains(turn.Prompt, r.ProposalID()) || !strings.Contains(turn.Prompt, "Candidates whose IDs start with request-") {
			t.Fatalf("reviewer prompt missing request context:\n%.400s", turn.Prompt)
		}
	}
	prompt := consolidationTurn(t, f)
	if !strings.Contains(prompt, r.ProposalID()) || !strings.Contains(prompt, "Return each operator request candidate") {
		t.Fatalf("consolidation prompt missing request context:\n%.400s", prompt)
	}
	for i, turn := range f.script.Turns(f.routes.Discovery) {
		if !strings.Contains(turn.Prompt, `"operator_requests"`) || !strings.Contains(turn.Prompt, "do not duplicate them") {
			t.Fatalf("discovery %d prompt missing request context:\n%.400s", i, turn.Prompt)
		}
		if !strings.Contains(turn.Prompt, r.ID) {
			t.Fatalf("discovery %d operator_requests context missing the request:\n%.400s", i, turn.Prompt)
		}
	}
}

func TestOperatorRequestExecutionSettlement(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"test -f feature.txt"} })
	r := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Add the requested feature")
	plan := withRequests(t, completePlan(t, f), []model.Proposal{requestCandidate(r, model.DecisionAccepted)})
	plan.consolidation.Answer = mustJSON(t, map[string]any{"proposals": []any{
		fixtureProposal("rejected", "Absorbed into the operator request."),
		requestCandidate(r, model.DecisionAccepted),
	}})
	plan.queue(f)
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("Complete"))
	app := f.pausedApp(t)
	if err := control(app, model.ControlCycle); err != nil {
		t.Fatal(err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	cycle := waitOnlyCycle(t, app)
	if cycle.Status != model.CycleCompleted {
		t.Fatalf("cycle %+v", cycle)
	}
	saved := savedRequest(t, f, r.ID)
	if saved.Status != model.RequestAccepted || saved.TaskID == nil || saved.Assessment == nil ||
		saved.Assessment.Mode != model.CycleModeExecution || saved.Assessment.Decision != model.DecisionAccepted {
		t.Fatalf("settled request %+v", saved)
	}
	task := driveTask(t, f, app, *saved.TaskID)
	if task.Proposal.ID != r.ProposalID() || task.Status != model.StatusPublished {
		t.Fatalf("request task %+v", task)
	}
}

func TestOperatorRequestRejectedAndDeferred(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rejected := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Rejected work")
	deferred := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Deferred work")
	plan := withRequests(t, completePlan(t, f), []model.Proposal{requestCandidate(rejected, model.DecisionRejected), requestCandidate(deferred, model.DecisionDeferred)})
	plan.queue(f)
	app := f.pausedApp(t)
	if err := control(app, model.ControlCycle); err != nil {
		t.Fatal(err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	cycle := waitOnlyCycle(t, app)
	if cycle.Status != model.CycleCompleted {
		t.Fatalf("cycle %+v", cycle)
	}
	saved := savedRequest(t, f, rejected.ID)
	if saved.Status != model.RequestRejected || saved.Assessment == nil || saved.Assessment.Decision != model.DecisionRejected {
		t.Fatalf("rejected %+v", saved)
	}
	saved = savedRequest(t, f, deferred.ID)
	if saved.Status != model.RequestPending || saved.Assessment == nil || saved.Assessment.Decision != model.DecisionDeferred {
		t.Fatalf("deferred %+v", saved)
	}
}

func TestOperatorRequestAuditAssessesOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Assessed work")
	plan := withRequests(t, completePlan(t, f), []model.Proposal{requestCandidate(r, model.DecisionAccepted)})
	plan.queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, app, id)
	if cycle.Status != model.CycleCompleted {
		t.Fatalf("cycle %+v", cycle)
	}
	saved := savedRequest(t, f, r.ID)
	if saved.Status != model.RequestPending || saved.TaskID != nil || saved.Assessment == nil ||
		saved.Assessment.Mode != model.CycleModeAudit || saved.Assessment.Decision != model.DecisionAccepted {
		t.Fatalf("audit settled %+v", saved)
	}
	var count int64
	if err := f.rawDB(t).QueryRow("SELECT count(*) FROM records WHERE kind='task'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("audit created tasks: %d, %v", count, err)
	}
}

func TestOperatorRequestIneligibleTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := requestFixture(t, f, f.cfg.GitHubRepo, "octomus/gone", "Aimed at a closed PR")
	completePlan(t, f).queue(f)
	app := f.pausedApp(t)
	if err := control(app, model.ControlCycle); err != nil {
		t.Fatal(err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	cycle := waitOnlyCycle(t, app)
	if cycle.Status != model.CycleCompleted {
		t.Fatalf("cycle %+v", cycle)
	}
	if strings.Contains(consolidationTurn(t, f), r.ProposalID()) {
		t.Fatal("ineligible request was seeded as a candidate")
	}
	saved := savedRequest(t, f, r.ID)
	if saved.Status != model.RequestRejected || saved.Assessment == nil ||
		saved.Assessment.Decision != model.DecisionRejected ||
		!strings.Contains(saved.Assessment.Reason, "no longer an owned open pull request or the default branch") {
		t.Fatalf("ineligible %+v", saved)
	}
}

func TestOperatorRequestTargetDriftFailsCycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	existingPRBranch(t, f)
	r := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Redirected work")
	moved := requestCandidate(r, model.DecisionAccepted)
	moved.Target = "octomus/existing"
	plan := withRequests(t, completePlan(t, f), []model.Proposal{moved})
	plan.queue(f)
	app := f.pausedApp(t)
	if err := control(app, model.ControlCycle); err != nil {
		t.Fatal(err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	cycle := waitOnlyCycle(t, app)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, r.ID) {
		t.Fatalf("cycle %+v", cycle)
	}
	saved := savedRequest(t, f, r.ID)
	if saved.Status != model.RequestPending || saved.Assessment != nil {
		t.Fatalf("failed cycle settled %+v", saved)
	}
}

func TestDiscoveryReservedRequestIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plan := completePlan(t, f)
	forged := fixtureProposal("candidate", "Delivers the documented feature.")
	forged["id"] = "request-forged"
	plan.discovery[0].Answer = mustJSON(t, map[string]any{"proposals": []any{forged}})
	plan.queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, app, id)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, `reserved proposal identity "request-forged"`) {
		t.Fatalf("cycle %+v", cycle)
	}
}

func TestOperatorRequestDecisionMemoryExemption(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	revision := git(t, f.repo, "rev-parse", "HEAD")
	r := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Requested feature")
	candidate := requestCandidate(r, model.DecisionAccepted)
	old := savedDecision(f.cfg.GitHubRepo, f.cfg.DefaultBranch, "old-rejection", candidate.ProblemIdentity(), revision)
	if err := store.Put(f.state, store.Decisions.Key(old.ID), old); err != nil {
		t.Fatal(err)
	}
	plan := withRequests(t, completePlan(t, f), []model.Proposal{candidate})
	plan.queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, app, id)
	if cycle.Status != model.CycleCompleted {
		t.Fatalf("request was blocked by a current rejection: %+v", cycle)
	}
}

func TestNonRequestRepeatStillFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	revision := git(t, f.repo, "rev-parse", "HEAD")
	old := savedDecision(f.cfg.GitHubRepo, f.cfg.DefaultBranch, "still-current", model.ProblemIdentity("Complete the fixture feature", ""), revision)
	if err := store.Put(f.state, store.Decisions.Key(old.ID), old); err != nil {
		t.Fatal(err)
	}
	completePlan(t, f).queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, app, id)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, `decision "still-current"`) {
		t.Fatalf("cycle %+v", cycle)
	}
}

func TestOperatorRequestSeedCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ids := []string{}
	for i := 0; i < 12; i++ {
		ids = append(ids, requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, fmt.Sprintf("Work %d", i)).ID)
	}
	other := requestFixture(t, f, "another/repo", f.cfg.DefaultBranch, "Elsewhere")
	candidates := []model.Proposal{}
	for _, id := range ids[:model.RequestSeedLimit] {
		candidates = append(candidates, requestCandidate(savedRequest(t, f, id), model.DecisionDeferred))
	}
	plan := withRequests(t, completePlan(t, f), candidates)
	plan.queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, app, id)
	if cycle.Status != model.CycleCompleted {
		t.Fatalf("cycle %+v", cycle)
	}
	prompt := consolidationTurn(t, f)
	seeded := 0
	for _, rid := range ids {
		if strings.Contains(prompt, "request-"+rid) {
			seeded++
		}
	}
	if seeded != model.RequestSeedLimit || strings.Contains(prompt, "request-"+other.ID) {
		t.Fatalf("seeded %d candidates, foreign request seeded %v", seeded, strings.Contains(prompt, "request-"+other.ID))
	}
	pending, err := f.state.PendingRequests(f.cfg.GitHubRepo, model.RequestPendingLimit)
	if err != nil {
		t.Fatal(err)
	}
	assessed := 0
	for _, p := range pending {
		if p.Assessment != nil {
			assessed++
		}
	}
	if len(pending) != 12 || assessed != model.RequestSeedLimit {
		t.Fatalf("pending %d assessed %d", len(pending), assessed)
	}
}

func TestRequestActionsAndValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	app := f.pausedApp(t)
	if _, err := app.CreateRequest(RequestInput{Title: "T", Description: "D", Target: "main"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, tc := range []RequestInput{
		{Title: "", Description: "D", Target: "main"},
		{Title: strings.Repeat("x", 201), Description: "D", Target: "main"},
		{Title: "bad\x07title", Description: "D", Target: "main"},
		{Title: "T", Description: "", Target: "main"},
		{Title: "T", Description: strings.Repeat("x", 8001), Target: "main"},
		{Title: "T", Description: "bad\rdescription", Target: "main"},
		{Title: "T", Description: "D", Target: "feature/not-owned"},
		{Title: "T", Description: "D", Target: "octomus/../escape"},
	} {
		if _, err := app.CreateRequest(tc); err == nil {
			t.Fatalf("invalid input accepted: %+v", tc)
		}
	}
	f.configure(t, func(cfg *config.Config) { cfg.GitHubRepo = "" })
	if _, err := app.CreateRequest(RequestInput{Title: "T", Description: "D", Target: "main"}); err == nil || !IsActionConflict(err) {
		t.Fatalf("unconfigured create: %v", err)
	}
	f.configure(t, func(cfg *config.Config) { cfg.GitHubRepo = "fixture/project" })
	for i := 1; i < model.RequestPendingLimit; i++ {
		if _, err := app.CreateRequest(RequestInput{Title: "T", Description: "D", Target: "main"}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if _, err := app.CreateRequest(RequestInput{Title: "T", Description: "D", Target: "main"}); err == nil || !IsActionConflict(err) {
		t.Fatalf("limit create: %v", err)
	}
}

func TestRequestWithdrawGuards(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := requestFixture(t, f, f.cfg.GitHubRepo, f.cfg.DefaultBranch, "Withdraw me")
	app := f.pausedApp(t)
	app.runtimeMu.Lock()
	mode := model.CycleModeExecution
	app.runtime.preflight = &mode
	app.runtimeMu.Unlock()
	err := app.RequestAction(r.ID, model.RequestActionWithdraw)
	if err == nil || !IsActionConflict(err) || ActionCode(err) != ActiveWorkConflict {
		t.Fatalf("withdraw during planning: %v", err)
	}
	app.runtimeMu.Lock()
	app.runtime.preflight = nil
	app.runtimeMu.Unlock()
	if saved := savedRequest(t, f, r.ID); saved.Status != model.RequestPending {
		t.Fatalf("refused withdraw changed status: %+v", saved)
	}
	if err := app.RequestAction(r.ID, model.RequestActionWithdraw); err != nil {
		t.Fatalf("idle withdraw: %v", err)
	}
	if saved := savedRequest(t, f, r.ID); saved.Status != model.RequestWithdrawn {
		t.Fatalf("withdraw: %+v", saved)
	}
	if err := app.RequestAction(r.ID, model.RequestActionWithdraw); err == nil || !IsActionConflict(err) {
		t.Fatalf("withdraw non-pending: %v", err)
	}
	var notFound NotFound
	if err := app.RequestAction("missing", model.RequestActionWithdraw); !errors.As(err, &notFound) {
		t.Fatalf("missing request: %v", err)
	}
	if err := app.RequestAction(r.ID, model.RequestAction(7)); err == nil {
		t.Fatal("unknown action accepted")
	}
}
