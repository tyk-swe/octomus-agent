package engine

// Planning role tests (grounding, discovery, proposal review, consolidation)
// against real local Git and the scripted runner adapter. No runner peer is
// spawned: every planning turn consumes a scripted reply on its role's route.
// The gh peer and the git.py identity shim stay until the GitHub port (#8).
// Protocol-dependent planning checks (for example Codex authentication in
// preflight) stay on the Python peers.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// newScriptedPlanningFixture is a scripted fixture whose origin reports the
// GitHub identity that planning preflight and grounding validate.
func newScriptedPlanningFixture(t *testing.T) *scriptedFixture {
	t.Helper()
	return newScriptedFixture(t, withGitHubIdentity())
}

// planningApp builds an app connected to the fixture's script without
// changing the operating mode, with retention and observation housekeeping
// deferred, and shuts it down when the test ends.
func (f *scriptedFixture) planningApp(t *testing.T, options ...Option) *App {
	t.Helper()
	app := New(f.state, f.dataDir, append([]Option{WithRunnerConnector(f.script.Connector())}, options...)...)
	t.Cleanup(app.Shutdown)
	app.runtime.lastRetention = time.Now()
	app.runtime.lastObserve = time.Now()
	return app
}

// fixtureProposal is the planning fixture's one concrete proposal: complete
// feature.txt on the default branch.
func fixtureProposal(decision, reason string) map[string]any {
	return map[string]any{
		"id": "d0-feature", "title": "Complete the fixture feature",
		"problem":  "The fixture has no complete feature output.",
		"evidence": []string{"README.md: the feature contract requires fixed output"},
		"benefit":  "Delivers the documented feature.", "category": "features",
		"target": "main", "tier": "M", "scope": "Implement feature.txt only.",
		"problem_key": "", "relevant_paths": []string{}, "reconsiders": []string{},
		"dependencies": []string{}, "prompt": "Create feature.txt with fixed output and verify its contents.",
		"decision": decision, "reason": reason,
	}
}

func planningJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// scriptedPlan holds the replies of one planning pass, by stage.
type scriptedPlan struct {
	grounding     runnertest.Reply
	discovery     []runnertest.Reply
	reviews       []runnertest.Reply
	consolidation runnertest.Reply
}

// completePlan scripts a pass that accepts the fixture proposal: one discovery
// agent proposes it, the rest find nothing, both adversaries accept it and the
// orchestrator accepts it.
func completePlan(t *testing.T, f *scriptedFixture) scriptedPlan {
	t.Helper()
	plan := scriptedPlan{
		grounding:     runnertest.Reply{Answer: planningJSON(t, map[string]any{"context": "Small fixture with a feature contract in README.md."})},
		consolidation: runnertest.Reply{Answer: planningJSON(t, map[string]any{"proposals": []any{fixtureProposal("accepted", "Both independent reviews accept the concrete feature; no duplicates.")}})},
	}
	plan.discovery = append(plan.discovery, runnertest.Reply{Answer: planningJSON(t, map[string]any{"proposals": []any{fixtureProposal("candidate", "Delivers the documented feature.")}})})
	for i := uint64(1); i < f.cfg.DiscoveryAgents; i++ {
		plan.discovery = append(plan.discovery, runnertest.Reply{Answer: `{"proposals": []}`})
	}
	assessments := planningJSON(t, map[string]any{"assessments": []any{map[string]any{"id": "d0-feature", "decision": "accepted", "reason": "Concrete and useful."}}})
	for range model.ReviewerSlots() {
		plan.reviews = append(plan.reviews, runnertest.Reply{Answer: assessments})
	}
	return plan
}

// queue scripts the plan. Grounding and consolidation share the orchestrator
// route and run in that order. Discovery agents and adversaries run in
// parallel, so which label consumes which reply on their routes is not fixed.
func (p scriptedPlan) queue(f *scriptedFixture) {
	f.script.Queue(f.routes.Orchestrator, p.grounding, p.consolidation)
	f.script.Queue(f.routes.Discovery, p.discovery...)
	f.script.Queue(f.routes.ProposalReviewer, p.reviews...)
}

