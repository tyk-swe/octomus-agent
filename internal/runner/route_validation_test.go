package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
)

func codexCatalog(entries map[string][]string) *runner.Runners {
	models := []runner.Model{}
	for name, efforts := range entries {
		models = append(models, runnertest.CodexModel(name, efforts...))
	}
	script := runnertest.New(models...)
	return runner.New(context.Background(), config.Default(), script.Connector())
}

func TestUnsupportedEffortNeverFallsBack(t *testing.T) {
	t.Parallel()
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
