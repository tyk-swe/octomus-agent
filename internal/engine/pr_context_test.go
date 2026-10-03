package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func prContextPR(t *testing.T, raw map[string]any) model.PullRequest {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var pr model.PullRequest
	if err := json.Unmarshal(data, &pr); err != nil {
		t.Fatal(err)
	}
	return pr
}

func TestTargetResolutionRejectsExternalAndClosedPRs(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	pr := func(state string, owned bool, baseRepo string) model.PullRequest {
		return prContextPR(t, map[string]any{
			"number": 7, "title": "PR", "branch": "octomus/fix", "head": strings.Repeat("a", 40),
			"base": "main", "url": "https://example.invalid/pull/7", "body": "",
			"state": state, "changed_lines": 1, "created_at": "2026-08-01T00:00:00Z",
			"owned": owned, "head_repository": "fixture/project", "base_repository": baseRepo,
		})
	}
	open := []model.PullRequest{pr("open", true, "fixture/project")}
	bound, err := ResolveTarget(cfg, open, "octomus/fix")
	if err != nil || bound == nil || bound.Number != 7 {
		t.Fatalf("owned open PR = %+v, %v; want PR 7", bound, err)
	}
	release := pr("open", true, "fixture/project")
	release.Base = "release"
	for name, prs := range map[string][]model.PullRequest{
		"external":      {pr("open", false, "fixture/project")},
		"closed":        {pr("closed", true, "fixture/project")},
		"merged":        {pr("merged", true, "fixture/project")},
		"foreign base":  {pr("open", true, "upstream/project")},
		"non-main base": {release},
	} {
		if target, err := ResolveTarget(cfg, prs, "octomus/fix"); err == nil {
			t.Errorf("%s PR resolved as a target: %+v", name, target)
		}
	}
	if target, err := ResolveTarget(cfg, open, "main"); err != nil || target != nil {
		t.Fatalf("default branch resolved to %+v, %v; want no PR", target, err)
	}
}
