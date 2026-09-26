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
		grounding:     runnertest.Reply{Answer: mustJSON(t, map[string]any{"context": "Small fixture with a feature contract in README.md."})},
		consolidation: runnertest.Reply{Answer: mustJSON(t, map[string]any{"proposals": []any{fixtureProposal("accepted", "Both independent reviews accept the concrete feature; no duplicates.")}})},
	}
	plan.discovery = append(plan.discovery, runnertest.Reply{Answer: mustJSON(t, map[string]any{"proposals": []any{fixtureProposal("candidate", "Delivers the documented feature.")}})})
	for i := uint64(1); i < f.cfg.DiscoveryAgents; i++ {
		plan.discovery = append(plan.discovery, runnertest.Reply{Answer: `{"proposals": []}`})
	}
	assessments := mustJSON(t, map[string]any{"assessments": []any{map[string]any{"id": "d0-feature", "decision": "accepted", "reason": "Concrete and useful."}}})
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
	assertNoOpenClients(t, f.script)
}

func TestAuditRunsCompleteIndependentPlanWithoutQueueingWork(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	completePlan(t, fixture).queue(fixture)
	existing := queuedTask(fixture.cfg, "already-queued", fixture.cfg.DefaultBranch, "octomus/already-queued")
	if err := fixture.state.Put("task", existing.ID, existing); err != nil {
		t.Fatal(err)
	}
	app := fixture.pausedApp(t)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	assertScriptedPlanningPass(t, fixture, cycle)
	if len(cycle.Proposals) != 1 || cycle.Proposals[0].ID != "d0-feature" || cycle.Proposals[0].Decision != model.DecisionAccepted {
		t.Fatalf("audit did not record the consolidated decision: %+v", cycle.Proposals)
	}
	// Roles that write proposals are told the bounds planning enforces.
	proposing := 0
	for _, turn := range fixture.planningTurns() {
		if strings.HasPrefix(turn.Prompt, "Discover worthwhile") || strings.HasPrefix(turn.Prompt, "Act as final orchestrator") {
			proposing++
			if !strings.Contains(turn.Prompt, "Hard limits: title at most 200 bytes; always set problem_key") {
				t.Fatalf("proposal prompt omits the metadata bounds: %.120s", turn.Prompt)
			}
		}
	}
	if proposing != int(fixture.cfg.DiscoveryAgents)+1 {
		t.Fatalf("checked %d proposal prompts; want every discovery and the consolidation", proposing)
	}
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Batch != nil {
		t.Fatalf("audit changed queue mode: %+v, %v", control, err)
	}
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 1 || tasks[0].ID != existing.ID || tasks[0].Status != model.StatusQueued || tasks[0].RunID != nil {
		t.Fatalf("audit disturbed or queued executable work: %+v, %v", tasks, err)
	}
	assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "audit")
}

// consolidationPrCapacity decodes the pr_capacity of the recorded context that
// closes the consolidation prompt.
func consolidationPrCapacity(t *testing.T, f *scriptedFixture) map[string]any {
	t.Helper()
	for _, turn := range f.script.Turns(f.routes.Orchestrator) {
		if !strings.HasPrefix(turn.Prompt, "Act as final orchestrator") {
			continue
		}
		const marker = "Context: "
		index := strings.LastIndex(turn.Prompt, marker)
		if index < 0 {
			t.Fatalf("consolidation prompt has no recorded context: %.200s", turn.Prompt)
		}
		var recorded struct {
			PrCapacity map[string]any `json:"pr_capacity"`
		}
		if err := json.Unmarshal([]byte(turn.Prompt[index+len(marker):]), &recorded); err != nil {
			t.Fatalf("consolidation context is not the recorded JSON: %v", err)
		}
		if recorded.PrCapacity == nil {
			t.Fatal("consolidation context has no pr_capacity")
		}
		return recorded.PrCapacity
	}
	t.Fatal("no consolidation turn ran")
	return nil
}

