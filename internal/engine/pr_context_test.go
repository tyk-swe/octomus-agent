package engine

// Ports of tests/pr_context.rs external-context and target-resolution cases:
// the bounded external context over a parsed inventory, and the executable
// target contract that only binds owned open PRs on the default branch.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/jsoncompat"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func prContextInventoryEntry(number int, branch, headRepo, body string) map[string]any {
	return map[string]any{
		"number":     number,
		"title":      fmt.Sprintf("Pull request %d", number),
		"body":       body,
		"state":      "open",
		"merged_at":  nil,
		"head":       map[string]any{"ref": branch, "sha": fmt.Sprintf("%040x", number), "repo": map[string]any{"full_name": headRepo}},
		"base":       map[string]any{"ref": "main", "repo": map[string]any{"full_name": "fixture/project"}},
		"html_url":   fmt.Sprintf("https://example.invalid/pull/%d", number),
		"additions":  3,
		"deletions":  1,
		"created_at": "2026-08-01T00:00:00Z",
	}
}

func prContextExternalEntry(number int) map[string]any {
	return prContextInventoryEntry(number, "contributor/work", "contributor/project", "External work without a task marker.")
}

func prContextParse(t *testing.T, entries []map[string]any) model.OpenPrInventory {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := gitops.ParseInventory(string(data), testConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}

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

// TestExternalContextBoundsCountsAndTruncatesUTF8 ports
// external_context_bounds_counts_and_truncates_utf8.
func TestExternalContextBoundsCountsAndTruncatesUTF8(t *testing.T) {
	entries := []map[string]any{}
	for n := 1; n <= 130; n++ {
		entries = append(entries, prContextExternalEntry(n))
	}
	entries = append(entries, prContextInventoryEntry(200, "octomus/mine", "fixture/project", "Owned work.\n<!-- octomus:task:task-200 -->"))
	external, coverage, err := ExternalContext(prContextParse(t, entries))
	if err != nil {
		t.Fatal(err)
	}
	if coverage.TotalOpen != 131 || coverage.TotalExternal != 130 || coverage.IncludedExternal != 100 ||
		coverage.OmittedExternal != 30 || !coverage.Complete || coverage.ObservedAt == nil {
		t.Fatalf("coverage = %+v", coverage)
	}
	if len(external) != 100 {
		t.Fatalf("external = %d entries; want 100", len(external))
	}
	for _, pr := range external {
		if pr.TitleTruncated || pr.BodyTruncated {
			t.Fatalf("short entry marked truncated: %+v", pr)
		}
	}

	titled := prContextExternalEntry(300)
	titled["title"] = strings.Repeat("héllo𐐀", 60)
	bodied := prContextExternalEntry(301)
	bodied["body"] = strings.Repeat("é", 2500)
	external, coverage, err = ExternalContext(prContextParse(t, []map[string]any{titled, bodied}))
	if err != nil {
		t.Fatal(err)
	}
	if len(external) != 2 {
		t.Fatalf("external = %+v", external)
	}
	if utf8.RuneCountInString(external[0].Title) != 200 || !external[0].TitleTruncated || !utf8.ValidString(external[0].Title) {
		t.Fatalf("title truncation = %d chars, truncated=%v", utf8.RuneCountInString(external[0].Title), external[0].TitleTruncated)
	}
	if utf8.RuneCountInString(external[1].Body) != 2000 || !external[1].BodyTruncated || !utf8.ValidString(external[1].Body) {
		t.Fatalf("body truncation = %d chars, truncated=%v", utf8.RuneCountInString(external[1].Body), external[1].BodyTruncated)
	}
	if coverage.IncludedExternal != 2 || coverage.OmittedExternal != 0 {
		t.Fatalf("coverage = %+v", coverage)
	}

	huge := []map[string]any{}
	for n := 400; n <= 500; n++ {
		p := prContextExternalEntry(n)
		p["body"] = strings.Repeat("𐐀", 2000)
		huge = append(huge, p)
	}
	external, coverage, err = ExternalContext(prContextParse(t, huge))
	if err != nil {
		t.Fatal(err)
	}
	if coverage.IncludedExternal >= coverage.TotalExternal || coverage.IncludedExternal >= coverage.MaxExternal {
		t.Fatalf("byte bound did not omit entries: %+v", coverage)
	}
	if coverage.IncludedExternal+coverage.OmittedExternal != coverage.TotalExternal {
		t.Fatalf("coverage does not account for every external PR: %+v", coverage)
	}
	encoded, err := jsoncompat.Marshal(external)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(encoded)) > coverage.MaxContextBytes {
		t.Fatalf("external context is %d bytes; bound %d", len(encoded), coverage.MaxContextBytes)
	}
	if coverage.MaxExternal != 100 || coverage.MaxTitleChars != 200 || coverage.MaxBodyChars != 2000 {
		t.Fatalf("coverage limits = %+v", coverage)
	}
}

// TestExternalContextSortsUnsortedInventories ports
// external_context_sorts_unsorted_inventories.
func TestExternalContextSortsUnsortedInventories(t *testing.T) {
	externalPR := func(number int) model.PullRequest {
		return prContextPR(t, map[string]any{
			"number": number, "title": "External", "branch": fmt.Sprintf("contributor/%d", number),
			"head": strings.Repeat("d", 40), "base": "main",
			"url": fmt.Sprintf("https://example.invalid/pull/%d", number), "body": "External work.",
			"state": "open", "changed_lines": 1, "created_at": "2026-08-01T00:00:00Z",
			"owned": false, "head_repository": "contributor/project",
			"base_repository": "fixture/project",
		})
	}
	inventory := model.OpenPrInventory{
		Repository: "fixture/project",
		ObservedAt: "2026-08-01T00:00:00Z",
		PRs:        []model.PullRequest{externalPR(9), externalPR(3), externalPR(7)},
	}
	external, coverage, err := ExternalContext(inventory)
	if err != nil {
		t.Fatal(err)
	}
	numbers := []uint64{}
	for _, pr := range external {
		numbers = append(numbers, pr.Number)
	}
	if fmt.Sprint(numbers) != fmt.Sprint([]uint64{3, 7, 9}) {
		t.Fatalf("external order = %v; want [3 7 9]", numbers)
	}
	if coverage.IncludedExternal != 3 {
		t.Fatalf("coverage = %+v", coverage)
	}
}

// TestTargetResolutionRejectsExternalAndClosedPRs ports
// target_resolution_rejects_external_and_closed_prs.
func TestTargetResolutionRejectsExternalAndClosedPRs(t *testing.T) {
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