// planningRoute is the configured route of a planning session label.
func (f *scriptedFixture) planningRoute(label string) config.Route {
	switch {
	case label == "grounding" || label == "consolidation":
		return f.routes.Orchestrator
	case strings.HasPrefix(label, "discovery-"):
		return f.routes.Discovery
	default:
		return f.routes.ProposalReviewer
	}
}

func (f *scriptedFixture) planningRoutes() []config.Route {
	return []config.Route{f.routes.Orchestrator, f.routes.Discovery, f.routes.ProposalReviewer}
}

func roleWorkspace(f *scriptedFixture, cycleID, label string) string {
	return filepath.Join(f.dataDir, "cycles", cycleID, label, "workspace")
}

// planningTurns returns every scripted planning turn, in no particular order.
func (f *scriptedFixture) planningTurns() []runnertest.Call {
	turns := []runnertest.Call{}
	for _, route := range f.planningRoutes() {
		turns = append(turns, f.script.Turns(route)...)
	}
	return turns
}

// waitOnlyCycle waits for the single recorded cycle to leave running.
func waitOnlyCycle(t *testing.T, state *store.Store) model.Cycle {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cycles, err := store.List[model.Cycle](state, "cycle")
		if err != nil {
			t.Fatal(err)
		}
		if len(cycles) == 1 && cycles[0].Status != model.CycleRunning {
			return cycles[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("planning cycle did not finish")
	return model.Cycle{}
}

// assertScriptedPlanningPass checks a completed pass: one fresh session per
// planning role on its configured route, one structured turn per role in its
// own role clone, every successful clone removed and every client closed.
func assertScriptedPlanningPass(t *testing.T, f *scriptedFixture, cycle model.Cycle) {
	t.Helper()
	want := int(f.cfg.DiscoveryAgents + 4)
	if cycle.Status != model.CycleCompleted || len(cycle.Sessions) != want || len(cycle.Assessments) != 2 {
		t.Fatalf("incomplete planning pass: status=%s sessions=%d/%d assessments=%d error=%v", cycle.Status, len(cycle.Sessions), want, len(cycle.Assessments), cycle.Error)
	}
	sessions := map[string]model.Session{}
	roles := map[string]int{}
	for _, session := range cycle.Sessions {
		if session.Status != model.SessionCompleted {
			t.Fatalf("nonterminal planning session: %+v", session)
		}
		if _, duplicate := sessions[session.ID]; duplicate {
			t.Fatalf("planning session %s was reused", session.ID)
		}
		sessions[session.ID] = session
		roles[session.Role]++
		if route := f.planningRoute(session.Role); session.Route.String() != route.String() {
			t.Fatalf("%s ran on %s; want %s", session.Role, session.Route, route)
		}
		if _, err := os.Stat(roleWorkspace(f, cycle.ID, session.Role)); !os.IsNotExist(err) {
			t.Fatalf("successful immutable %s clone remains: %v", session.Role, err)
		}
	}
	wantRoles := append([]string{"grounding", "consolidation"}, model.ReviewerSlots()...)
	for i := uint64(0); i < f.cfg.DiscoveryAgents; i++ {
		wantRoles = append(wantRoles, fmt.Sprintf("discovery-%d", i))
	}
	for _, role := range wantRoles {
		if roles[role] != 1 {
			t.Fatalf("missing independent planning role %s: %+v", role, roles)
		}
	}

	turns := f.planningTurns()
	if len(turns) != want {
		t.Fatalf("planning pass used %d runner turns; want one per role (%d)", len(turns), want)
	}
	workspaces := map[string]struct{}{}
	for _, turn := range turns {
		session, ok := sessions[turn.Session]
		if !ok {
			t.Fatalf("runner turn used unknown planning session %q", turn.Session)
		}
		if turn.Cwd != roleWorkspace(f, cycle.ID, session.Role) {
			t.Fatalf("%s turn ran in %s", session.Role, turn.Cwd)
		}
		if _, duplicate := workspaces[turn.Cwd]; duplicate {
			t.Fatalf("planning roles shared workspace %s", turn.Cwd)
		}
		workspaces[turn.Cwd] = struct{}{}
		if turn.Schema == nil {
			t.Fatalf("%s turn had no structured output schema", session.Role)
		}
	}
	for _, route := range f.planningRoutes() {
		for _, start := range f.script.Starts(route) {
			if start.Resume != nil {
				t.Fatalf("planning session resumed %s; roles must start fresh", *start.Resume)
			}
		}
		if pending := f.script.Pending(route); pending != 0 {
			t.Fatalf("%d replies left on %s", pending, route)
		}
	}
	if open := f.script.OpenClients(); open != 0 {
		t.Fatalf("%d runner clients left open", open)
	}
}

func TestAuditRunsCompleteIndependentPlanWithoutQueueingWork(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	completePlan(t, fixture).queue(fixture)
	existing := queuedTask(fixture.cfg, "already-queued", fixture.cfg.DefaultBranch, "octomus/already-queued")
	if err := fixture.state.Put("task", existing.ID, existing); err != nil {
		t.Fatal(err)
	}
	app := fixture.planningApp(t)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	assertScriptedPlanningPass(t, fixture, cycle)
	if len(cycle.Proposals) != 1 || cycle.Proposals[0].ID != "d0-feature" || cycle.Proposals[0].Decision != model.DecisionAccepted {
		t.Fatalf("audit did not record the consolidated decision: %+v", cycle.Proposals)
	}
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Batch != nil {
		t.Fatalf("audit changed queue mode: %+v, %v", control, err)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 1 || tasks[0].ID != existing.ID || tasks[0].Status != model.StatusQueued || tasks[0].RunID != nil {
		t.Fatalf("audit disturbed or queued executable work: %+v, %v", tasks, err)
	}
	used, err := fixture.state.SessionsToday()
	if err != nil || used != fixture.cfg.PlanningAdmissionsRequired() {
		t.Fatalf("audit admissions = %d, %v; want %d", used, err, fixture.cfg.PlanningAdmissionsRequired())
	}
}

func TestRunOnceCommitsCompletePlanningQueueAndPhase(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	completePlan(t, fixture).queue(fixture)
	app := fixture.planningApp(t)
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	cycle := waitOnlyCycle(t, fixture.state)
	assertScriptedPlanningPass(t, fixture, cycle)
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("complete plan did not commit one task: %d, %v", len(tasks), err)
	}
	control, _ := app.Control()
	if control.Mode != model.OperatingModeRunOnce || control.Batch == nil || control.Batch.Phase != model.BatchPhaseExecuting || tasks[0].RunID == nil || *tasks[0].RunID != control.Batch.ID {
		t.Fatalf("queue and RunOnce phase were not committed together: control=%+v task=%+v", control, tasks[0])
	}
	if tasks[0].SourceRevision == "" || tasks[0].DefaultRevision == "" || tasks[0].Branch == "" || tasks[0].AttemptPolicy == nil {
		t.Fatalf("planned task did not snapshot execution inputs: %+v", tasks[0])
	}
	if tasks[0].Route.String() != fixture.routes.Executor.String() || tasks[0].Proposal.ID != "d0-feature" {
		t.Fatalf("planned task did not take the accepted proposal and its tier route: %+v", tasks[0])
	}
}

func TestFailedPlanningCommitsNoPartialQueueOrDecisionMemory(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	plan := completePlan(t, fixture)
	plan.discovery[0] = runnertest.Reply{Answer: "this discovery answer is not JSON"}
	plan.queue(fixture)
	app := fixture.planningApp(t)
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	failed := waitOnlyCycle(t, fixture.state)
	app.wg.Wait() // Cycle status is persisted before control finalization finishes.
	if failed.Status != model.CycleFailed || failed.Error == nil || !strings.Contains(*failed.Error, "invalid JSON") {
		t.Fatalf("malformed discovery was accepted: %+v", failed)
	}
	statuses := map[string]int{}
	for _, session := range failed.Sessions {
		statuses[session.Status]++
	}
	if len(failed.Sessions) != int(1+fixture.cfg.DiscoveryAgents) || statuses[model.SessionFailed] != 1 {
		t.Fatalf("failed plan did not retain terminal evidence for every started role: %+v", failed.Sessions)
	}
	if turns := fixture.script.Turns(fixture.routes.ProposalReviewer); len(turns) != 0 {
		t.Fatalf("proposal review ran after discovery failed: %+v", turns)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("partial plan leaked tasks: %+v, %v", tasks, err)
	}
	memory, err := fixture.state.DecisionMemory(fixture.cfg.GitHubRepo)
	if err != nil || len(memory) != 0 {
		t.Fatalf("partial plan leaked decision memory: %+v, %v", memory, err)
	}
	control, _ := app.Control()
	if control.Mode != model.OperatingModePaused || control.Batch != nil || control.Error == nil {
		t.Fatalf("failed RunOnce planning was not paused: %+v", control)
	}
	if open := fixture.script.OpenClients(); open != 0 {
		t.Fatalf("%d runner clients left open", open)
	}
}

func TestConsolidationMustAccountForEveryOriginalProposal(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	plan := completePlan(t, fixture)
	plan.consolidation = runnertest.Reply{Answer: `{"proposals": []}`}
	plan.queue(fixture)
	app := fixture.planningApp(t)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "omitted or invented") {
		t.Fatalf("incomplete consolidation was accepted: %+v", cycle)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("incomplete consolidation leaked executable work: %+v, %v", tasks, err)
	}
	app.wg.Wait() // Cycle status is persisted before control finalization finishes.
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Error == nil || !strings.Contains(*control.Error, "omitted or invented") {
		t.Fatalf("failed audit did not retain durable control evidence: %+v, %v", control, err)
	}
}

// planningMutation is a scripted worker edit to a read-only role clone, and a
// check that the preserved clone still shows it.
type planningMutation struct {
	effect    func(cwd string) error
	preserved func(t *testing.T, workspace string)
}

func untrackedPlanningFile() planningMutation {
	return planningMutation{
		effect: writeFile("planning-mutation.txt", "fixture mutation\n"),
		preserved: func(t *testing.T, workspace string) {
			if data, err := os.ReadFile(filepath.Join(workspace, "planning-mutation.txt")); err != nil || string(data) != "fixture mutation\n" {
				t.Fatalf("preserved workspace lost the untracked mutation: %q, %v", data, err)
			}
		},
	}
}

func editedPlanningFile() planningMutation {
	return planningMutation{
		effect: writeFile("README.md", "# Rewritten by a planning role\n"),
		preserved: func(t *testing.T, workspace string) {
			if data, err := os.ReadFile(filepath.Join(workspace, "README.md")); err != nil || string(data) != "# Rewritten by a planning role\n" {
				t.Fatalf("preserved workspace lost the tracked edit: %q, %v", data, err)
			}
		},
	}
}

// committedPlanningChange leaves a clean tree at a new commit, so only the
// revision check can catch it.
func committedPlanningChange() planningMutation {
	return planningMutation{
		effect: func(cwd string) error {
			cmd := exec.Command("/usr/bin/git", "-c", "user.name=Planner", "-c", "user.email=planner@example.com", "commit", "--allow-empty", "-m", "Planning mutation")
			cmd.Dir = cwd
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("fixture commit: %w: %s", err, output)
			}
			return nil
		},
		preserved: func(t *testing.T, workspace string) {
			cmd := exec.Command("/usr/bin/git", "log", "-1", "--format=%s")
			cmd.Dir = workspace
			output, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(output)) != "Planning mutation" {
				t.Fatalf("preserved workspace lost the committed mutation: %q, %v", output, err)
			}
		},
	}
}

