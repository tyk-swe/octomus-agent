package engine

// The scripted fixture runs the engine against real local Git and a scripted
// runner adapter (package runnertest) instead of the Python Codex/OpenCode
// peers. Replies are keyed by route, and every role has its own model, so a
// route identifies the role that consumes a reply.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// scriptedRoutes are the fixture's per-role routes. Every tier uses Executor.
type scriptedRoutes struct {
	Executor, Reviewer, Repair                config.Route
	Orchestrator, Discovery, ProposalReviewer config.Route
}

func (r scriptedRoutes) all() []config.Route {
	return []config.Route{r.Executor, r.Reviewer, r.Repair, r.Orchestrator, r.Discovery, r.ProposalReviewer}
}

// scriptedFixture embeds the shared local fixture, so the planning and
// execution helpers (executionTask, saveExecutionTask, driveTask, waitCycle,
// sessionByRole, remoteHead, publications) accept fixture.planningFixture.
type scriptedFixture struct {
	*planningFixture
	script *runnertest.Script
	routes scriptedRoutes
}

type scriptedOption func(*scriptedSettings)

type scriptedSettings struct{ githubIdentity bool }

// withGitHubIdentity puts the git.py shim on PATH so origin reports as
// github.com/fixture/project. Remote validation (doctor, planning preflight,
// housekeeping, publication) needs it until the GitHub port (#8) lands;
// without it Git runs unshimmed against the local bare remote.
func withGitHubIdentity() scriptedOption {
	return func(s *scriptedSettings) { s.githubIdentity = true }
}

// newScriptedFixture builds a bare remote plus a pushed clone, the gh peer on
// PATH (the scheduler's PR refresh still shells out to gh) and saved settings
// whose routes all resolve against fixture.script's catalog. The configured
// runner binaries do not exist, so any accidental real connection fails. Git
// is real and unshimmed unless withGitHubIdentity is given.
func newScriptedFixture(t *testing.T, options ...scriptedOption) *scriptedFixture {
	t.Helper()
	settings := scriptedSettings{}
	for _, option := range options {
		option(&settings)
	}
	root := t.TempDir()
	if settings.githubIdentity {
		if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		pythonFixtureShim(t, filepath.Join(root, "bin", "git"), root, "git.py")
	}
	routes := scriptedRoutes{
		Executor:         config.NewRoute("scripted-executor", "medium"),
		Reviewer:         config.NewRoute("scripted-reviewer", "high"),
		Repair:           config.NewRoute("scripted-repair", "medium"),
		Orchestrator:     config.NewRoute("scripted-orchestrator", "medium"),
		Discovery:        config.NewRoute("scripted-discovery", "medium"),
		ProposalReviewer: config.NewRoute("scripted-proposal-reviewer", "medium"),
	}
	fixture := newFixture(t, root, func(cfg *config.Config) {
		cfg.CodexBinary = filepath.Join(root, "no-codex-installed")
		cfg.OpencodeBinary = filepath.Join(root, "no-opencode-installed")
		cfg.Roles["orchestrator"] = routes.Orchestrator
		cfg.Roles["discovery"] = routes.Discovery
		cfg.Roles["proposal_reviewer"] = routes.ProposalReviewer
		cfg.Roles["code_reviewer"] = routes.Reviewer
		for _, tier := range config.Tiers() {
			cfg.Tiers[tier] = routes.Executor
		}
		cfg.RepairRoute = routes.Repair
	})
	return &scriptedFixture{
		planningFixture: fixture,
		script:          runnertest.New(runnertest.CatalogFor(routes.all()...)...),
		routes:          routes,
	}
}

// configure saves an adjusted copy of the fixture settings. Tasks snapshot the
// configuration when built, so configure before executionTask.
func (f *scriptedFixture) configure(t *testing.T, adjust func(*config.Config)) {
	t.Helper()
	cfg := f.cfg.Clone()
	adjust(&cfg)
	if err := f.state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
}

// pausedApp builds an app connected to the fixture's script without changing
// the operating mode, with retention and observation housekeeping deferred,
// and shuts it down when the test ends. Audits and single planning runs need
// the service left paused.
func (f *scriptedFixture) pausedApp(t *testing.T, options ...Option) *App {
	t.Helper()
	app := New(f.state, f.dataDir, append([]Option{WithRunnerConnector(f.script.Connector())}, options...)...)
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	return app
}

// newApp is pausedApp resumed, so the scheduler picks up queued work.
func (f *scriptedFixture) newApp(t *testing.T, options ...Option) *App {
	t.Helper()
	app := f.pausedApp(t, options...)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	return app
}

func assertAdmissions(t *testing.T, state *store.Store, want uint64, label string) {
	t.Helper()
	if used, err := state.SessionsToday(); err != nil || used != want {
		t.Fatalf("admissions = %d, %v; want %d (%s)", used, err, want, label)
	}
}

func assertNoOpenClients(t *testing.T, script *runnertest.Script) {
	t.Helper()
	if open := script.OpenClients(); open != 0 {
		t.Fatalf("%d runner clients left open", open)
	}
}

