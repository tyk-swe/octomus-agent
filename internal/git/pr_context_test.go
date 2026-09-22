package git_test

// Ports of tests/pr_context.rs inventory parsing cases: every paginated page
// is read, ownership needs every identity signal, and malformed or conflicting
// inventories fail closed.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/git"
)

func prContextEntry(number int, branch string, headRepo, baseRepo any, body, state string) map[string]any {
	return map[string]any{
		"number":     number,
		"title":      fmt.Sprintf("Pull request %d", number),
		"body":       body,
		"state":      state,
		"merged_at":  nil,
		"head":       map[string]any{"ref": branch, "sha": fmt.Sprintf("%040x", number), "repo": headRepo},
		"base":       map[string]any{"ref": "main", "repo": baseRepo},
		"html_url":   fmt.Sprintf("https://example.invalid/pull/%d", number),
		"additions":  3,
		"deletions":  1,
		"created_at": "2026-08-01T00:00:00Z",
	}
}

func prContextRepo(name string) map[string]any { return map[string]any{"full_name": name} }

func prContextOwned(number int, branch string) map[string]any {
	return prContextEntry(number, branch, prContextRepo("fixture/project"), prContextRepo("fixture/project"),
		fmt.Sprintf("Owned work.\n<!-- octomus:task:task-%d -->", number), "open")
}

func prContextExternal(number int) map[string]any {
	return prContextEntry(number, "contributor/work", prContextRepo("contributor/project"), prContextRepo("fixture/project"),
		"External work without a task marker.", "open")
}