// TestPlanningRejectsAndPreservesAMutatedRoleWorkspace: a planning turn that
// changes its role clone in any way (untracked file, tracked edit or new
// commit) fails that role's session and the cycle, commits no work, and keeps
// the mutated clone for inspection while successful clones are removed.
func TestPlanningRejectsAndPreservesAMutatedRoleWorkspace(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutation planningMutation
		// mutate attaches the effect to one reply of the stage under test.
		mutate func(*scriptedPlan, func(string) error)
		// inStage reports whether a session label belongs to that stage.
		inStage func(label string) bool
		// admissions is how many planning turns were admitted before the
		// cycle failed, as a function of the discovery agent count.
		admissions func(discovery uint64) uint64
	}{
		{
			name: "grounding writes an untracked file", mutation: untrackedPlanningFile(),
			mutate:     func(p *scriptedPlan, effect func(string) error) { p.grounding.Effect = effect },
			inStage:    func(label string) bool { return label == "grounding" },
			admissions: func(uint64) uint64 { return 1 },
		},
		{
			name: "discovery edits a tracked file", mutation: editedPlanningFile(),
			mutate:     func(p *scriptedPlan, effect func(string) error) { p.discovery[0].Effect = effect },
			inStage:    func(label string) bool { return strings.HasPrefix(label, "discovery-") },
			admissions: func(discovery uint64) uint64 { return 1 + discovery },
		},
		{
			name: "discovery commits", mutation: committedPlanningChange(),
			mutate:     func(p *scriptedPlan, effect func(string) error) { p.discovery[0].Effect = effect },
			inStage:    func(label string) bool { return strings.HasPrefix(label, "discovery-") },
			admissions: func(discovery uint64) uint64 { return 1 + discovery },
		},
		{
			name: "proposal review writes an untracked file", mutation: untrackedPlanningFile(),
			mutate:     func(p *scriptedPlan, effect func(string) error) { p.reviews[0].Effect = effect },
			inStage:    func(label string) bool { return slices.Contains(model.ReviewerSlots(), label) },
			admissions: func(discovery uint64) uint64 { return 3 + discovery },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			plan := completePlan(t, fixture)
			test.mutate(&plan, test.mutation.effect)
			plan.queue(fixture)
			app := fixture.planningApp(t)
			cycleID, err := app.StartAudit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cycle := waitCycle(t, fixture.state, cycleID)
			app.wg.Wait()
			if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "modified its source snapshot") {
				t.Fatalf("mutated planning workspace was accepted: %+v", cycle)
			}

			var failed *model.Session
			for i, session := range cycle.Sessions {
				switch session.Status {
				case model.SessionFailed:
					if failed != nil {
						t.Fatalf("more than one planning session failed: %+v", cycle.Sessions)
					}
					failed = &cycle.Sessions[i]
				case model.SessionCompleted:
					if _, err := os.Stat(roleWorkspace(fixture, cycle.ID, session.Role)); !os.IsNotExist(err) {
						t.Fatalf("successful %s clone remains: %v", session.Role, err)
					}
				default:
					t.Fatalf("nonterminal planning session: %+v", session)
				}
			}
			if failed == nil || !test.inStage(failed.Role) || !strings.Contains(failed.Summary, "modified its source snapshot") {
				t.Fatalf("mutated planning role lost terminal session evidence: %+v", cycle.Sessions)
			}
			if route := fixture.planningRoute(failed.Role); failed.Route.String() != route.String() {
				t.Fatalf("failed %s session recorded route %s; want %s", failed.Role, failed.Route, route)
			}
			workspace := roleWorkspace(fixture, cycle.ID, failed.Role)
			ranThere := false
			for _, turn := range fixture.planningTurns() {
				if turn.Session == failed.ID && turn.Cwd == workspace {
					ranThere = true
				}
			}
			if !ranThere {
				t.Fatalf("failed session %s has no scripted turn in %s", failed.ID, workspace)
			}
			test.mutation.preserved(t, workspace)

			if want := uint64(len(cycle.Sessions)); want != test.admissions(fixture.cfg.DiscoveryAgents) || uint64(len(fixture.planningTurns())) != want {
				t.Fatalf("planning ran %d sessions and %d turns; want %d before the failure", want, len(fixture.planningTurns()), test.admissions(fixture.cfg.DiscoveryAgents))
			}
			if used, err := fixture.state.SessionsToday(); err != nil || used != test.admissions(fixture.cfg.DiscoveryAgents) {
				t.Fatalf("admissions = %d, %v; want %d", used, err, test.admissions(fixture.cfg.DiscoveryAgents))
			}
			tasks, err := store.List[model.Task](fixture.state, "task")
			if err != nil || len(tasks) != 0 {
				t.Fatalf("mutated planning workspace leaked executable work: %+v, %v", tasks, err)
			}
			memory, err := fixture.state.DecisionMemory(fixture.cfg.GitHubRepo)
			if err != nil || len(memory) != 0 {
				t.Fatalf("mutated planning workspace leaked decision memory: %+v, %v", memory, err)
			}
			if open := fixture.script.OpenClients(); open != 0 {
				t.Fatalf("%d runner clients left open", open)
			}
		})
	}
}

