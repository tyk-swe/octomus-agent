package config

import (
	"encoding/json"
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
