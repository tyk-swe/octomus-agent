package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	names := func(routes []NamedRoute) []string {
		result := []string{}
		for _, route := range routes {
			result = append(result, route.Name)
		}
		return result
	}
	if got, want := names(c.RoutesFor(true)), []string{"discovery", "orchestrator", "proposal_reviewer"}; !slices.Equal(got, want) {
		t.Fatalf("audit routes = %v; want %v", got, want)
	}
	if got, want := names(c.RoutesFor(false)), []string{"code_reviewer", "discovery", "orchestrator", "proposal_reviewer", "L", "M", "S", "XL", "XS", "repair"}; !slices.Equal(got, want) {
		t.Fatalf("execution routes = %v; want %v", got, want)
	}
	for _, route := range c.RoutesFor(false) {
		want := c.RepairRoute
		if role, ok := c.Roles[route.Name]; ok {
			want = role
		} else if tier, ok := c.Tiers[route.Name]; ok {
			want = tier
		}
		if route.Route != want {
			t.Fatalf("%s route = %+v; want %+v", route.Name, route.Route, want)
		}
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
		message   string
	}{
		{"discovery_agents", 8, 10, "Discovery requires 8–10 agents"},
		{"execution_concurrency", 1, 8, "Execution concurrency must be 1–8"},
		{"max_tasks_per_cycle", 1, 20, "Tasks per cycle must be 1–20"},
		{"max_repair_rounds", 1, 20, "Repair rounds must be 1–20"},
		{"max_retries", 0, 10, "Retry limit must be at most 10"},
		{"cycle_interval_seconds", 30, 604800, "Cycle interval must be 30–604800 seconds"},
		{"maintenance_every_cycles", 1, 10000, "Maintenance cadence must be 1–10000 cycles"},
		{"command_timeout_seconds", 1, 604800, "Command timeout must be 1–604800 seconds"},
		{"max_sessions_per_day", 1, 1000000, "Daily session budget must be 1–1000000"},
		{"max_open_prs", 1, 1000, "Open PR capacity must be 1–1000"},
		{"max_workspace_bytes", 1000000, 1000000000000000, "Workspace budget must be 1000000–1000000000000000 bytes"},
		{"retain_completed_days", 1, 36500, "Workspace retention must be 1–36500 days"},
		{"retain_events", 100, 100000, "Retained activity events must be 100–100000"},
	} {
		values := []uint64{test.low, test.high, test.high + 1}
		if test.low > 0 {
			values = append(values, test.low-1)
		}
		for _, n := range values {
			data, _ := json.Marshal(map[string]uint64{test.field: n})
			var c Config
			if err := json.Unmarshal(data, &c); err != nil {
				t.Fatal(err)
			}
			err := c.Validate(false)
			if valid := n >= test.low && n <= test.high; valid != (err == nil) {
				t.Errorf("%s=%d: %v", test.field, n, err)
			} else if !valid && err.Error() != test.message {
				t.Errorf("%s=%d: %q; want %q", test.field, n, err, test.message)
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

// Each rejected setting names its field and, for ranges, what it accepts.
func TestValidationNamesTheFailingSetting(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{"no-progress rounds", func(c *Config) { c.MaxNoProgressRounds = 0 }, "No-progress rounds must be at least 1"},
		{"session below range", func(c *Config) { c.SessionTimeoutSeconds = 9 }, "Session timeout must be 10–604800 seconds"},
		{"session above range", func(c *Config) { c.SessionTimeoutSeconds, c.TaskTimeoutSeconds = 604801, 604801 }, "Session timeout must be 10–604800 seconds"},
		{"task below session", func(c *Config) { c.SessionTimeoutSeconds, c.TaskTimeoutSeconds = 1800, 900 }, "Task timeout must be at least the session timeout and at most 604800 seconds"},
		{"task above range", func(c *Config) { c.TaskTimeoutSeconds = 604801 }, "Task timeout must be at least the session timeout and at most 604800 seconds"},
		{"default branch", func(c *Config) { c.DefaultBranch = "main." }, "Default branch must be a valid branch name"},
		{"prefix without slash", func(c *Config) { c.BranchPrefix = "octomus" }, `Owned branch prefix must be a valid branch path ending in "/"`},
		{"invalid prefix", func(c *Config) { c.BranchPrefix = "bad..x/" }, `Owned branch prefix must be a valid branch path ending in "/"`},
		{"prefix covers default", func(c *Config) { c.DefaultBranch = "octomus/main" }, "Owned branch prefix must exclude the default branch"},
		{"extra tier", func(c *Config) { c.Tiers["XXL"] = NewRoute("", "") }, "Configure exactly the five execution tiers (XS, S, M, L, XL)"},
		{"missing tier", func(c *Config) { delete(c.Tiers, "XL") }, "Configure exactly the five execution tiers (XS, S, M, L, XL)"},
		{"missing role", func(c *Config) { delete(c.Roles, "code_reviewer") }, "Configure exactly the four planning and review roles (orchestrator, discovery, proposal_reviewer, code_reviewer)"},
		{"blank command", func(c *Config) { c.VerificationCommands = []string{"go test ./...", " "} }, "Each verification command must be non-empty and at most 4096 bytes"},
		{"long command", func(c *Config) { c.VerificationCommands = []string{strings.Repeat("x", 4097)} }, "Each verification command must be non-empty and at most 4096 bytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Default()
			test.mutate(&c)
			if err := c.Validate(false); err == nil || err.Error() != test.message {
				t.Fatalf("Validate(false) = %v; want %q", err, test.message)
			}
		})
	}
	c := Default()
	c.VerificationCommands = []string{strings.Repeat("x", 4096)}
	if err := c.Validate(false); err != nil {
		t.Fatalf("a 4096-byte command: %v", err)
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

// Route errors name the route and the component that failed.
func TestRouteErrorsNameTheRouteAndComponent(t *testing.T) {
	text := func(s string) *string { return &s }
	for _, test := range []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{"model spacing", func(c *Config) { c.Tiers["L"] = NewRoute("gpt-5 ", "low") },
			"L route: Model must be at most 100 bytes with no surrounding spaces or control characters"},
		{"codex model length", func(c *Config) { c.Roles["discovery"] = NewRoute(strings.Repeat("m", 101), "low") },
			"discovery route: Model must be at most 100 bytes with no surrounding spaces or control characters"},
		{"opencode model length", func(c *Config) {
			c.Roles["orchestrator"] = Route{Backend: BackendOpencode, Model: strings.Repeat("m", 513), Provider: text("provider")}
		}, "orchestrator route: Model must be at most 512 bytes with no surrounding spaces or control characters"},
		{"effort length", func(c *Config) { c.Tiers["XS"] = NewRoute("model", strings.Repeat("e", 21)) },
			"XS route: Effort must be at most 20 bytes with no surrounding spaces or control characters"},
		{"provider spacing", func(c *Config) {
			c.Roles["code_reviewer"] = Route{Backend: BackendOpencode, Model: "model", Provider: text(" x")}
		}, "code_reviewer route: Provider must be at most 100 bytes with no surrounding spaces or control characters"},
		{"empty variant", func(c *Config) {
			c.RepairRoute = Route{Backend: BackendOpencode, Model: "model", Provider: text("provider"), Variant: text("")}
		}, "repair route: Variant must be a non-empty name of at most 100 bytes with no surrounding spaces or control characters"},
		{"codex variant", func(c *Config) {
			c.RepairRoute = Route{Backend: BackendCodex, Model: "model", Effort: "low", Variant: text("high")}
		}, "repair route: Codex routes use reasoning effort, not an OpenCode provider or variant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Default()
			test.mutate(&c)
			if err := c.Validate(false); err == nil || err.Error() != test.message {
				t.Fatalf("Validate(false) = %v; want %q", err, test.message)
			}
		})
	}
	c := Default()
	c.Roles["orchestrator"] = Route{Backend: BackendOpencode, Model: strings.Repeat("m", 512), Provider: text("provider"), Variant: text("high")}
	if err := c.Validate(false); err != nil {
		t.Fatalf("a 512-byte OpenCode model: %v", err)
	}
	if err := Default().ValidateAudit(); err == nil || err.Error() != "discovery route: Set the Codex model and effort for every required route" {
		t.Fatalf("ValidateAudit() = %v; want the first unset planning route named", err)
	}
}