func TestRemotePreflightDoesNotHoldControlLockAndRejectsChangedPolicy(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	hold := filepath.Join(fixture.root, "reconcile-hold")
	if err := os.WriteFile(hold, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(hold) })
	app := fixture.planningApp(t)
	type preflightResult struct {
		id  string
		err error
	}
	finished := make(chan preflightResult, 1)
	go func() {
		id, err := app.StartAudit(context.Background())
		finished <- preflightResult{id: id, err: err}
	}()
	waitForFixtureFile(t, filepath.Join(fixture.root, "reconcile-entered"), "remote preflight did not reach the deterministic barrier")
	paused := make(chan error, 1)
	go func() { paused <- app.Pause() }()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote preflight held the control lock")
	}
	changed := fixture.cfg.Clone()
	changed.GitHubRepo = "fixture/changed"
	if err := fixture.state.Put("settings", "config", changed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	outcome := <-finished
	if outcome.err == nil || outcome.id != "" {
		t.Fatalf("stale preflight committed after policy changed: %+v", outcome)
	}
	cycles, err := store.List[model.Cycle](fixture.state, "cycle")
	if err != nil || len(cycles) != 0 {
		t.Fatalf("stale preflight created a cycle: %d, %v", len(cycles), err)
	}
	if turns := fixture.planningTurns(); len(turns) != 0 {
		t.Fatalf("stale preflight ran planning turns: %+v", turns)
	}
}

