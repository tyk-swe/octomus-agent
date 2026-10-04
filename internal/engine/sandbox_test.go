package engine

// Where untrusted children run: no runner outlives its turn, and every session records its sandbox.

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// watchedBackend records how many runner clients were open whenever a verification command started.
type watchedBackend struct {
	sandbox.Host
	script *runnertest.Script
	mu     sync.Mutex
	open   []int
	dirs   []string
}

func (w *watchedBackend) Start(ctx context.Context, spec sandbox.Spec) (sandbox.Child, error) {
	w.mu.Lock()
	w.open = append(w.open, w.script.OpenClients())
	w.dirs = append(w.dirs, spec.Dir)
	w.mu.Unlock()
	return w.Host.Start(ctx, spec)
}

func TestNoRunnerOutlivesItsTurn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
	routes, script := f.routes, f.script
	var connectMu sync.Mutex
	openAtConnect := []int{}
	connect := script.Connector()
	watched := func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
		connectMu.Lock()
		openAtConnect = append(openAtConnect, script.OpenClients())
		connectMu.Unlock()
		return connect(ctx, backend, cfg, cwd)
	}
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("First pass looks complete"), cleanReview("Repair verified"))
	script.Queue(routes.Repair, runnertest.Reply{Answer: "Wrote the fixed output", Effect: writeFile("feature.txt", "fixed output\n")})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)
	backend := &watchedBackend{script: script}

	saved := driveTask(t, f, f.newApp(t, WithRunnerConnector(watched), WithSandbox(backend)), task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task = %+v; want it published", saved)
	}
	if len(backend.open) != 2 {
		t.Fatalf("verification starts = %v; want one per review round", backend.open)
	}
	for i, open := range backend.open {
		if open != 0 {
			t.Fatalf("%d runner clients were open when verification %d started", open, i)
		}
		if want := filepath.Join(filepath.Dir(saved.Workspace), verificationDir, "workspace"); backend.dirs[i] != want {
			t.Fatalf("verification %d ran in %s; want the pristine checkout %s", i, backend.dirs[i], want)
		}
	}
	for i, open := range openAtConnect {
		if open != 0 {
			t.Fatalf("connect %d found %d runner clients still open; every turn must release its runner", i, open)
		}
	}
	if connects := len(openAtConnect); connects < 5 {
		t.Fatalf("connects = %d; want route validation, executor, two reviewers and repair each on a fresh runner", connects)
	}
	assertNoOpenClients(t, script)
}

func TestSessionsRecordSandboxes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
	routes, script := f.routes, f.script
	script.RecordSandbox(&model.SandboxRecord{ImageID: "sha256:sandbox", Runs: 1, Egress: model.SandboxEgress{
		Allowed: map[string]uint64{"api.openai.com:443": 2}, Denied: map[string]uint64{"example.com:443": 1}}})
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("First pass looks complete"), cleanReview("Repair verified"))
	script.Queue(routes.Repair, runnertest.Reply{Answer: "Wrote the fixed output", Effect: writeFile("feature.txt", "fixed output\n")})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if saved.Status != model.StatusPublished || len(saved.Sessions) != 4 {
		t.Fatalf("task = %+v", saved)
	}
	for _, session := range saved.Sessions {
		if session.Sandbox == nil || session.Sandbox.Runs != 1 || session.Sandbox.ImageID != "sha256:sandbox" ||
			session.Sandbox.Egress.Allowed["api.openai.com:443"] != 2 || session.Sandbox.Egress.Denied["example.com:443"] != 1 {
			t.Fatalf("%s session sandbox = %+v; want the record of its own turn", session.Role, session.Sandbox)
		}
	}
}
