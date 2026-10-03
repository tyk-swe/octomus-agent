package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func newScriptedPlanningFixture(t *testing.T) *scriptedFixture {
	t.Helper()
	return newScriptedFixture(t, withGitHubIdentity())
}

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

type scriptedPlan struct {
	grounding     runnertest.Reply
	discovery     []runnertest.Reply
	reviews       []runnertest.Reply
	consolidation runnertest.Reply
}

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

func (p scriptedPlan) queue(f *scriptedFixture) {
	f.script.Queue(f.routes.Orchestrator, p.grounding, p.consolidation)
	f.script.Queue(f.routes.Discovery, p.discovery...)
	f.script.Queue(f.routes.ProposalReviewer, p.reviews...)
}

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

func (f *scriptedFixture) planningTurns() []runnertest.Call {
	turns := []runnertest.Call{}
	for _, route := range f.planningRoutes() {
		turns = append(turns, f.script.Turns(route)...)
	}
	return turns
}

func waitOnlyCycle(t *testing.T, state *store.Store) model.Cycle {
	t.Helper()
	var cycles []model.Cycle
	if !testutil.WaitUntil(30*time.Second, func() bool {
		var err error
		if cycles, err = store.List[model.Cycle](state, "cycle"); err != nil {
			t.Fatal(err)
		}
		return len(cycles) == 1 && cycles[0].Status != model.CycleRunning
	}) {
		t.Fatal("planning cycle did not finish")
	}
	return cycles[0]
}