// An audit runs paused, so this process holds no PR dispatch authority. The
// planning roles still receive the capacity of the complete inventory the
// same pass grounded on, never "no inventory has been observed".
func TestAuditPlanningContextReportsTheGroundedPrCapacity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		limit     uint64
		owned     []maintenancePRFixture
		status    string
		remaining float64
		reason    any
	}{
		{name: "ready", limit: 5, status: "ready", remaining: 5, reason: nil},
		{name: "full", limit: 1, owned: []maintenancePRFixture{{name: "open", ageDays: 1, changedLines: 1}}, status: "full", remaining: 0, reason: prCapacityFullReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			fixture.configure(t, func(cfg *config.Config) { cfg.MaxOpenPRs = tc.limit })
			writeMaintenancePRFixture(t, fixture, tc.owned)
			completePlan(t, fixture).queue(fixture)
			app := fixture.pausedApp(t)
			cycleID, err := app.StartAudit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cycle := waitCycle(t, fixture.state, cycleID)
			if cycle.Status != model.CycleCompleted || cycle.Grounding == nil || cycle.Grounding.PRCoverage.ObservedAt == nil {
				t.Fatalf("audit did not complete with grounding: status=%s error=%v", cycle.Status, cycle.Error)
			}
			capacity := consolidationPrCapacity(t, fixture)
			if capacity["status"] != tc.status || capacity["remaining"] != tc.remaining || capacity["reason"] != tc.reason {
				t.Fatalf("planning context capacity = %v; want status %s, remaining %v, reason %v", capacity, tc.status, tc.remaining, tc.reason)
			}
			if capacity["limit"] != float64(tc.limit) || capacity["owned_open"] != float64(len(tc.owned)) || capacity["reserved"] != float64(0) {
				t.Fatalf("planning context capacity counts = %v", capacity)
			}
			if capacity["observed_at"] != *cycle.Grounding.PRCoverage.ObservedAt {
				t.Fatalf("capacity observed at %v; grounding observed at %s", capacity["observed_at"], *cycle.Grounding.PRCoverage.ObservedAt)
			}
			// The context is not dispatch authority: the paused service still has none.
			authority, err := app.PrCapacity()
			if err != nil || authority.Status != "unavailable" || authority.Remaining != nil {
				t.Fatalf("paused audit gained PR dispatch authority: %+v, %v", authority, err)
			}
		})
	}
}

