package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

func TestPlanningStartFailureKeepsRunnerCleanupError(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	route := config.NewRoute("scripted-discovery", "medium")
	script := runnertest.New(runnertest.CatalogFor(route)...)
	startErr := errors.New("fixture session start failed")
	cleanupErr := &sandbox.SandboxError{Err: errors.New("fixture runner end is unconfirmed")}
	script.FailStart(route, startErr)
	script.FailClose(route.Backend, cleanupErr)
	clients := runner.New(context.Background(), config.Default(), script.Connector())

	answer, err := app.invoke(context.Background(), clients, invocation{
		cycleID: "cycle", role: "discovery-0", route: route, workspace: t.TempDir(),
		prompt: "Discover", ownsClients: true,
	})
	if answer != "" || !errors.Is(err, startErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("failed session start = %q, %v; want both the start and cleanup errors", answer, err)
	}
	if !sandbox.Infrastructure(err) || model.BlockedReasonFromError(err) != model.BlockedReasonRunnerUnavailable {
		t.Fatalf("failed session start lost its runner-unavailable classification: %v", err)
	}
	if turns := script.Turns(route); len(turns) != 0 {
		t.Fatalf("failed start reached a turn: %+v", turns)
	}
	if strings.Count(err.Error(), cleanupErr.Error()) != 1 {
		t.Fatalf("failed start duplicated its cleanup error: %v", err)
	}
	closed := 0
	for _, call := range script.Calls() {
		if call.Kind == runnertest.CallClose {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("failed start closed its runner %d times; want once", closed)
	}
	assertNoOpenClients(t, script)
}

func TestPlanningRecordsCodexExitBeforeSessionStarts(t *testing.T) {
	t.Parallel()
	fixture := newPlanningFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.root, "codex-mode"), []byte("exit-on-start"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	if cycle.Status != model.CycleFailed || cycle.Error == nil {
		t.Fatalf("Codex exit did not fail planning: %+v", cycle)
	}
	for _, want := range []string{"Codex app-server disconnected", "exit status 37"} {
		if !strings.Contains(*cycle.Error, want) {
			t.Errorf("planning error omitted %q: %s", want, *cycle.Error)
		}
	}
	if len(cycle.Sessions) != 0 {
		t.Fatalf("failed start recorded an unstarted session: %+v", cycle.Sessions)
	}
	assertAdmissions(t, fixture.state, 1, "failed grounding session start")
}
