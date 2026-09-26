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
