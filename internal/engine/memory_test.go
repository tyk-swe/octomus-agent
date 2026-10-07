package engine

// Decision memory: reconsideration, absorption and rediscovery requests.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestDecisionMemoryReconsideration(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := f.cfg
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(cfg.Repository, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Repository, "docs", "guide.md"), []byte("# Guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, cfg.Repository, "add", "docs/guide.md")
	git(t, cfg.Repository, "commit", "-m", "Add the guide")
	recorded := git(t, cfg.Repository, "rev-parse", "HEAD")

	future := time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	decision := func(id, target, problem, verdict, cycleID, after string, paths ...string) {
		t.Helper()
		if paths == nil {
			paths = []string{}
		}
		fingerprint, err := decisionFingerprint(ctx, cfg, recorded, paths)
		if err != nil {
			t.Fatal(err)
		}
		record := map[string]any{
			"id": id, "mode": "execution", "repository": cfg.GitHubRepo, "target": target,
			"problem_key": problem, "relevant_paths": paths, "decision": verdict,
			"reason": "Recorded reason", "source_revision": recorded,
			"context_fingerprint": fingerprint, "reconsider_after": after, "cycle_id": cycleID,
		}
		if err := f.state.Put("decision", id, record); err != nil {
			t.Fatal(err)
		}
	}
	decision("readme", "main", "readme-problem", model.DecisionRejected, "cycle-1", future, "README.md")
	decision("guide", "main", "guide-problem", model.DecisionRejected, "cycle-1", future, "docs/guide.md")
	decision("whole-tree", "main", "tree-problem", model.DecisionDeferred, "cycle-1", future)
	decision("expired", "main", "expired-problem", model.DecisionRejected, "cycle-1", past, "docs/guide.md")
	decision("pr-readme", "octomus/open", "pr-problem", model.DecisionRejected, "cycle-1", future, "README.md")
	decision("closed", "octomus/closed", "closed-problem", model.DecisionRejected, "cycle-1", future, "README.md")
	decision("keyless", "main", "", model.DecisionRejected, "cycle-1", future, "docs/guide.md")
	decision("merged-accepted", "main", "merged-problem", model.DecisionAccepted, "cycle-2", future, "docs/guide.md")
	decision("merged-absorbed", "main", "merged-problem", model.DecisionRejected, "cycle-2", future, "docs/guide.md")
	decision("merged-elsewhere", "main", "merged-problem", model.DecisionRejected, "cycle-3", future, "docs/guide.md")

	cancelled := queuedTask(cfg, "cancelled-task", "main", "octomus/cancelled-task")
	cancelled.Status = model.StatusCancelled
	cancelled.RediscoveryRequested = true
	putTask(t, f, cancelled)

	app := New(f.state, f.dataDir)
	cleanupApp(t, app)
	pr := ownedPR("octomus/open")
	pr.Head = recorded
	check := func(label, revision string, wantDue map[string]bool) {
		t.Helper()
		memory, err := app.planningMemory(ctx, cfg, model.Grounding{Revision: revision, PRs: []model.PullRequest{pr}})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		due := map[string]bool{}
		for _, record := range memory.decisions {
			if _, duplicate := due[record.ID]; duplicate {
				t.Fatalf("%s: decision %s listed twice", label, record.ID)
			}
			due[record.ID] = record.ReconsiderationDue
		}
		if fmt.Sprint(due) != fmt.Sprint(wantDue) {
			t.Fatalf("%s: reconsideration_due by decision = %v; want %v", label, due, wantDue)
		}
		if len(memory.requests) != 1 {
			t.Fatalf("%s: rediscovery requests = %+v; want one", label, memory.requests)
		}
		rediscovery := memory.requests[0]
		if rediscovery.ID != cancelled.ID || rediscovery.Target != "main" || rediscovery.entry["title"] != cancelled.Proposal.Title {
			t.Fatalf("%s: rediscovery request = %+v", label, rediscovery)
		}
	}
	check("at the recorded revision", recorded, map[string]bool{
		"readme": false, "guide": false, "whole-tree": false, "expired": true,
		"pr-readme": false, "merged-accepted": false, "merged-elsewhere": false,
	})

	if err := os.WriteFile(filepath.Join(cfg.Repository, "README.md"), []byte("# Fixture\n\nThe contract changed.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, cfg.Repository, "commit", "-am", "Change the README")
	check("after a README change", git(t, cfg.Repository, "rev-parse", "HEAD"), map[string]bool{
		"readme": true, "guide": false, "whole-tree": true, "expired": true,
		"pr-readme": false, "merged-accepted": false, "merged-elsewhere": false,
	})
}

func TestDecisionMemoryAbsorbs(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	a := New(state, t.TempDir())
	cleanupApp(t, a)
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
	stored := records[0]
	generic, err := wirejson.GenericMap(stored)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := wirejson.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	// The saved shape is the version-7 record: mode as text, an empty path list, no kind or reconsideration_due.
	want := fmt.Sprintf(`{"context_fingerprint":%q,"cycle_id":%q,"decision":"accepted","id":%q,"mode":"execution","problem_key":"stable-problem","reason":%q,"reconsider_after":%q,"relevant_paths":[],"repository":%q,"source_revision":"revision","target":%q}`,
		stored.ContextFingerprint, cycle.ID, cycle.ID+":accepted", accepted.Reason, stored.ReconsiderAfter, cfg.GitHubRepo, cfg.DefaultBranch)
	if string(saved) != want {
		t.Fatalf("decision record changed:\n%s\nwant\n%s", saved, want)
	}
	recorded := model.DecisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeExecution,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionRejected, Reason: "Current decision", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	}
	if err := validateDecisionMemory([]model.Proposal{accepted}, decisionMemory{decisions: []model.DecisionRecord{recorded}}); err == nil {
		t.Fatal("unchanged rejected work became executable without rediscovery")
	}
	auditRecommendation := model.DecisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeAudit,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionAccepted, Reason: "Audit recommendation", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	}
	if err := validateDecisionMemory([]model.Proposal{accepted}, decisionMemory{decisions: []model.DecisionRecord{auditRecommendation}}); err != nil {
		t.Fatalf("audit recommendation incorrectly vetoed execution: %v", err)
	}
	requestID := model.ID()
	request := rediscoveryRequest{ID: requestID, Target: cfg.DefaultBranch}
	reconsidered := accepted.Clone()
	reconsidered.Reconsiders = []string{requestID}
	if err := validateDecisionMemory([]model.Proposal{reconsidered}, decisionMemory{decisions: []model.DecisionRecord{recorded}, requests: []rediscoveryRequest{request}}); err != nil {
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

func savedDecision(cfgRepo, target, id, problem, revision string) model.DecisionRecord {
	return model.DecisionRecord{
		ID: id, CycleMode: model.CycleModeExecution, Repository: cfgRepo,
		Target: target, ProblemKey: problem, RelevantPaths: []string{},
		Decision: model.DecisionRejected, Reason: "Current rejection", SourceRevision: revision,
		ContextFingerprint: revision, ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: id,
	}
}

func TestDecisionAdmissionBeyondPromptWindow(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	ctx := context.Background()
	grounding := model.Grounding{Revision: "same-revision"}
	proposed := proposal("repeat", cfg.DefaultBranch)
	proposed.ProblemKey = "unchanged-problem"
	old := savedDecision(strings.ToUpper(cfg.GitHubRepo), cfg.DefaultBranch, "old", proposed.ProblemIdentity(), grounding.Revision)
	if err := state.Put("decision", old.ID, old); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		id := fmt.Sprintf("unrelated-%d", i)
		if err := state.Put("decision", id, savedDecision(cfg.GitHubRepo, cfg.DefaultBranch, id, id, grounding.Revision)); err != nil {
			t.Fatal(err)
		}
	}
	memory, err := app.planningMemory(ctx, cfg, grounding)
	if err != nil || len(memory.decisions) != 100 {
		t.Fatalf("prompt memory: %d, %v", len(memory.decisions), err)
	}
	for _, record := range memory.decisions {
		if record.ID == old.ID {
			t.Fatal("old decision should be outside this prompt window")
		}
	}
	if err := app.enforceDecisionMemory(ctx, cfg, grounding, []model.Proposal{proposed}, nil); err == nil || !strings.Contains(err.Error(), `decision "old"`) {
		t.Fatalf("current rejection was evicted from admission: %v", err)
	}
	changed := grounding
	changed.Revision = "new-revision"
	if err := app.enforceDecisionMemory(ctx, cfg, changed, []model.Proposal{proposed}, nil); err != nil {
		t.Fatalf("changed context did not allow reconsideration: %v", err)
	}
	request := rediscoveryRequest{ID: "rediscover", Target: cfg.DefaultBranch}
	reconsidered := proposed.Clone()
	reconsidered.Reconsiders = []string{request.ID}
	if err := app.enforceDecisionMemory(ctx, cfg, grounding, []model.Proposal{reconsidered}, []rediscoveryRequest{request}); err != nil {
		t.Fatalf("explicit rediscovery failed: %v", err)
	}
	old.ReconsiderAfter = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := state.Put("decision", old.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := app.enforceDecisionMemory(ctx, cfg, grounding, []model.Proposal{proposed}, nil); err != nil {
		t.Fatalf("expired decision blocked reconsideration: %v", err)
	}
}

func TestDecisionPromptFiltersTargetsBeforeLimit(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	pr := ownedPR("octomus/open")
	pr.Head = "pr-revision"
	grounding := model.Grounding{Revision: "main-revision", PRs: []model.PullRequest{pr}}
	for _, record := range []model.DecisionRecord{
		savedDecision(cfg.GitHubRepo, cfg.DefaultBranch, "main", "main", grounding.Revision),
		savedDecision(cfg.GitHubRepo, pr.Branch, "open", "open", pr.Head),
	} {
		if err := state.Put("decision", record.ID, record); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 100 {
		id := fmt.Sprintf("closed-%d", i)
		if err := state.Put("decision", id, savedDecision(cfg.GitHubRepo, "octomus/closed", id, id, "closed")); err != nil {
			t.Fatal(err)
		}
		id = fmt.Sprintf("other-repository-%d", i)
		if err := state.Put("decision", id, savedDecision("another/repository", cfg.DefaultBranch, id, id, grounding.Revision)); err != nil {
			t.Fatal(err)
		}
	}
	memory, err := app.planningMemory(context.Background(), cfg, grounding)
	if err != nil || len(memory.decisions) != 2 {
		t.Fatalf("irrelevant targets evicted current prompt entries: %+v, %v", memory.decisions, err)
	}
	for _, record := range memory.decisions {
		if record.ID != "main" && record.ID != "open" || record.ReconsiderationDue {
			t.Fatalf("wrong target context: %+v", record)
		}
	}
}

func TestPlanRejectsDecisionOutsidePromptWindow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	revision := git(t, f.repo, "rev-parse", "HEAD")
	old := savedDecision(f.cfg.GitHubRepo, f.cfg.DefaultBranch, "old-rejection", model.ProblemIdentity("Complete the fixture feature", ""), revision)
	if err := f.state.Put("decision", old.ID, old); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		id := fmt.Sprintf("unrelated-%d", i)
		if err := f.state.Put("decision", id, savedDecision(f.cfg.GitHubRepo, f.cfg.DefaultBranch, id, id, revision)); err != nil {
			t.Fatal(err)
		}
	}
	completePlan(t, f).queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, app, id)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, `decision "old-rejection"`) {
		t.Fatalf("planning admitted current rejected work outside the prompt window: %+v", cycle)
	}
}

func TestDecisionAdmissionChecksAllExactHistory(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	grounding := model.Grounding{Revision: "current"}
	proposed := proposal("repeat", cfg.DefaultBranch)
	proposed.ProblemKey = "same-problem"
	old := savedDecision(cfg.GitHubRepo, cfg.DefaultBranch, "still-current", proposed.ProblemIdentity(), grounding.Revision)
	if err := state.Put("decision", old.ID, old); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		id := fmt.Sprintf("different-context-%d", i)
		if err := state.Put("decision", id, savedDecision(cfg.GitHubRepo, cfg.DefaultBranch, id, proposed.ProblemIdentity(), id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.enforceDecisionMemory(context.Background(), cfg, grounding, []model.Proposal{proposed}, nil); err == nil || !strings.Contains(err.Error(), `decision "still-current"`) {
		t.Fatalf("exact history was limited or newer context evicted a current decision: %v", err)
	}
}
