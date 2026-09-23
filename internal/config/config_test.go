package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRoutesAreExactAndRolesExplicit(t *testing.T) {
	c := Default()
	if err := c.Validate(false); err != nil {
		t.Fatal(err)
	}
	if c.Validate(true) == nil {
		t.Fatal("unconfigured routes ready")
	}
	for _, r := range c.RoutesFor(false) {
		if r.Route.Model != "" {
			t.Fatal("default silently named a model")
		}
	}
	if c.Tiers["XS"].Effort != "xhigh" || c.Tiers["S"].Effort != "max" || c.Tiers["XL"].Effort != "high" {
		t.Fatal("tier ladder changed")
	}
	if c.RepairRoute != DefaultRepairRoute() {
		t.Fatal("repair route default changed")
	}
	if len(c.RoutesFor(true)) != 3 || len(c.RoutesFor(false)) != 10 || c.PlanningAdmissionsRequired() != 13 {
		t.Fatal("route/admission counts")
	}
}
func TestReadinessAndBaseline(t *testing.T) {
	c := Default()
	c.Repository = t.TempDir()
	if err := os.Mkdir(filepath.Join(c.Repository, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	c.GitHubRepo = "fixture/project"
	for _, role := range []string{"orchestrator", "discovery", "proposal_reviewer"} {
		c.Roles[role] = NewRoute("available", "low")
	}
	if err := c.ValidateAudit(); err != nil {
		t.Fatal(err)
	}
	if c.Validate(true) == nil {
		t.Fatal("audit configuration ready to execute")
	}
	if c.ValidateBaseline() == nil {
		t.Fatal("baseline without commands")
	}
	c.VerificationCommands = []string{"go test ./..."}
	if err := c.ValidateBaseline(); err != nil {
		t.Fatal(err)
	}
	for key := range c.Roles {
		c.Roles[key] = NewRoute("available", "low")
	}
	for key := range c.Tiers {
		c.Tiers[key] = NewRoute("available", "low")
	}
	c.RepairRoute = NewRoute("available", "low")
	if err := c.Validate(true); err != nil {
		t.Fatal(err)
	}
	c.GitHubRepo = "owner/repo/extra"
	if c.Validate(true) == nil {
		t.Fatal("invalid repository accepted")
	}
}
func TestRepositoryValidationPreservesSymlinkParent(t *testing.T) {
	for _, test := range []struct {
		name            string
		gitFile         bool
		gitAtLinkParent bool
	}{
		{name: "checkout"},
		{name: "worktree", gitFile: true},
		{name: "reject_lexical_checkout", gitAtLinkParent: true},
		{name: "reject_lexical_worktree", gitFile: true, gitAtLinkParent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			repository := filepath.Join(root, "repository")
			child := filepath.Join(repository, "child")
			if err := os.MkdirAll(child, 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(child, link); err != nil {
				t.Fatal(err)
			}
			gitPath := filepath.Join(repository, ".git")
			if test.gitAtLinkParent {
				gitPath = filepath.Join(root, ".git")
			}
			if test.gitFile {
				if err := os.WriteFile(gitPath, []byte("gitdir: /synthetic/worktree\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(gitPath, 0700); err != nil {
				t.Fatal(err)
			}
			c := Default()
			// filepath.Join would erase the symlink/.. component under test.
			c.Repository = link + string(os.PathSeparator) + ".."
			c.GitHubRepo = "fixture/project"
			c.VerificationCommands = []string{"go test ./..."}
			if err := c.ValidateBaseline(); (err == nil) == test.gitAtLinkParent {
				t.Fatalf("ValidateBaseline() = %v; want valid = %t", err, !test.gitAtLinkParent)
			}
		})
	}
}

func TestBranchValidation(t *testing.T) {
	for _, s := range []string{"", "-x", "a..b", "main:evil", "a.lock", "a/../b", "a//b", ".hidden", "a/.hidden", "a/lock.lock", "main.", "main/", "a@{x}", "é"} {
		if ValidBranch(s) {
			t.Errorf("valid: %q", s)
		}
	}
	for _, s := range []string{"octomus/task-123", "tyk/task-123", "main", "feature/a_b.c"} {
		if !ValidBranch(s) {
			t.Errorf("invalid: %q", s)
		}
	}
}
func TestSnapshotsOwnRoutesAndContainers(t *testing.T) {
	c := Default()
	provider := "provider"
	variant := "high"
	c.Roles["discovery"] = Route{Backend: BackendOpencode, Model: "model", Provider: &provider, Variant: &variant}
	c.RunnerStoragePaths["codex"] = "/owned"
	snapshot := c.Clone()
	snapshot.Categories[0] = "mutated"
	snapshot.VerificationCommands = append(snapshot.VerificationCommands, "false")
	snapshot.RunnerStoragePaths["codex"] = "/changed"
	route := snapshot.Roles["discovery"]
	*route.Provider = "other"
	*route.Variant = "low"
	snapshot.Roles["orchestrator"] = NewRoute("changed", "low")
	routes := c.RoutesFor(true)
	for _, r := range routes {
		if r.Route.Provider != nil {
			*r.Route.Provider = "third"
		}
	}
	if c.Categories[0] != "features" || c.RunnerStoragePaths["codex"] != "/owned" || *c.Roles["discovery"].Provider != "provider" || *c.Roles["discovery"].Variant != "high" || c.Roles["orchestrator"].Model != "" {
		t.Fatal("snapshot aliases its owner")
	}
	d := Default()
	d.Roles["discovery"] = NewRoute("changed", "low")
	if Default().Roles["discovery"].Model != "" {
		t.Fatal("shared defaults")
	}
}
func TestAbsentAndEmptyMapsAreDifferent(t *testing.T) {
	var missing, empty Config
	if err := json.Unmarshal([]byte(`{}`), &missing); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"roles":{},"tiers":{}}`), &empty); err != nil {
		t.Fatal(err)
	}
	if len(missing.Roles) != 4 || len(empty.Roles) != 0 || len(empty.Tiers) != 0 || empty.Validate(false) == nil {
		t.Fatal("empty maps inherited defaults")
	}
	original := missing.Clone()
	if json.Unmarshal([]byte(`{"roles":null}`), &missing) == nil {
		t.Fatal("null accepted")
	}
	if !reflect.DeepEqual(original, missing) {
		t.Fatal("failed load mutated snapshot")
	}
}
func TestValidationNumericBoundaries(t *testing.T) {
	for _, test := range []struct {
		field     string
		low, high uint64
	}{
		{"discovery_agents", 8, 10}, {"execution_concurrency", 1, 8}, {"max_tasks_per_cycle", 1, 20}, {"max_repair_rounds", 1, 20}, {"max_retries", 0, 10}, {"cycle_interval_seconds", 30, 604800}, {"maintenance_every_cycles", 1, 10000}, {"max_sessions_per_day", 1, 1000000}, {"max_open_prs", 1, 1000}, {"max_workspace_bytes", 1000000, 1000000000000000}, {"retain_completed_days", 1, 36500}, {"retain_events", 100, 100000},
	} {
		for _, n := range []uint64{test.low, test.high, test.high + 1} {
			data, _ := json.Marshal(map[string]uint64{test.field: n})
			var c Config
			if err := json.Unmarshal(data, &c); err != nil {
				t.Fatal(err)
			}
			if (c.Validate(false) == nil) != (n <= test.high) {
				t.Errorf("%s=%d", test.field, n)
			}
		}
		if test.low > 0 {
			data, _ := json.Marshal(map[string]uint64{test.field: test.low - 1})
			var c Config
			_ = json.Unmarshal(data, &c)
			if c.Validate(false) == nil {
				t.Errorf("below %s", test.field)
			}
		}
	}
	c := Default()
	c.TaskTimeoutSeconds = 604800
	c.SessionTimeoutSeconds = 604800
	c.CommandTimeoutSeconds = 604800
	if err := c.Validate(false); err != nil {
		t.Fatal(err)
	}
	c.SessionTimeoutSeconds++
	if c.Validate(false) == nil {
		t.Fatal("time limit")
	}
	c = Default()
	c.RepairRoute.Model = strings.Repeat("ü", 51)
	if c.Validate(false) == nil {
		t.Fatal("route length counted characters, not bytes")
	}
}

func TestRepositoryIdentity(t *testing.T) {
	base := Default()
	base.Repository = "/srv/projects/source"
	base.GitHubRepo = "Fixture/Project"
	for _, tc := range []struct {
		name, path, remote, branch string
		want                       bool
	}{
		{"same", "/srv/projects//source/.", "fixture/project", "main", true},
		{"different path", "/srv/projects/other", "fixture/project", "main", false},
		{"different remote", "/srv/projects/source", "fixture/other", "main", false},
		{"different branch", "/srv/projects/source", "fixture/project", "develop", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base.Clone()
			other.Repository, other.GitHubRepo, other.DefaultBranch = tc.path, tc.remote, tc.branch
			if got := base.SameRemoteIdentity(other); got != tc.want {
				t.Fatalf("SameRemoteIdentity() = %t; want %t", got, tc.want)
			}
		})
	}
}