func TestPlanningAllowanceConsumedDuringPreflightUsesModeSemantics(t *testing.T) {
	for _, test := range []struct {
		name    string
		runOnce bool
	}{
		{name: "run once pauses with capacity event", runOnce: true},
		{name: "continuous waits without an error", runOnce: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			fixture.configure(t, func(cfg *config.Config) { cfg.MaxSessionsPerDay = cfg.PlanningAdmissionsRequired() })
			cfg := fixture.cfg
			hold := filepath.Join(fixture.root, "reconcile-hold")
			if err := os.WriteFile(hold, []byte("1"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(hold) })
			app := fixture.planningApp(t)
			if test.runOnce {
				if err := app.RunOnce(); err != nil {
					t.Fatal(err)
				}
			} else if err := app.Resume(); err != nil {
				t.Fatal(err)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			waitForFixtureFile(t, filepath.Join(fixture.root, "reconcile-entered"), "planning preflight did not reach the deterministic barrier")
			if err := fixture.state.ReserveSession(0, store.NewAdmission("competing-cycle", nil, "discovery", cfg.Roles["discovery"])); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(hold); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()

			control, err := app.Control()
			if err != nil {
				t.Fatal(err)
			}
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 0 {
				t.Fatalf("capacity race created a cycle: %d, %v", len(cycles), err)
			}
			if turns := fixture.planningTurns(); len(turns) != 0 {
				t.Fatalf("capacity race ran planning turns: %+v", turns)
			}
			if test.runOnce {
				if control.Mode != model.OperatingModePaused || control.Batch != nil || control.Error == nil {
					t.Fatalf("RunOnce capacity race did not pause: %+v", control)
				}
				events, err := fixture.state.Events(nil)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, event := range events {
					if event.Kind == "planning_capacity" {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("RunOnce capacity race recorded %d planning-capacity events", count)
				}
			} else if control.Mode != model.OperatingModeContinuous || control.Error != nil || control.NextCycleAt <= time.Now().Unix() {
				t.Fatalf("Continuous capacity race did not wait for UTC reset: %+v", control)
			}
		})
	}
}

// waitForFixtureFile waits for a fixture peer's barrier file.
func waitForFixtureFile(t *testing.T, path, failure string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(failure)
}