func prContextPage(t *testing.T, entries ...map[string]any) string {
	t.Helper()
	if entries == nil {
		entries = []map[string]any{}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestInventoryReadsEveryPageDedupesAndSorts ports
// inventory_reads_every_page_dedupes_and_sorts.
func TestInventoryReadsEveryPageDedupesAndSorts(t *testing.T) {
	c := testConfig()
	first, second, third := []map[string]any{}, []map[string]any{}, []map[string]any{}
	for n := 60; n >= 1; n-- {
		first = append(first, prContextExternal(n))
	}
	for n := 120; n >= 61; n-- {
		second = append(second, prContextOwned(n, fmt.Sprintf("octomus/work-%d", n)))
	}
	for n := 150; n >= 121; n-- {
		if n%2 == 0 {
			third = append(third, prContextOwned(n, fmt.Sprintf("octomus/work-%d", n)))
		} else {
			third = append(third, prContextExternal(n))
		}
	}
	pages := prContextPage(t, first...) + prContextPage(t, second...) + prContextPage(t, third...)
	inventory, err := git.ParseInventory(pages, c)
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Repository != "fixture/project" {
		t.Fatalf("repository = %q", inventory.Repository)
	}
	if inventory.ObservedAt == "" {
		t.Fatal("observed_at is empty")
	}
	if len(inventory.PRs) != 150 {
		t.Fatalf("inventory has %d PRs; want every page's 150", len(inventory.PRs))
	}
	for i := 1; i < len(inventory.PRs); i++ {
		if inventory.PRs[i-1].Number >= inventory.PRs[i].Number {
			t.Fatalf("inventory not strictly sorted at %d: %d then %d", i, inventory.PRs[i-1].Number, inventory.PRs[i].Number)
		}
	}
	owned := 0
	for _, pr := range inventory.PRs {
		if pr.Owned {
			owned++
		}
	}
	if owned != 75 {
		t.Fatalf("owned = %d; want 75", owned)
	}
	duplicate := prContextPage(t, prContextExternal(7)) + prContextPage(t, prContextExternal(7))
	inventory, err = git.ParseInventory(duplicate, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.PRs) != 1 {
		t.Fatalf("identical duplicates = %+v; want one entry", inventory.PRs)
	}
}

// TestOwnershipRequiresPrefixHeadRepositoryBaseRepositoryAndMarker ports
// ownership_requires_prefix_head_repository_base_repository_and_marker.
func TestOwnershipRequiresPrefixHeadRepositoryBaseRepositoryAndMarker(t *testing.T) {
	c := testConfig()
	cases := []struct {
		entry map[string]any
		owned bool
	}{
		{prContextOwned(1, "octomus/owned"), true},
		{prContextEntry(2, "octomus/fork", prContextRepo("contributor/project"), prContextRepo("fixture/project"),
			"Fork work.\n<!-- octomus:task:fork -->", "open"), false},
		{prContextEntry(3, "feature/unprefixed", prContextRepo("fixture/project"), prContextRepo("fixture/project"),
			"Same repository but not an Octomus branch.\n<!-- octomus:task:u -->", "open"), false},
		{prContextEntry(4, "octomus/unmarked", prContextRepo("fixture/project"), prContextRepo("fixture/project"),
			"Prefixed branch without the task marker.", "open"), false},
		{prContextEntry(5, "octomus/deleted-head", nil, prContextRepo("fixture/project"),
			"Source repository was deleted.\n<!-- octomus:task:gone -->", "open"), false},
	}
	entries := []map[string]any{}
	for _, tc := range cases {
		entries = append(entries, tc.entry)
	}
	inventory, err := git.ParseInventory(prContextPage(t, entries...), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.PRs) != len(cases) {
		t.Fatalf("inventory = %+v", inventory.PRs)
	}
	for i, tc := range cases {
		if inventory.PRs[i].Owned != tc.owned {
			t.Errorf("case %d: owned = %v; want %v", i+1, inventory.PRs[i].Owned, tc.owned)
		}
	}
	deleted := inventory.PRs[4]
	if deleted.HeadRepository != "" || deleted.Branch != "octomus/deleted-head" {
		t.Fatalf("deleted head = %+v; want empty head repository and preserved branch", deleted)
	}
	wrongBase := prContextEntry(6, "octomus/cross-repo", prContextRepo("fixture/project"), prContextRepo("upstream/project"),
		"Targets a different base repository.\n<!-- octomus:task:x -->", "open")
	if _, err := git.ParseInventory(prContextPage(t, wrongBase), c); err == nil {
		t.Fatal("a different base repository must fail")
	}
}

// TestMalformedOrConflictingInventoryFailsClosed ports
// malformed_or_conflicting_inventory_fails_closed.
func TestMalformedOrConflictingInventoryFailsClosed(t *testing.T) {
	c := testConfig()
	conflicting := prContextExternal(9)
	conflicting["head"].(map[string]any)["sha"] = strings.Repeat("f", 40)
	if _, err := git.ParseInventory(prContextPage(t, prContextExternal(9))+prContextPage(t, conflicting), c); err == nil {
		t.Fatal("conflicting head across pages must fail")
	}
	marked := prContextEntry(30, "octomus/shared", prContextRepo("fixture/project"), prContextRepo("fixture/project"),
		"Marked.\n<!-- octomus:task:m -->", "open")
	unmarked := prContextEntry(30, "octomus/shared", prContextRepo("fixture/project"), prContextRepo("fixture/project"),
		"The marker is gone.", "open")
	if _, err := git.ParseInventory(prContextPage(t, marked)+prContextPage(t, unmarked), c); err == nil {
		t.Fatal("conflicting marker across pages must fail")
	}
	for _, empty := range []string{"", "  \n"} {
		if _, err := git.ParseInventory(empty, c); err == nil {
			t.Fatalf("empty response %q must fail", empty)
		}
	}
	missingState := prContextExternal(20)
	delete(missingState, "state")
	if _, err := git.ParseInventory(prContextPage(t, missingState), c); err == nil {
		t.Fatal("a missing state must fail")
	}
	unknownState := prContextEntry(21, "contributor/work", prContextRepo("contributor/project"), prContextRepo("fixture/project"),
		"Unknown state.", "draft")
	if _, err := git.ParseInventory(prContextPage(t, unknownState), c); err == nil {
		t.Fatal("an unknown state must fail")
	}
	for _, name := range []string{"head.ref", "head.sha", "base.ref", "html_url"} {
		broken := prContextExternal(10)
		switch name {
		case "head.ref":
			broken["head"].(map[string]any)["ref"] = ""
		case "head.sha":
			broken["head"].(map[string]any)["sha"] = ""
		case "base.ref":
			broken["base"].(map[string]any)["ref"] = ""
		default:
			broken["html_url"] = ""
		}
		if _, err := git.ParseInventory(prContextPage(t, broken), c); err == nil {
			t.Errorf("empty %s must fail", name)
		}
	}
	noBaseRepo := prContextExternal(11)
	noBaseRepo["base"].(map[string]any)["repo"] = nil
	if _, err := git.ParseInventory(prContextPage(t, noBaseRepo), c); err == nil {
		t.Fatal("a missing base repository must fail")
	}
	if _, err := git.ParseInventory(`[{"title":"No identity","state":"open"}]`, c); err == nil {
		t.Fatal("a missing number must fail")
	}
	closed := prContextEntry(12, "octomus/closed", prContextRepo("fixture/project"), prContextRepo("fixture/project"),
		"Done.\n<!-- octomus:task:closed -->", "closed")
	inventory, err := git.ParseInventory(prContextPage(t, closed, prContextOwned(13, "octomus/open")), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.PRs) != 1 || inventory.PRs[0].Number != 13 {
		t.Fatalf("inventory = %+v; want only open PR 13", inventory.PRs)
	}
}
