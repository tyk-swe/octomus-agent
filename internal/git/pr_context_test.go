// Ownership of pull requests in the external PR inventory.

package git

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
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

func TestPROwnership(t *testing.T) {
	t.Parallel()
	c := config.Default()
	c.GitHubRepo = "fixture/project"
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
	inventory, err := parseInventory(prContextPage(t, entries...), c)
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
	if _, err := parseInventory(prContextPage(t, wrongBase), c); err == nil {
		t.Fatal("a different base repository must fail")
	}
}