// mustJSON marshals a scripted structured answer.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// cleanReview is a structured reviewer answer with no findings.
func cleanReview(summary string) string {
	return `{"completed": true, "summary": "` + summary + `", "findings": []}`
}

// writeFile is a reply effect that writes one workspace file.
func writeFile(name, content string) func(string) error {
	return func(cwd string) error {
		return os.WriteFile(filepath.Join(cwd, name), []byte(content), 0o644)
	}
}

// TestScriptedFixtureDrivesTaskThroughRepairToPublication runs a task end to
// end with no runner peer: the executor's edit fails verification after a
// clean review, the repair fixes it, a fresh reviewer approves and the task
// publishes.
func TestScriptedFixtureDrivesTaskThroughRepairToPublication(t *testing.T) {
	fixture := newScriptedFixture(t, withGitHubIdentity())
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"grep -q fixed feature.txt"}
	})
	routes, script := fixture.routes, fixture.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("First pass looks complete"), cleanReview("Repair verified"))
	script.Queue(routes.Repair, runnertest.Reply{Answer: "Wrote the fixed output", Effect: writeFile("feature.txt", "fixed output\n")})
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if saved.Status != model.StatusPublished || saved.PRNumber == nil || saved.OutputCommit == nil {
		t.Fatalf("scripted task did not publish: %+v", saved)
	}
	if len(saved.Reviews) != 2 || len(saved.Verification) != 2 || saved.Verification[0].Success || !saved.Verification[1].Success {
		t.Fatalf("review/verification evidence: reviews=%+v verification=%+v", saved.Reviews, saved.Verification)
	}
	for role, want := range map[string]int{"executor": 1, "reviewer": 2, "repair": 1} {
		sessions := sessionByRole(saved, role)
		if len(sessions) != want {
			t.Fatalf("%s sessions = %+v", role, sessions)
		}
		for _, session := range sessions {
			if session.Status != model.SessionCompleted {
				t.Fatalf("%s session not completed: %+v", role, session)
			}
		}
	}
	if summary := sessionByRole(saved, "executor")[0].Summary; summary != "Created feature.txt" {
		t.Fatalf("executor summary = %q", summary)
	}

	executorTurns := script.Turns(routes.Executor)
	if len(executorTurns) != 1 || executorTurns[0].Cwd != saved.Workspace || executorTurns[0].Schema != nil ||
		executorTurns[0].Session != *saved.ExecutionSession {
		t.Fatalf("executor turns = %+v", executorTurns)
	}
	reviewStarts := script.Starts(routes.Reviewer)
	if len(reviewStarts) != 2 || reviewStarts[0].Resume != nil || reviewStarts[1].Resume != nil || reviewStarts[0].Session == reviewStarts[1].Session {
		t.Fatalf("reviewers must be fresh sessions: %+v", reviewStarts)
	}
	for _, turn := range script.Turns(routes.Reviewer) {
		if turn.Schema == nil {
			t.Fatalf("reviewer turn without a structured schema: %+v", turn)
		}
	}
	if repairs := script.Turns(routes.Repair); len(repairs) != 1 || saved.RepairSession == nil || repairs[0].Session != *saved.RepairSession {
		t.Fatalf("repair turns = %+v (session %v)", repairs, saved.RepairSession)
	}
	for _, route := range routes.all() {
		if pending := script.Pending(route); pending != 0 {
			t.Fatalf("%d replies left on %s", pending, route)
		}
	}
	assertNoOpenClients(t, script)
	assertAdmissions(t, fixture.state, 4, "executor + 2 reviewers + repair")
	if head := remoteHead(t, fixture.planningFixture, saved.Branch); head != *saved.OutputCommit {
		t.Fatalf("published head %s, output %s", head, *saved.OutputCommit)
	}
}

// TestScriptedCatalogRejectsMissingRoute: route validation runs for real
// against the scripted catalog, so a route absent from it blocks the task as
// runner_unavailable before any admission, workspace or session.
func TestScriptedCatalogRejectsMissingRoute(t *testing.T) {
	fixture := newScriptedFixture(t)
	routes := fixture.routes
	fixture.script.SetCatalog(runnertest.CatalogFor(routes.Executor, routes.Repair, routes.Orchestrator, routes.Discovery, routes.ProposalReviewer)...)
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)

	saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonRunnerUnavailable {
		t.Fatalf("missing reviewer route outcome = %+v", saved)
	}
	if saved.Error == nil || !strings.Contains(*saved.Error, routes.Reviewer.String()) {
		t.Fatalf("the block must name the missing route: %v", saved.Error)
	}
	if saved.Workspace != "" || saved.ExecutionSession != nil || len(saved.Sessions) != 0 {
		t.Fatalf("a rejected route initialized the task: %+v", saved)
	}
	for _, call := range fixture.script.Calls() {
		if call.Kind == runnertest.CallStart || call.Kind == runnertest.CallTurn {
			t.Fatalf("a rejected route reached the runner: %+v", call)
		}
	}
	assertAdmissions(t, fixture.state, 0, "a rejected route admits nothing")
	assertNoOpenClients(t, fixture.script)
}