func assertScriptedPlanningPass(t *testing.T, f *scriptedFixture, cycle model.Cycle) {
	t.Helper()
	want := int(f.cfg.DiscoveryAgents + 4)
	if cycle.Status != model.CycleCompleted || len(cycle.Sessions) != want || len(cycle.Assessments) != 2 {
		t.Fatalf("incomplete planning pass: status=%s sessions=%d/%d assessments=%d error=%s", cycle.Status, len(cycle.Sessions), want, len(cycle.Assessments), optionalText(cycle.Error))
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

func assertPlanningRulePrompts(t *testing.T, f *scriptedFixture, mode model.CycleMode) {
	t.Helper()
	categories := fmt.Sprint(f.cfg.Categories)
	execution := mode == model.CycleModeExecution
	discoveries := f.script.Turns(f.routes.Discovery)
	if len(discoveries) != int(f.cfg.DiscoveryAgents) {
		t.Fatalf("checked %d discovery prompts; want %d", len(discoveries), f.cfg.DiscoveryAgents)
	}
	for _, turn := range discoveries {
		for _, rule := range []string{"set each proposal's category to exactly one of " + categories + ".", fmt.Sprintf("Return at most %d proposals.", 100/int(f.cfg.DiscoveryAgents))} {
			if !strings.Contains(turn.Prompt, rule) {
				t.Fatalf("discovery prompt omits %q: %.300s", rule, turn.Prompt)
			}
		}
		if strings.Contains(turn.Prompt, "Always return reconsiders=[]") != execution || strings.Contains(turn.Prompt, "reconsiders=[] unless handling a supplied rediscovery request") == execution {
			t.Fatalf("%s discovery prompt states the wrong reconsiders rule: %.600s", mode, turn.Prompt)
		}
	}
	consolidations := 0
	for _, turn := range f.script.Turns(f.routes.Orchestrator) {
		if !strings.HasPrefix(turn.Prompt, "Act as final orchestrator") {
			continue
		}
		consolidations++
		if rule := "Every accepted proposal's category must be one of " + categories + "."; !strings.Contains(turn.Prompt, rule) {
			t.Fatalf("consolidation prompt omits %q", rule)
		}
		if strings.Contains(turn.Prompt, "Each rediscovery request ID must appear in reconsiders of exactly one returned proposal") != execution || strings.Contains(turn.Prompt, "only when it keeps that request's target") == execution {
			t.Fatalf("%s consolidation prompt states the wrong rediscovery rule", mode)
		}
	}
	if consolidations != 1 {
		t.Fatalf("checked %d consolidation prompts; want 1", consolidations)
	}
}

func TestAuditRunsCompleteIndependentPlanWithoutQueueingWork(t *testing.T) {
	t.Parallel()
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
	assertPlanningRulePrompts(t, fixture, model.CycleModeAudit)
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

func TestRunOnceCommitsCompletePlanningQueueAndPhase(t *testing.T) {
	t.Parallel()
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
	assertPlanningRulePrompts(t, fixture, model.CycleModeExecution)
}

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

func TestFailedPlanningCommitsNoPartialQueueOrDecisionMemory(t *testing.T) {
	t.Parallel()
	for _, mode := range []model.OperatingMode{model.OperatingModeRunOnce, model.OperatingModeContinuous} {
		t.Run(mode.String(), func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			plan := completePlan(t, fixture)
			plan.discovery[0] = runnertest.Reply{Answer: "this discovery answer is not JSON"}
			plan.queue(fixture)
			seeded := model.DefaultControl()
			seeded.IdleStreak = 2
			if err := fixture.state.SaveControl(seeded); err != nil {
				t.Fatal(err)
			}
			app := fixture.pausedApp(t)
			start := app.RunOnce
			if mode == model.OperatingModeContinuous {
				start = app.Resume
			}
			if err := start(); err != nil {
				t.Fatal(err)
			}
			before := time.Now().Unix()
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			failed := waitOnlyCycle(t, fixture.state)
			app.wg.Wait()
			after := time.Now().Unix()
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
			if mode == model.OperatingModeContinuous && (control.Mode != model.OperatingModeContinuous || control.Batch != nil || control.Error == nil || *control.Error != *failed.Error) {
				t.Fatalf("failed Continuous planning did not keep running with its error: %+v", control)
			}
			if control.IdleStreak != 2 {
				t.Fatalf("a failed pass changed the idle streak to %d", control.IdleStreak)
			}
			if delay := int64(2 * fixture.cfg.CycleIntervalSeconds); mode == model.OperatingModeContinuous && (control.NextCycleAt < before+delay || control.NextCycleAt > after+delay) {
				t.Fatalf("failed Continuous planning scheduled the next cycle at %d; want the streak's backoff %d after %d..%d", control.NextCycleAt, delay, before, after)
			}
			if events := planningErrors(t, fixture.state, failed.ID); len(events) != 1 || events[0].Message != *failed.Error {
				t.Fatalf("failed pass logged %+v; want one planning_error with the cycle error", events)
			}
			assertNoOpenClients(t, fixture.script)
		})
	}
}

func TestConsolidationMustAccountForEveryOriginalProposal(t *testing.T) {
	t.Parallel()
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
	app.wg.Wait()
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Error == nil || !strings.Contains(*control.Error, "omitted or invented") {
		t.Fatalf("failed audit did not retain durable control evidence: %+v, %v", control, err)
	}
	if events := planningErrors(t, fixture.state, cycleID); len(events) != 1 || events[0].Message != *cycle.Error {
		t.Fatalf("failed audit logged %+v; want one planning_error with the cycle error", events)
	}
}

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

func committedPlanningChange() planningMutation {
	return planningMutation{
		effect: func(cwd string) error {
			cmd := gitCommand(cwd, "-c", "user.name=Planner", "-c", "user.email=planner@example.com", "commit", "--allow-empty", "-m", "Planning mutation")
			if output, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("fixture commit: %w: %s", err, output)
			}
			return nil
		},
		preserved: func(t *testing.T, workspace string) {
			output, err := gitCommand(workspace, "log", "-1", "--format=%s").Output()
			if err != nil || strings.TrimSpace(string(output)) != "Planning mutation" {
				t.Fatalf("preserved workspace lost the committed mutation: %q, %v", output, err)
			}
		},
	}
}

func TestPlanningRejectsAndPreservesAMutatedRoleWorkspace(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		mutation   planningMutation
		mutate     func(*scriptedPlan, func(string) error)
		inStage    func(label string) bool
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
