package engine

// Decision memory: fingerprints of model-supplied relevant paths and the
// bounds on proposal decision metadata.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// relevant_paths come from model output. A path that looks like git pathspec
// magic is matched literally (normally matching nothing) instead of failing
// ls-tree and, with it, the whole plan; ordinary paths keep the fingerprint
// they had before, so saved decisions still match.
func TestDecisionFingerprintTreatsPathsLiterally(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	gitOutput := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("/usr/bin/git", args...)
		cmd.Dir = fixture.cfg.Repository
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(output)
	}
	revision := strings.TrimSpace(gitOutput("rev-parse", "HEAD"))
	ctx := context.Background()

	nothing := fmt.Sprintf("%x", sha256.Sum256(nil))
	for _, paths := range [][]string{{":(glob)*.md"}, {":!README.md"}} {
		fingerprint, err := decisionFingerprint(ctx, fixture.cfg, revision, paths)
		if err != nil {
			t.Fatalf("pathspec-like path %q failed the fingerprint: %v", paths, err)
		}
		if fingerprint != nothing {
			t.Fatalf("pathspec-like path %q matched files: %s", paths, fingerprint)
		}
	}

	listing := strings.TrimSpace(gitOutput("ls-tree", "-r", revision, "--", "README.md"))
	if listing == "" {
		t.Fatal("fixture README.md is not tracked")
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(listing)))
	fingerprint, err := decisionFingerprint(ctx, fixture.cfg, revision, []string{"README.md"})
	if err != nil || fingerprint != want {
		t.Fatalf("ordinary path fingerprint = %s, %v; want %s", fingerprint, err, want)
	}
}

// Decision memory records every proposal, whatever its decision, and bounds
// its metadata. A bound failure names the proposal and the field; an empty
// problem_key falls back to the title, so a long title on a rejected idea is
// the likely cause. The bounds themselves are unchanged: a long title with a
// short explicit problem_key stays valid.
func TestDecisionMetadataBoundsNameTheProposalAndField(t *testing.T) {
	cfg := testConfig(t.TempDir())
	rejected := func(id, title, key string) model.Proposal {
		return model.Proposal{ID: id, Title: title, ProblemKey: key, Target: cfg.DefaultBranch, Decision: model.DecisionRejected, Reason: "No measured benefit.", RelevantPaths: []string{}, Reconsiders: []string{}}
	}
	// Planning replaces every problem_key with the proposal's identity first.
	normalized := func(proposal model.Proposal) []model.Proposal {
		proposal.ProblemKey = proposal.ProblemIdentity()
		return []model.Proposal{proposal}
	}
	longTitle := strings.Repeat("긴제목", 30) // 90 characters, 270 bytes

	keyed := normalized(rejected("d1-keyed", longTitle, "short-stable-key"))
	if err := ValidateProposals(cfg, keyed, model.Grounding{}, nil); err != nil {
		t.Fatalf("long rejected title with a short key: %v", err)
	}
	if err := ValidateDecisionMemory(keyed, nil); err != nil {
		t.Fatalf("long rejected title with a short key: %v", err)
	}

	fallback := normalized(rejected("d1-long", longTitle, ""))
	if err := ValidateProposals(cfg, fallback, model.Grounding{}, nil); err != nil {
		t.Fatalf("rejected proposals carry no title bound: %v", err)
	}
	err := ValidateDecisionMemory(fallback, nil)
	if err == nil || !strings.Contains(err.Error(), `"d1-long"`) || !strings.Contains(err.Error(), "problem identity is 270 bytes") || !strings.Contains(err.Error(), "exceeds bounds") {
		t.Fatalf("title-derived identity bound = %v; want the proposal, field and size", err)
	}

	paths := rejected("d1-paths", "Short title", "")
	for i := 0; i < 41; i++ {
		paths.RelevantPaths = append(paths.RelevantPaths, fmt.Sprintf("file-%d.go", i))
	}
	if err := ValidateDecisionMemory(normalized(paths), nil); err == nil || !strings.Contains(err.Error(), `"d1-paths"`) || !strings.Contains(err.Error(), "41 relevant_paths") {
		t.Fatalf("relevant_paths bound = %v; want the proposal and field", err)
	}

	reconsiders := rejected("d1-reconsiders", "Short title", "")
	for i := 0; i < 101; i++ {
		reconsiders.Reconsiders = append(reconsiders.Reconsiders, fmt.Sprintf("task-%d", i))
	}
	if err := ValidateDecisionMemory(normalized(reconsiders), nil); err == nil || !strings.Contains(err.Error(), `"d1-reconsiders"`) || !strings.Contains(err.Error(), "101 reconsiders") {
		t.Fatalf("reconsiders bound = %v; want the proposal and field", err)
	}
}
