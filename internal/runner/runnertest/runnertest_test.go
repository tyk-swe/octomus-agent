package runnertest_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
)

var (
	worker   = config.NewRoute("worker", "medium")
	reviewer = config.NewRoute("reviewer", "high")
)

func runners(ctx context.Context, script *runnertest.Script) *runner.Runners {
	return runner.New(ctx, config.Default(), script.Connector())
}

func TestRoutesMissingFromTheCatalogAreRejectedAsRunnerUnavailable(t *testing.T) {
	script := runnertest.New(runnertest.CatalogFor(worker)...)
	clients := runners(context.Background(), script)
	defer clients.Close()
	for _, route := range []config.Route{reviewer, config.NewRoute("worker", "high")} {
		_, err := clients.Start(route, t.TempDir(), nil)
		if !errors.Is(err, model.BlockedReasonRunnerUnavailable) || !strings.Contains(err.Error(), route.String()) {
			t.Fatalf("route %s: got %v", route, err)
		}
	}
	if starts := script.Starts(reviewer); len(starts) != 0 {
		t.Fatalf("an unavailable route reached the adapter: %+v", starts)
	}
	// The adapter itself refuses an absent route, independent of Runners.
	adapter, err := script.Connector()(context.Background(), config.BackendCodex, config.Default(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Start(reviewer, t.TempDir(), nil); err == nil {
		t.Fatal("the scripted adapter started a route absent from its catalog")
	}
}

func TestRepliesAreServedPerRouteInOrderWithWorkspaceEffects(t *testing.T) {
	script := runnertest.New(runnertest.CatalogFor(worker, reviewer)...)
	ws := t.TempDir()
	script.Queue(worker,
		runnertest.Reply{Answer: "first", Effect: func(cwd string) error {
			return os.WriteFile(filepath.Join(cwd, "feature.txt"), []byte("fixed\n"), 0o644)
		}},
		runnertest.Reply{Answer: "second"})
	script.Answer(reviewer, `{"completed": true, "summary": "clean", "findings": []}`)
	clients := runners(context.Background(), script)
	session, err := clients.Start(worker, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := clients.Start(reviewer, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := clients.Turn(review, reviewer, ws, "review", schemas.ReviewSchema()); err != nil || answer != `{"completed":true,"findings":[],"summary":"clean"}` {
		t.Fatalf("structured answer = %q, %v", answer, err)
	}
	for _, want := range []string{"first", "second"} {
		if answer, err := clients.Turn(session, worker, ws, "implement "+want, nil); err != nil || answer != want {
			t.Fatalf("turn = %q, %v; want %q", answer, err, want)
		}
	}
	if data, err := os.ReadFile(filepath.Join(ws, "feature.txt")); err != nil || string(data) != "fixed\n" {
		t.Fatalf("effect did not edit the workspace: %q, %v", data, err)
	}
	if _, err := clients.Turn(session, worker, ws, "unscripted", nil); err == nil || !strings.Contains(err.Error(), "no scripted reply") {
		t.Fatalf("an exhausted queue must fail the turn, got %v", err)
	}
	if err := clients.Close(); err != nil {
		t.Fatal(err)
	}
	turns := script.Turns(worker)
	if len(turns) != 3 || turns[0].Prompt != "implement first" || turns[0].Session != session || turns[0].Cwd != ws || turns[0].Schema != nil {
		t.Fatalf("worker turns = %+v", turns)
	}
	if reviews := script.Turns(reviewer); len(reviews) != 1 || reviews[0].Schema == nil || reviews[0].Session == session {
		t.Fatalf("reviewer turns = %+v", reviews)
	}
	if script.Pending(worker) != 0 || script.Pending(reviewer) != 0 || script.OpenClients() != 0 {
		t.Fatalf("pending=%d/%d open=%d", script.Pending(worker), script.Pending(reviewer), script.OpenClients())
	}
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

func TestResumeRequiresAKnownSessionAndIsRecorded(t *testing.T) {
	script := runnertest.New(runnertest.CatalogFor(worker)...)
	script.Answer(worker, "done")
	ws := t.TempDir()
	first := runners(context.Background(), script)
	session, err := first.Start(worker, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := runners(context.Background(), script)
	defer second.Close()
	if _, err := second.Turn(session, worker, ws, "turn", nil); err == nil {
		t.Fatal("a turn on a session not started or resumed on this client must fail")
	}
	unknown := "never-started"
	if _, err := second.Start(worker, ws, &unknown); err == nil {
		t.Fatal("resuming an unknown session must fail")
	}
	resumed, err := second.Start(worker, ws, &session)
	if err != nil || resumed != session {
		t.Fatalf("resume = %q, %v", resumed, err)
	}
	if answer, err := second.Turn(session, worker, ws, "turn", nil); err != nil || answer != "done" {
		t.Fatalf("resumed turn = %q, %v", answer, err)
	}
	starts := script.Starts(worker)
	if len(starts) != 3 || starts[0].Resume != nil || starts[2].Resume == nil || *starts[2].Resume != session || starts[0].Client == starts[2].Client {
		t.Fatalf("starts = %+v", starts)
	}
}

func TestInjectedErrorsSurfaceAtConnectStartTurnAndClose(t *testing.T) {
	script := runnertest.New(runnertest.CatalogFor(worker)...)
	ws := t.TempDir()
	script.FailConnect(config.BackendCodex, errors.New("connect refused"))
	clients := runners(context.Background(), script)
	if _, err := clients.Start(worker, ws, nil); !errors.Is(err, model.BlockedReasonRunnerUnavailable) || !strings.Contains(err.Error(), "connect refused") {
		t.Fatalf("connect failure = %v", err)
	}
	clients = runners(context.Background(), script)
	script.FailStart(worker, errors.New("start refused"))
	if _, err := clients.Start(worker, ws, nil); !errors.Is(err, model.BlockedReasonRunnerUnavailable) || !strings.Contains(err.Error(), "start refused") {
		t.Fatalf("start failure = %v", err)
	}
	session, err := clients.Start(worker, ws, nil)
	if err != nil {
		t.Fatalf("an injected start failure must apply once: %v", err)
	}
	script.Queue(worker, runnertest.Reply{Err: errors.New("turn crashed")})
	if _, err := clients.Turn(session, worker, ws, "turn", nil); !errors.Is(err, model.BlockedReasonRunnerUnavailable) || !strings.Contains(err.Error(), "turn crashed") {
		t.Fatalf("turn failure = %v", err)
	}
	script.FailClose(config.BackendCodex, errors.New("close failed"))
	if err := clients.Close(); err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("close failure = %v", err)
	}
	if script.OpenClients() != 0 {
		t.Fatalf("a failed close still releases the client, open=%d", script.OpenClients())
	}
}

func TestGatedTurnsBlockUntilReleasedOrCancelled(t *testing.T) {
	script := runnertest.New(runnertest.CatalogFor(worker)...)
	ws := t.TempDir()
	released := runnertest.NewGate()
	cancelled := runnertest.NewGate()
	script.Queue(worker, runnertest.Reply{Answer: "released", Gate: released}, runnertest.Reply{Answer: "never", Gate: cancelled})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clients := runners(ctx, script)
	defer clients.Close()
	session, err := clients.Start(worker, ws, nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		answer string
		err    error
	}
	turn := func() <-chan result {
		done := make(chan result, 1)
		go func() {
			answer, err := clients.Turn(session, worker, ws, "turn", nil)
			done <- result{answer, err}
		}()
		return done
	}
	done := turn()
	<-released.Entered()
	select {
	case r := <-done:
		t.Fatalf("a gated turn finished before release: %+v", r)
	case <-time.After(20 * time.Millisecond):
	}
	released.Release()
	if r := <-done; r.err != nil || r.answer != "released" {
		t.Fatalf("released turn = %+v", r)
	}
	done = turn()
	<-cancelled.Entered()
	cancel()
	if r := <-done; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled turn = %+v", r)
	}
}
