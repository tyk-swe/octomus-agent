// Policy defaults, exact routes, validation bounds and the strict JSON contract of saved configuration.

package config

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestDefaultRoutes(t *testing.T) {
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
	if len(c.RoutesFor(true)) != 3 || len(c.RoutesFor(false)) != 10 || c.PlanningCost() != 13 {
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

func TestConfigValidation(t *testing.T) {
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
		{"auto_merge_max_lines", 1, 10000, "Auto-merge line limit must be 1–10000"},
		{"auto_merge_max_files", 1, 100, "Auto-merge file limit must be 1–100"},
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
	for _, test := range []struct {
		field, value, message string
	}{
		{"default_branch", "refs/heads/main", "Default branch"},
		{"default_branch", strings.Repeat("aB09", 10), "Default branch"},
		{"branch_prefix", "refs/tasks/", "Owned branch prefix"},
	} {
		data, _ := json.Marshal(map[string]string{test.field: test.value})
		var c Config
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(false); err == nil || !strings.Contains(err.Error(), test.message) {
			t.Errorf("%s=%q: %v; want %q", test.field, test.value, err, test.message)
		}
	}
}

func TestConfigJSONContract(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"github_repo":"fixture/project"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubRepo != "fixture/project" || cfg.DiscoveryAgents != Default().DiscoveryAgents {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"github_repo"`, `"roles"`, `"tiers"`, `"verification_commands"`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("missing %s in %s", field, data)
		}
	}
	for _, raw := range []string{`{"unknown":1}`, `{"github_repo":"a","github_repo":"b"}`, `{"roles":null}`} {
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestDeliveryModeDefaultsStandardAndEffectiveCategories(t *testing.T) {
	cfg := Default()
	if cfg.DeliveryMode != DeliveryModeStandard {
		t.Fatalf("default delivery mode = %v", cfg.DeliveryMode)
	}
	if got := cfg.EffectiveCategories(); len(got) != len(cfg.Categories) {
		t.Fatalf("standard effective categories = %v", got)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"delivery_mode":"standard"`) ||
		!strings.Contains(string(data), `"auto_merge_max_lines":500`) ||
		!strings.Contains(string(data), `"auto_merge_max_files":10`) {
		t.Fatalf("delivery defaults missing from %s", data)
	}
	cfg.DeliveryMode = DeliveryModeMaintenance
	if err := cfg.Validate(false); err != nil {
		t.Fatal(err)
	}
	got := cfg.EffectiveCategories()
	if slices.Contains(got, "features") || len(got) != len(cfg.Categories)-1 {
		t.Fatalf("maintenance effective categories = %v", got)
	}
	data, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"delivery_mode":"maintenance"`) {
		t.Fatalf("maintenance mode missing from %s", data)
	}
	var decoded Config
	if err := json.Unmarshal([]byte(`{"delivery_mode":"maintenance"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.DeliveryMode != DeliveryModeMaintenance {
		t.Fatalf("decoded mode = %v", decoded.DeliveryMode)
	}
	if err := json.Unmarshal([]byte(`{"delivery_mode":"turbo"}`), &decoded); err == nil {
		t.Fatal("accepted an unknown delivery mode")
	}
	only := Default()
	only.Categories = []string{"features"}
	only.DeliveryMode = DeliveryModeMaintenance
	if err := only.Validate(false); err == nil || !strings.Contains(err.Error(), "non-feature category") {
		t.Fatalf("features-only maintenance = %v", err)
	}
	only.DeliveryMode = DeliveryModeStandard
	if err := only.Validate(false); err != nil {
		t.Fatalf("features-only standard must stay valid: %v", err)
	}
}

func TestAutoMergeExcludedPathsValidation(t *testing.T) {
	cfg := Default()
	valid := []string{"internal/access/", "docs", "generated/bindings/", "dir.with.dots/", "a-b_c"}
	for _, entry := range valid {
		cfg.AutoMergeExcludedPaths = []string{entry}
		if err := cfg.Validate(false); err != nil {
			t.Fatalf("valid exclusion %q rejected: %v", entry, err)
		}
	}
	for _, entry := range []string{"../up", "/abs/path", "has space", "has\ttab", "has\nline", "glob/*", "expr{a,b}", "x$var", "a\\b", "q?", "b[0]"} {
		cfg.AutoMergeExcludedPaths = []string{entry}
		if err := cfg.Validate(false); err == nil {
			t.Fatalf("invalid exclusion %q accepted", entry)
		}
	}
	for _, pair := range [][2]string{{"foo", "foo/"}, {"internal/", "internal/"}} {
		cfg.AutoMergeExcludedPaths = []string{pair[0], pair[1]}
		if err := cfg.Validate(false); err == nil {
			t.Fatalf("duplicate normalized exclusions %v accepted", pair)
		}
	}
	cfg.AutoMergeExcludedPaths = nil
	if err := cfg.Validate(false); err != nil {
		t.Fatal(err)
	}
}
