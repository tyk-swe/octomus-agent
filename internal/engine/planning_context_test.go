package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestReviewMissingForkAvailabilityReachesGrounding(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	remote := filepath.Join(f.root, "remote.git")
	fork := filepath.Join(f.root, "contributor")
	git(t, f.root, "clone", remote, fork)
	git(t, fork, "config", "user.name", "Fixture contributor")
	git(t, fork, "config", "user.email", "contributor@example.com")
	must0(t, os.WriteFile(filepath.Join(fork, "fork.txt"), []byte("fork-only change\n"), 0600))
	git(t, fork, "add", "fork.txt")
	git(t, fork, "commit", "-m", "Contributor change")
	fetched := git(t, fork, "rev-parse", "HEAD")
	git(t, fork, "push", "origin", "HEAD:refs/pull/44/head", "HEAD:refs/pull/45/head")
	prs := []map[string]any{}
	for _, entry := range []struct {
		number uint64
		head   string
	}{{43, strings.Repeat("b", 40)}, {44, fetched}, {45, strings.Repeat("a", 40)}} {
		prs = append(prs, map[string]any{
			"number": entry.number, "title": "Contributor fix", "body": "Read-only context",
			"head": map[string]any{"ref": fmt.Sprintf("contributor/%d", entry.number), "sha": entry.head,
				"repo": map[string]any{"full_name": "contributor/project"}},
			"base":     map[string]any{"ref": "main"},
			"html_url": fmt.Sprintf("https://github.com/fixture/project/pull/%d", entry.number), "state": "open",
		})
	}
	data, err := json.Marshal(prs)
	must0(t, err)
	must0(t, os.WriteFile(filepath.Join(f.root, "prs.json"), data, 0600))
	plan := completePlan(t, f)
	plan.grounding.Effect = func(cwd string) error {
		_, err := gitops.WorkGit(t.Context(), f.cfg, cwd, []string{"cat-file", "-e", fetched + "^{commit}"})
		return err
	}
	plan.queue(f)
	app := f.pausedApp(t)
	id, err := app.startAudit(t.Context())
	must0(t, err)
	cycle := waitCycle(t, app, id)
	assertPlanningPass(t, f, cycle)
	if cycle.Grounding == nil || len(cycle.Grounding.ExternalPRs) != 3 {
		t.Fatalf("missing external context: %+v", cycle.Grounding)
	}
	for _, pr := range cycle.Grounding.ExternalPRs {
		// #43 cannot be fetched. #45's ref was successfully fetched, but its
		// recorded object is absent: a moved ref is not evidence for that SHA.
		if want := pr.Number == 44; pr.LocalHeadAvailable != want {
			t.Fatalf("PR #%d availability = %v, want %v", pr.Number, pr.LocalHeadAvailable, want)
		}
		encoded := mustJSON(t, pr)
		for _, turn := range f.planningTurns() {
			if !strings.Contains(turn.Prompt, encoded) {
				t.Fatalf("planning session %s omitted exact PR #%d availability context", turn.Session, pr.Number)
			}
		}
	}
	if !cycle.Grounding.PRCoverage.Complete || cycle.Grounding.PRCoverage.IncludedExternal != 3 {
		t.Fatal("local object availability changed remote inventory coverage")
	}
	events, err := f.state.Events(&id)
	must0(t, err)
	found := false
	for _, event := range events {
		found = found || event.Message == "Fork PR heads unavailable locally: [43 45]"
	}
	if !found {
		t.Fatal("unavailable recorded heads were not logged")
	}
}

func TestExternalPRHistoricalAvailabilityIsConservative(t *testing.T) {
	t.Parallel()
	var pr model.ExternalPRContext
	must0(t, json.Unmarshal([]byte(`{"number":43,"url":"url","title":"title","body":"body","branch":"fork","head":"recorded","base":"main","head_repository":"contributor/project","base_repository":"fixture/project","title_truncated":false,"body_truncated":false}`), &pr))
	if pr.LocalHeadAvailable {
		t.Fatal("an old record without an object observation asserted availability")
	}
}
