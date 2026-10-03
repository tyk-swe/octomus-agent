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

var (
	reviewer = config.NewRoute("reviewer", "high")
)

func runners(ctx context.Context, script *runnertest.Script) *runner.Runners {
	return runner.New(ctx, config.Default(), script.Connector())
}

func TestStructuredAnswersAreCheckedLikeProductionAdapters(t *testing.T) {
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
		_, err = clients.Turn(session, reviewer, ws, "review", schemas.ReviewSchema())
		if !errors.Is(err, model.BlockedReasonRunnerUnavailable) || !strings.Contains(err.Error(), want) {
			t.Fatalf("got %v, want %q", err, want)
		}
	}
}
