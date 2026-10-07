// The scripted runner adapter holds the same structured-output and route checks as the production adapters.

package runnertest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

var reviewer = config.NewRoute("reviewer", "high")

func runners(ctx context.Context, script *runnertest.Script) *runner.Runners {
	return runner.New(ctx, config.Default(), script.Connector())
}

func TestStructuredAnswerChecks(t *testing.T) {
	script := runnertest.New(runnertest.CatalogFor(reviewer)...)
	script.Answer(reviewer, `{"completed": true`, `{"completed": "yes", "summary": "", "findings": []}`)
	clients := runners(context.Background(), script)
	defer clients.Close()
	ws := t.TempDir()
	for _, want := range []string{"invalid JSON", "invalid structured result"} {
		session, err := clients.Start(reviewer, ws, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = clients.Turn(session, reviewer, ws, "review", schemas.ReviewSchema(), nil)
		if !errors.Is(err, model.BlockedRunnerUnavailable) || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
	}
}

func TestUnsupportedEffortNoFallback(t *testing.T) {
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
	r := runners(context.Background(), runnertest.New(
		runnertest.CodexModel("gpt-6-astra", "medium", "low", "high"),
		runnertest.CodexModel("gpt-5.6-luna", "xhigh")))
	defer r.Close()
	err := r.ValidateRoutes(c, t.TempDir(), false)
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("unsupported tier S effort must fail naming it, got %v", err)
	}
	if c.Tiers["S"].Effort != "max" {
		t.Fatalf("validation substituted the tier S effort: %q", c.Tiers["S"].Effort)
	}
}
