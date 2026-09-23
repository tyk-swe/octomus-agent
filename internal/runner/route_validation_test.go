package runner_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
)

// codexCatalog maps each model to its supported efforts and serves it through
// a scripted adapter, so whole-configuration route validation runs without a
// runner process. The scripted adapter carries no replies: validation must
// never start a session or run a turn.
func codexCatalog(entries map[string][]string) *runner.Runners {
	models := []runner.Model{}
	for name, efforts := range entries {
		models = append(models, runnertest.CodexModel(name, efforts...))
	}
	script := runnertest.New(models...)
	return runner.New(context.Background(), config.Default(), nil, "fixture", script.Connector())
}

func TestUnsupportedEffortNeverFallsBack(t *testing.T) {
	c := config.Default()
	for _, role := range []string{"orchestrator", "discovery", "proposal_reviewer", "code_reviewer"} {
		c.Roles[role] = config.NewRoute("gpt-6-astra", "medium")
	}
	c.Tiers = map[string]config.Route{
		"XS": config.NewRoute("gpt-5.6-luna", "xhigh"),
		"S":  config.NewRoute("gpt-5.6-luna", "max"),
		"M":  config.NewRoute("gpt-6-astra", "low"),
		"L":  config.NewRoute("gpt-6-astra", "medium"),
		"XL": config.NewRoute("gpt-6-astra", "high"),
	}
	c.RepairRoute = config.NewRoute("gpt-6-astra", "medium")
	r := codexCatalog(map[string][]string{"gpt-6-astra": {"medium", "low", "high"}, "gpt-5.6-luna": {"xhigh"}})
	err := r.ValidateRoutes(c, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("unsupported tier S effort must fail naming it, got %v", err)
	}
	if c.Tiers["S"].Effort != "max" {
		t.Fatalf("validation substituted the tier S effort: %q", c.Tiers["S"].Effort)
	}
}

func TestRepairRoutesAreValidated(t *testing.T) {
	c := config.Default()
	for role := range c.Roles {
		c.Roles[role] = config.NewRoute("available", "low")
	}
	for tier := range c.Tiers {
		c.Tiers[tier] = config.NewRoute("available", "low")
	}
	r := codexCatalog(map[string][]string{"available": {"low"}})
	// The repair route is validated against the catalog like any other route,
	// even when every role and tier around it is satisfiable.
	c.RepairRoute = config.NewRoute("gpt-6-astra", "medium")
	if err := r.ValidateRoutes(c, t.TempDir(), false); err == nil || !strings.Contains(err.Error(), "gpt-6-astra / medium") {
		t.Fatalf("unavailable repair model must fail, got %v", err)
	}
	c.RepairRoute = config.NewRoute("available", "high")
	if err := r.ValidateRoutes(c, t.TempDir(), false); err == nil || !strings.Contains(err.Error(), "available / high") {
		t.Fatalf("unsupported repair effort must fail, got %v", err)
	}
	c.RepairRoute.Effort = "low"
	if err := r.ValidateRoutes(c, t.TempDir(), false); err != nil {
		t.Fatalf("satisfiable repair route: %v", err)
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded config.Config
	if err := json.Unmarshal(data, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.RepairRoute != c.RepairRoute {
		t.Fatalf("repair route did not round-trip: %+v", reloaded.RepairRoute)
	}
	c.RepairRoute.Model = strings.Repeat("x", 101)
	if c.Validate(false) == nil {
		t.Fatal("an overlong repair model must fail configuration validation")
	}
}

func TestAuditReadinessRequiresOnlyPlanningRoutesAndNoVerification(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.Repository = repository
	c.GitHubRepo = "fixture/project"
	for _, role := range []string{"orchestrator", "discovery", "proposal_reviewer"} {
		c.Roles[role] = config.NewRoute("available", "low")
	}
	r := codexCatalog(map[string][]string{"available": {"low"}})
	if err := c.ValidateAudit(); err != nil {
		t.Fatalf("audit readiness: %v", err)
	}
	if c.Validate(true) == nil {
		t.Fatal("execution readiness must require verification and execution routes")
	}
	if err := r.ValidateRoutes(c, repository, true); err != nil {
		t.Fatalf("audit routes: %v", err)
	}
	if r.ValidateRoutes(c, repository, false) == nil {
		t.Fatal("execution routes must fail while unset")
	}
	discovery := c.Roles["discovery"]
	discovery.Effort = "max"
	c.Roles["discovery"] = discovery
	if r.ValidateRoutes(c, repository, true) == nil {
		t.Fatal("an unsupported discovery effort must fail the audit route check")
	}
	discovery.Model = ""
	c.Roles["discovery"] = discovery
	if c.ValidateAudit() == nil {
		t.Fatal("a cleared discovery model must fail audit readiness")
	}
}
