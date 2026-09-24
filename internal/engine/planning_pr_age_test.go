package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

type maintenancePRFixture struct {
	name         string
	ageDays      int
	createdAt    string
	changedLines uint64
}

func writeMaintenancePRFixture(t *testing.T, fixture *scriptedFixture, prs []maintenancePRFixture) {
	t.Helper()
	now := time.Now().UTC()
	entries := make([]map[string]any, 0, len(prs))
	for i, pr := range prs {
		branch := fixture.cfg.BranchPrefix + pr.name
		command(t, fixture.repo, "/usr/bin/git", "branch", branch)
		command(t, fixture.repo, "/usr/bin/git", "push", "origin", branch)
		createdAt := pr.createdAt
		if createdAt == "" {
			createdAt = now.AddDate(0, 0, -pr.ageDays).Format(time.RFC3339)
		}
		entries = append(entries, map[string]any{
			"number": i + 1, "title": fmt.Sprintf("Owned PR %d", i+1),
			"body":     fmt.Sprintf("<!-- octomus:task:age-%d -->", i+1),
			"head":     map[string]any{"ref": branch, "repo": map[string]any{"full_name": fixture.cfg.GitHubRepo}},
			"base":     map[string]any{"ref": fixture.cfg.DefaultBranch, "repo": map[string]any{"full_name": fixture.cfg.GitHubRepo}},
			"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", fixture.cfg.GitHubRepo, i+1),
			"state":    "open", "merged_at": nil,
			"additions": pr.changedLines, "deletions": 0, "created_at": createdAt,
		})
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "prs.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The audit entry point records the same grounding that discovery receives.
// These cases also exercise configuration validation, owned PR parsing, and
// the independent changed-line maintenance rule.
func TestAuditGroundingOwnedPRAgeThresholds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		threshold uint64
		prs       []maintenancePRFixture
		want      []string
	}{
		{
			name: "ordinary", threshold: 7,
			prs:  []maintenancePRFixture{{name: "old", ageDays: 8, changedLines: 1}, {name: "recent", ageDays: 2, changedLines: 1}},
			want: []string{"old"},
		},
		{
			name: "zero days with invalid and future creation times", threshold: 0,
			prs: []maintenancePRFixture{
				{name: "past", ageDays: 2, changedLines: 1},
				{name: "future", ageDays: -1, changedLines: 1},
				{name: "invalid", createdAt: "not-a-date", changedLines: 1},
			},
			want: []string{"past"},
		},
		{
			name: "future creation time", threshold: 7,
			prs: []maintenancePRFixture{{name: "future", ageDays: -1, changedLines: 1}},
		},
		{
			name: "106751 days", threshold: 106751,
			prs: []maintenancePRFixture{
				{name: "at-limit", ageDays: 106751, changedLines: 1},
				{name: "below-limit", ageDays: 106750, changedLines: 1},
			},
			want: []string{"at-limit"},
		},
		{
			name: "106752 days", threshold: 106752,
			prs: []maintenancePRFixture{
				{name: "at-limit", ageDays: 106752, changedLines: 1},
				{name: "below-limit", ageDays: 106751, changedLines: 1},
			},
			want: []string{"at-limit"},
		},
		{
			name: "largest accepted uint64 threshold", threshold: ^uint64(0),
			prs: []maintenancePRFixture{
				{name: "ancient", ageDays: 106753, changedLines: 1},
				{name: "large-invalid", createdAt: "not-a-date", changedLines: 1000},
				{name: "large-future", ageDays: -1, changedLines: 1000},
			},
			want: []string{"large-invalid", "large-future"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			fixture.configure(t, func(cfg *config.Config) { cfg.LongLivedPRDays = tc.threshold })
			if err := fixture.cfg.ValidateAudit(); err != nil {
				t.Fatalf("threshold %d rejected by configuration validation: %v", tc.threshold, err)
			}
			writeMaintenancePRFixture(t, fixture, tc.prs)
			completePlan(t, fixture).queue(fixture)
			app := fixture.pausedApp(t)
			cycleID, err := app.StartAudit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cycle := waitCycle(t, fixture.state, cycleID)
			if cycle.Status != model.CycleCompleted || cycle.Grounding == nil {
				t.Fatalf("audit did not record grounding: status=%s error=%v", cycle.Status, cycle.Error)
			}
			want := make([]string, 0, len(tc.want))
			for _, name := range tc.want {
				want = append(want, fixture.cfg.BranchPrefix+name)
			}
			sort.Strings(want)
			if !slices.Equal(cycle.Grounding.MaintenanceTargets, want) {
				t.Fatalf("maintenance targets for %d days = %v; want %v", tc.threshold, cycle.Grounding.MaintenanceTargets, want)
			}
		})
	}
}
