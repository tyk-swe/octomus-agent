package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestDecisionFingerprintTreatsPathsLiterally(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	revision := git(t, fixture.cfg.Repository, "rev-parse", "HEAD")
	ctx := context.Background()

	nothing := fmt.Sprintf("%x", sha256.Sum256(nil))
	for _, paths := range [][]string{{":(glob)*.md"}, {":!README.md"}, {":/README.md"}} {
		fingerprint, err := decisionFingerprint(ctx, fixture.cfg, revision, paths)
		if err != nil {
			t.Fatalf("pathspec-like path %q failed the fingerprint: %v", paths, err)
		}
		if fingerprint != nothing {
			t.Fatalf("pathspec-like path %q matched files: %s", paths, fingerprint)
		}
	}

	listing := git(t, fixture.cfg.Repository, "ls-tree", "-r", revision, "--", "README.md")
	if listing == "" {
		t.Fatal("fixture README.md is not tracked")
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(listing)))
	fingerprint, err := decisionFingerprint(ctx, fixture.cfg, revision, []string{"README.md"})
	if err != nil || fingerprint != want {
		t.Fatalf("ordinary path fingerprint = %s, %v; want %s", fingerprint, err, want)
	}
	if fingerprint, err := decisionFingerprint(ctx, fixture.cfg, revision, []string{}); err != nil || fingerprint != revision {
		t.Fatalf("pathless fingerprint = %s, %v; want the revision %s", fingerprint, err, revision)
	}
}

func TestDecisionMetadataBoundsNameTheProposalAndField(t *testing.T) {
	cfg := testConfig(t.TempDir())
	rejected := func(id, title, key string) model.Proposal {
		return model.Proposal{ID: id, Title: title, ProblemKey: key, Target: cfg.DefaultBranch, Decision: model.DecisionRejected, Reason: "No measured benefit.", RelevantPaths: []string{}, Reconsiders: []string{}}
	}
	normalized := func(proposal model.Proposal) []model.Proposal {
		proposal.ProblemKey = proposal.ProblemIdentity()
		return []model.Proposal{proposal}
	}
	longTitle := strings.Repeat("긴제목", 30)

	keyed := normalized(rejected("d1-keyed", longTitle, "short-stable-key"))
	if err := ValidateProposals(cfg, keyed, model.Grounding{}, nil); err != nil {
		t.Fatalf("long rejected title with a short key: %v", err)
	}
	if err := validateDecisionMemory(keyed, decisionMemory{}); err != nil {
		t.Fatalf("long rejected title with a short key: %v", err)
	}

	fallback := normalized(rejected("d1-long", longTitle, ""))
	if err := ValidateProposals(cfg, fallback, model.Grounding{}, nil); err != nil {
		t.Fatalf("rejected proposals carry no title bound: %v", err)
	}
	err := validateDecisionMemory(fallback, decisionMemory{})
	if err == nil || !strings.Contains(err.Error(), `"d1-long"`) || !strings.Contains(err.Error(), "problem identity is 270 bytes") || !strings.Contains(err.Error(), "exceeds bounds") {
		t.Fatalf("title-derived identity bound = %v; want the proposal, field and size", err)
	}

	paths := rejected("d1-paths", "Short title", "")
	for i := 0; i < 41; i++ {
		paths.RelevantPaths = append(paths.RelevantPaths, fmt.Sprintf("file-%d.go", i))
	}
	if err := validateDecisionMemory(normalized(paths), decisionMemory{}); err == nil || !strings.Contains(err.Error(), `"d1-paths"`) || !strings.Contains(err.Error(), "41 relevant_paths") {
		t.Fatalf("relevant_paths bound = %v; want the proposal and field", err)
	}

	reconsiders := rejected("d1-reconsiders", "Short title", "")
	for i := 0; i < 101; i++ {
		reconsiders.Reconsiders = append(reconsiders.Reconsiders, fmt.Sprintf("task-%d", i))
	}
	if err := validateDecisionMemory(normalized(reconsiders), decisionMemory{}); err == nil || !strings.Contains(err.Error(), `"d1-reconsiders"`) || !strings.Contains(err.Error(), "101 reconsiders") {
		t.Fatalf("reconsiders bound = %v; want the proposal and field", err)
	}
}

func TestPlanningMemoryReconsiderationRules(t *testing.T) {
	fixture := newScriptedFixture(t)
	cfg := fixture.cfg
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
		if err := fixture.state.Put("decision", id, record); err != nil {
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
	if err := fixture.state.Put("task", cancelled.ID, cancelled); err != nil {
		t.Fatal(err)
	}

	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
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

func TestPlanningMemoryPromptJSONIsStable(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	ctx := context.Background()
	const revision = "grounded-revision"
	current, err := decisionFingerprint(ctx, cfg, revision, []string{})
	if err != nil {
		t.Fatal(err)
	}
	decision := func(id, problem, verdict, cycleID, fingerprint string) {
		t.Helper()
		record := map[string]any{
			"id": id, "mode": "execution", "repository": cfg.GitHubRepo, "target": cfg.DefaultBranch,
			"problem_key": problem, "relevant_paths": []string{}, "decision": verdict,
			"reason": "Recorded " + id, "source_revision": revision,
			"context_fingerprint": fingerprint, "reconsider_after": "2999-01-01T00:00:00Z", "cycle_id": cycleID,
		}
		if err := state.Put("decision", id, record); err != nil {
			t.Fatal(err)
		}
	}
	decision("current", "current-problem", model.DecisionRejected, "cycle-1", current)
	decision("changed", "changed-problem", model.DecisionDeferred, "cycle-1", "older-fingerprint")
	decision("merged", "merged-problem", model.DecisionAccepted, "cycle-2", current)
	decision("absorbed", "merged-problem", model.DecisionRejected, "cycle-2", current)
	cancelled := queuedTask(cfg, "cancelled-task", cfg.DefaultBranch, "octomus/cancelled-task")
	cancelled.Status = model.StatusCancelled
	cancelled.RediscoveryRequested = true
	if err := state.Put("task", cancelled.ID, cancelled); err != nil {
		t.Fatal(err)
	}

	memory, err := app.planningMemory(ctx, cfg, model.Grounding{Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	got, err := wirejson.Marshal(memory.promptEntries())
	if err != nil {
		t.Fatal(err)
	}
	decisionJSON := func(id, problem, verdict, cycleID, fingerprint string, due bool) string {
		return fmt.Sprintf(`{"context_fingerprint":%q,"cycle_id":%q,"decision":%q,"id":%q,"kind":"decision","mode":"execution","problem_key":%q,"reason":"Recorded %s","reconsider_after":"2999-01-01T00:00:00Z","reconsideration_due":%t,"relevant_paths":[],"repository":"fixture/project","source_revision":"grounded-revision","target":"main"}`,
			fingerprint, cycleID, verdict, id, problem, id, due)
	}
	want := "[" + strings.Join([]string{
		decisionJSON("merged", "merged-problem", model.DecisionAccepted, "cycle-2", current, false),
		decisionJSON("changed", "changed-problem", model.DecisionDeferred, "cycle-1", "older-fingerprint", true),
		decisionJSON("current", "current-problem", model.DecisionRejected, "cycle-1", current, false),
		`{"id":"cancelled-task","kind":"rediscovery","problem":"Missing behavior cancelled-task","scope":"one file","target":"main","title":"Concrete cancelled-task"}`,
	}, ",") + "]"
	if string(got) != want {
		t.Fatalf("decision memory prompt JSON changed:\n got %s\nwant %s", got, want)
	}
}