// advanceMainDuringObservation replaces the fixture's git shim with one that,
// once, advances main on the bare remote just before grounding reads the
// remote default-branch head, and records the new commit in the returned path.
func advanceMainDuringObservation(t *testing.T, f *scriptedFixture) string {
	t.Helper()
	fixtures := filepath.Join(repositoryRoot(t), "tests", "fixtures")
	advanced := filepath.Join(f.root, "concurrent-main")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os, runpy, subprocess, sys
from pathlib import Path
root = Path(%[1]q)
os.environ['OCTOMUS_FIXTURE'] = str(root)
sys.path.insert(0, %[2]q)
args = sys.argv[1:]
marker = root / 'advance-main-on-ls-remote'
if marker.exists() and args[:2] == ['ls-remote', '--heads'] and args[-1] == 'refs/heads/main':
    marker.unlink()
    remote = str(root / 'remote.git')
    identity = dict(os.environ, GIT_AUTHOR_NAME='Maintainer', GIT_AUTHOR_EMAIL='maintainer@example.com',
                    GIT_COMMITTER_NAME='Maintainer', GIT_COMMITTER_EMAIL='maintainer@example.com')
    commit = subprocess.check_output(['/usr/bin/git', '--git-dir', remote, 'commit-tree', 'main^{tree}', '-p', 'main', '-m', 'Concurrent main'], text=True, env=identity).strip()
    subprocess.check_call(['/usr/bin/git', '--git-dir', remote, 'update-ref', 'refs/heads/main', commit])
    Path(%[3]q).write_text(commit)
runpy.run_path(%[4]q, run_name='__main__')
`, f.root, fixtures, advanced, filepath.Join(fixtures, "git.py"))
	if err := os.WriteFile(filepath.Join(f.root, "bin", "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "advance-main-on-ls-remote"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return advanced
}

// Main can advance after the checkout's last fetch and before grounding reads
// the remote head. Grounding records that head, so it must be local before any
// role clones the checkout at it.
func TestGroundingFetchesTheRemoteHeadsItObserved(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	advanced := advanceMainDuringObservation(t, fixture)
	completePlan(t, fixture).queue(fixture)
	app := fixture.pausedApp(t)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	assertScriptedPlanningPass(t, fixture, cycle)
	commit, err := os.ReadFile(advanced)
	if err != nil {
		t.Fatalf("main did not advance during grounding: %v", err)
	}
	if cycle.Grounding == nil || cycle.Grounding.Revision != strings.TrimSpace(string(commit)) {
		t.Fatalf("grounding revision %+v; want the concurrently pushed %s", cycle.Grounding, commit)
	}
}

func TestRunOnceCommitsCompletePlanningQueueAndPhase(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	completePlan(t, fixture).queue(fixture)
	app := fixture.pausedApp(t)
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

// The plan commit rewrites control (batch phase, idle streak), so it waits for
// the scheduler gate that operator controls hold across their control
// read-modify-write: nothing of the plan becomes durable while it is held.
func TestPlanCommitSerializesWithTheSchedulerGate(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	plan := completePlan(t, fixture)
	consolidation := runnertest.NewGate()
	plan.consolidation.Gate = consolidation
	plan.queue(fixture)
	app := fixture.pausedApp(t)
	if err := app.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if err := app.Tick(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-consolidation.Entered():
	case <-time.After(30 * time.Second):
		t.Fatal("planning did not reach consolidation")
	}
	app.gate.Lock()
	held := true
	defer func() {
		if held {
			app.gate.Unlock()
		}
	}()
	consolidation.Release()
	// Wait until the finished consolidation is recorded, then give the rest of
	// the pass (decision fingerprints, task snapshots) time to reach its commit.
	deadline := time.Now().Add(30 * time.Second)
	for {
		cycles, err := store.List[model.Cycle](fixture.state, "cycle")
		if err != nil || len(cycles) != 1 {
			t.Fatalf("cycles: %d, %v", len(cycles), err)
		}
		done := false
		for _, session := range cycles[0].Sessions {
			done = done || session.Role == "consolidation" && session.Status == model.SessionCompleted
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("consolidation did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for settle := time.Now().Add(time.Second); time.Now().Before(settle); time.Sleep(20 * time.Millisecond) {
		tasks, err := store.List[model.Task](fixture.state, "task")
		if err != nil || len(tasks) != 0 {
			t.Fatalf("plan committed %d tasks while the gate was held: %v", len(tasks), err)
		}
		cycle, err := store.List[model.Cycle](fixture.state, "cycle")
		if err != nil || len(cycle) != 1 || cycle[0].Status != model.CycleRunning {
			t.Fatalf("plan finished its cycle while the gate was held: %+v, %v", cycle, err)
		}
	}
	held = false
	app.gate.Unlock()
	cycle := waitOnlyCycle(t, fixture.state)
	assertScriptedPlanningPass(t, fixture, cycle)
	tasks, err := store.List[model.Task](fixture.state, "task")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("released plan did not commit its task: %d, %v", len(tasks), err)
	}
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModeRunOnce || control.Batch == nil || control.Batch.Phase != model.BatchPhaseExecuting {
		t.Fatalf("plan commit did not advance the batch phase: %+v, %v", control, err)
	}
}

func TestResumeClearsOldDelayAndNextTickStartsPlanning(t *testing.T) {
	for _, action := range []string{"direct", "control action"} {
		t.Run(action, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			completePlan(t, fixture).queue(fixture)
			app := fixture.pausedApp(t)
			control := model.DefaultControl()
			control.NextCycleAt = time.Now().Add(time.Hour).Unix()
			message := "earlier planning failure"
			control.Error = &message
			if err := fixture.state.SaveControl(control); err != nil {
				t.Fatal(err)
			}
			if action == "direct" {
				if err := app.Resume(); err != nil {
					t.Fatal(err)
				}
			} else if _, err := app.ControlAction("resume"); err != nil {
				t.Fatal(err)
			}
			resumed, err := app.Control()
			if err != nil || resumed.Mode != model.OperatingModeContinuous || resumed.NextCycleAt != 0 || resumed.Error != nil {
				t.Fatalf("resumed control: %+v, %v", resumed, err)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			cycle := waitOnlyCycle(t, fixture.state)
			assertScriptedPlanningPass(t, fixture, cycle)
		})
	}
}

// planningErrors returns the planning_error events recorded for entity.
func planningErrors(t *testing.T, state *store.Store, entity string) []model.Event {
	t.Helper()
	events, err := state.Events(&entity)
	if err != nil {
		t.Fatal(err)
	}
	found := []model.Event{}
	for _, event := range events {
		if event.Kind == "planning_error" {
			found = append(found, event)
		}
	}
	return found
}

// A failed pass commits nothing of its plan and logs one planning_error for
// its cycle in either execution mode: Run once pauses, Continuous keeps
// running and reports the failure in control.
func TestFailedPlanningCommitsNoPartialQueueOrDecisionMemory(t *testing.T) {
	for _, mode := range []model.OperatingMode{model.OperatingModeRunOnce, model.OperatingModeContinuous} {
		t.Run(mode.String(), func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			plan := completePlan(t, fixture)
			plan.discovery[0] = runnertest.Reply{Answer: "this discovery answer is not JSON"}
			plan.queue(fixture)
			app := fixture.pausedApp(t)
			start := app.RunOnce
			if mode == model.OperatingModeContinuous {
				start = app.Resume
			}
			if err := start(); err != nil {
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
			if mode == model.OperatingModeRunOnce && (control.Mode != model.OperatingModePaused || control.Batch != nil || control.Error == nil) {
				t.Fatalf("failed RunOnce planning was not paused: %+v", control)
			}
			if mode == model.OperatingModeContinuous && (control.Mode != model.OperatingModeContinuous || control.Error == nil || *control.Error != *failed.Error) {
				t.Fatalf("failed Continuous planning did not keep running with its error: %+v", control)
			}
			if events := planningErrors(t, fixture.state, failed.ID); len(events) != 1 || events[0].Message != *failed.Error {
				t.Fatalf("failed pass logged %+v; want one planning_error with the cycle error", events)
			}
			assertNoOpenClients(t, fixture.script)
		})
	}
}

func TestConsolidationMustAccountForEveryOriginalProposal(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	plan := completePlan(t, fixture)
	plan.consolidation = runnertest.Reply{Answer: `{"proposals": []}`}
	plan.queue(fixture)
	app := fixture.pausedApp(t)
	cycleID, err := app.StartAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, fixture.state, cycleID)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "omitted or invented") || !strings.Contains(*cycle.Error, `omitted "d0-feature"`) {
		t.Fatalf("incomplete consolidation was accepted or its error does not name the omitted proposal: %+v", cycle)
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
	if events := planningErrors(t, fixture.state, cycleID); len(events) != 1 || events[0].Message != *cycle.Error {
		t.Fatalf("failed audit logged %+v; want one planning_error with the cycle error", events)
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
			app := fixture.pausedApp(t)
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
			assertAdmissions(t, fixture.state, test.admissions(fixture.cfg.DiscoveryAgents), "planning turns before the failure")
			tasks, err := store.List[model.Task](fixture.state, "task")
			if err != nil || len(tasks) != 0 {
				t.Fatalf("mutated planning workspace leaked executable work: %+v, %v", tasks, err)
			}
			memory, err := fixture.state.DecisionMemory(fixture.cfg.GitHubRepo)
			if err != nil || len(memory) != 0 {
				t.Fatalf("mutated planning workspace leaked decision memory: %+v, %v", memory, err)
			}
			assertNoOpenClients(t, fixture.script)
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
	app := fixture.pausedApp(t)
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
			app := fixture.pausedApp(t)
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

// groundingCycle saves a running cycle for a direct grounding capture.
func groundingCycle(t *testing.T, f *scriptedFixture, mode model.CycleMode) model.Cycle {
	t.Helper()
	cycle := model.Cycle{
		Mode: mode, ID: model.ID(), Number: 1, Status: model.CycleRunning, StartedAt: model.Now(),
		Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: f.cfg.GitHubRepo,
	}
	if err := f.state.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	return cycle
}

// An audit grounds while the service is paused. Its persisted inventory
// supersedes an earlier refresh failure, but a paused service still gains no
// dispatch authority from it.
func TestPausedGroundingClearsEarlierRefreshFailure(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	cycle := groundingCycle(t, fixture, model.CycleModeAudit)
	app.runtimeMu.Lock()
	app.runtime.prRefreshError = "earlier fixture failure"
	app.runtimeMu.Unlock()
	if _, err := app.captureGrounding(context.Background(), fixture.cfg, &cycle); err != nil {
		t.Fatal(err)
	}
	if cycle.Grounding == nil {
		t.Fatal("grounding was not recorded")
	}
	app.runtimeMu.Lock()
	refreshError, observation := app.runtime.prRefreshError, app.runtime.prObservation
	app.runtimeMu.Unlock()
	if refreshError != "" {
		t.Fatalf("paused grounding kept the earlier refresh failure: %q", refreshError)
	}
	if observation != nil {
		t.Fatal("paused grounding gained dispatch authority")
	}
}

// A concurrent refresh whose fetch started after grounding's can persist a
// newer inventory first. Grounding's older inventory is then superseded, not a
// failure: planning continues, and neither the newer saved inventory nor the
// observation that refresh authorized is replaced by the older one.
func TestGroundingSupersededByANewerInventoryContinues(t *testing.T) {
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	newer := model.OpenPrInventory{Repository: fixture.cfg.GitHubRepo, ObservedAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339), PRs: []model.PullRequest{}}
	if persisted, err := fixture.state.PersistPrInventory(newer, nil); err != nil || !persisted {
		t.Fatalf("persist newer inventory: %t, %v", persisted, err)
	}
	authority := &freshPrObservation{identity: store.PrIdentityOf(fixture.cfg), inventory: newer.Clone(), fetchedAt: time.Now()}
	app.runtimeMu.Lock()
	app.runtime.prObservation = authority
	app.runtimeMu.Unlock()
	cycle := groundingCycle(t, fixture, model.CycleModeExecution)
	observed, err := app.captureGrounding(context.Background(), fixture.cfg, &cycle)
	if err != nil {
		t.Fatalf("superseded grounding failed planning: %v", err)
	}
	if cycle.Grounding == nil || cycle.Grounding.Revision == "" {
		t.Fatalf("grounding was not recorded: %+v", cycle.Grounding)
	}
	// Planning context stays consistent with the grounding it was built from.
	if cycle.Grounding.PRCoverage.ObservedAt == nil || observed.ObservedAt != *cycle.Grounding.PRCoverage.ObservedAt || observed.ObservedAt == newer.ObservedAt {
		t.Fatalf("grounding returned inventory observed at %s; grounding coverage %v", observed.ObservedAt, cycle.Grounding.PRCoverage.ObservedAt)
	}
	saved, err := store.Get[model.Cycle](fixture.state, "cycle", cycle.ID)
	if err != nil || saved == nil || saved.Grounding == nil || saved.Grounding.Revision != cycle.Grounding.Revision {
		t.Fatalf("grounding was not saved with the cycle: %+v, %v", saved, err)
	}
	stored, err := fixture.state.OpenPrInventory()
	if err != nil || stored == nil || stored.ObservedAt != newer.ObservedAt {
		t.Fatalf("older grounding inventory replaced the newer one: %+v, %v", stored, err)
	}
	app.runtimeMu.Lock()
	observation := app.runtime.prObservation
	app.runtimeMu.Unlock()
	if observation != authority {
		t.Fatal("superseded grounding replaced the newer refresh's observation")
	}
}
