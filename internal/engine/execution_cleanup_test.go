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
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

func TestExecutionStartFailureKeepsRunnerCleanupError(t *testing.T) {
	t.Parallel()
	for stage, role := range []string{"executor", "reviewer", "repair"} {
		t.Run(role, func(t *testing.T) {
			fixture := newScriptedFixture(t)
			routes, script := fixture.routes, fixture.script
			route := []config.Route{routes.Executor, routes.Reviewer, routes.Repair}[stage]
			if stage > 0 {
				script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "draft\n")})
			}
			if stage > 1 {
				script.Answer(routes.Reviewer, `{"completed":true,"summary":"One finding","findings":[{"title":"Finish the output","file":"feature.txt:1","detail":"The output is still a draft.","priority":"P1"}]}`)
			}
			startErr := errors.New("fixture " + role + " start failed")
			cleanupErr := &sandbox.SandboxError{Err: errors.New("fixture runner end is unconfirmed")}
			script.FailStart(route, startErr)
			// Route preflight and any earlier successful turns close before this start.
			for range stage + 1 {
				script.FailClose(route.Backend, nil)
			}
			script.FailClose(route.Backend, cleanupErr)
			task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
			saveExecutionTask(t, fixture.planningFixture, task)
			err := fixture.pausedApp(t).execute(context.Background(), &task)
			if !errors.Is(err, startErr) || !errors.Is(err, cleanupErr) {
				t.Fatalf("failed %s start omitted an error: %v", role, err)
			}
			if !sandbox.Infrastructure(err) || model.BlockedReasonFromError(err) != model.BlockedReasonRunnerUnavailable {
				t.Fatalf("failed start lost its runner-unavailable classification: %v", err)
			}
			if strings.Count(err.Error(), cleanupErr.Error()) != 1 {
				t.Fatalf("failed start duplicated cleanup diagnostics: %v", err)
			}
			if len(script.Turns(route)) != 0 || len(task.Sessions) != stage {
				t.Fatalf("failed start began a turn or recorded an unstarted session: %+v", task.Sessions)
			}
			closed := 0
			for _, call := range script.Calls() {
				if call.Kind == runnertest.CallClose {
					closed++
				}
			}
			if closed != stage+2 {
				t.Fatalf("closed %d runners; want %d", closed, stage+2)
			}
			assertNoOpenClients(t, script)
			assertUnpublished(t, fixture, task)
			assertAdmissions(t, fixture.state, uint64(stage+1), "successful turns plus failed start")
		})
	}
}

func TestExecutionRecordsCodexExitBeforeSessionStarts(t *testing.T) {
	t.Parallel()
	fixture := newExecutionFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.root, "codex-mode"), []byte("exit-on-start"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := executionTask(t, fixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture, task)
	app := newExecutionApp(t, fixture)
	saved := driveTask(t, fixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.Error == nil {
		t.Fatalf("Codex exit did not block execution: %+v", saved)
	}
	for _, want := range []string{"Codex app-server disconnected", "exit status 37"} {
		if !strings.Contains(*saved.Error, want) {
			t.Errorf("execution error omitted %q: %s", want, *saved.Error)
		}
	}
	if len(saved.Sessions) != 0 || saved.ExecutionSession != nil {
		t.Fatalf("failed start recorded an unstarted session: %+v", saved.Sessions)
	}
	if saved.OutputCommit != nil || len(publications(t, fixture)) != 0 {
		t.Fatal("failed start published work")
	}
	assertAdmissions(t, fixture.state, 1, "failed executor session start")
}
