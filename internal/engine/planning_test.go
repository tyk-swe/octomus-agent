package engine

// Planning passes: grounding, discovery, adversarial review and consolidation, committed as a whole or not at all;
// decision memory; the planning admission budget.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

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

func completePlan(t *testing.T, f *fixture) scriptedPlan {
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

func (p scriptedPlan) queue(f *fixture) {
	f.script.Queue(f.routes.Orchestrator, p.grounding, p.consolidation)
	f.script.Queue(f.routes.Discovery, p.discovery...)
	f.script.Queue(f.routes.ProposalReviewer, p.reviews...)
}

func (f *fixture) planningRoute(label string) config.Route {
	switch {
	case label == "grounding" || label == "consolidation":
		return f.routes.Orchestrator
	case strings.HasPrefix(label, "discovery-"):
		return f.routes.Discovery
	default:
		return f.routes.ProposalReviewer
	}
}

func (f *fixture) planningRoutes() []config.Route {
	return []config.Route{f.routes.Orchestrator, f.routes.Discovery, f.routes.ProposalReviewer}
}

func roleWorkspace(f *fixture, cycleID, label string) string {
	return filepath.Join(f.dataDir, "cycles", cycleID, label, "workspace")
}

func (f *fixture) planningTurns() []runnertest.Call {
	turns := []runnertest.Call{}
	for _, route := range f.planningRoutes() {
		turns = append(turns, f.script.Turns(route)...)
	}
	return turns
}

func assertPlanningPass(t *testing.T, f *fixture, cycle model.Cycle) {
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

func TestAuditPlansWithoutQueueing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	completePlan(t, f).queue(f)
	existing := queuedTask(f.cfg, "already-queued", f.cfg.DefaultBranch, "octomus/already-queued")
	putTask(t, f, existing)
	app := f.pausedApp(t)
	cycleID, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, f.state, cycleID)
	assertPlanningPass(t, f, cycle)
	if len(cycle.Proposals) != 1 || cycle.Proposals[0].ID != "d0-feature" || cycle.Proposals[0].Decision != model.DecisionAccepted {
		t.Fatalf("audit did not record the consolidated decision: %+v", cycle.Proposals)
	}
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Batch != nil {
		t.Fatalf("audit changed queue mode: %+v, %v", control, err)
	}
	tasks, err := store.List[model.Task](f.state, "task")
	if err != nil || len(tasks) != 1 || tasks[0].ID != existing.ID || tasks[0].Status != model.StatusQueued || tasks[0].RunID != nil {
		t.Fatalf("audit disturbed or queued executable work: %+v, %v", tasks, err)
	}
	assertAdmissions(t, f.state, f.cfg.PlanningCost(), "audit")
}

func TestRunOnceCommitsPlan(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	completePlan(t, f).queue(f)
	app := f.pausedApp(t)
	if err := control(app, "cycle"); err != nil {
		t.Fatal(err)
	}
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	cycle := waitOnlyCycle(t, f.state)
	assertPlanningPass(t, f, cycle)
	tasks, err := store.List[model.Task](f.state, "task")
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
	if tasks[0].Route.String() != f.routes.Executor.String() || tasks[0].Proposal.ID != "d0-feature" {
		t.Fatalf("planned task did not take the accepted proposal and its tier route: %+v", tasks[0])
	}
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

func TestFailedPlanningCommitsNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range []model.OperatingMode{model.OperatingModeRunOnce, model.OperatingModeContinuous} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFixture(t)
			plan := completePlan(t, f)
			plan.discovery[0] = runnertest.Reply{Answer: "this discovery answer is not JSON"}
			plan.queue(f)
			seeded := model.DefaultControl()
			seeded.IdleStreak = 2
			if err := f.state.SaveControl(seeded); err != nil {
				t.Fatal(err)
			}
			app := f.pausedApp(t)
			action := "cycle"
			if mode == model.OperatingModeContinuous {
				action = "resume"
			}
			if err := control(app, action); err != nil {
				t.Fatal(err)
			}
			before := time.Now().Unix()
			if err := app.tick(); err != nil {
				t.Fatal(err)
			}
			failed := waitOnlyCycle(t, f.state)
			app.wg.Wait()
			after := time.Now().Unix()
			if failed.Status != model.CycleFailed || failed.Error == nil || !strings.Contains(*failed.Error, "invalid JSON") {
				t.Fatalf("malformed discovery was accepted: %+v", failed)
			}
			statuses := map[string]int{}
			for _, session := range failed.Sessions {
				statuses[session.Status]++
			}
			if len(failed.Sessions) != int(1+f.cfg.DiscoveryAgents) || statuses[model.SessionFailed] != 1 {
				t.Fatalf("failed plan did not retain terminal evidence for every started role: %+v", failed.Sessions)
			}
			if turns := f.script.Turns(f.routes.ProposalReviewer); len(turns) != 0 {
				t.Fatalf("proposal review ran after discovery failed: %+v", turns)
			}
			tasks, err := store.List[model.Task](f.state, "task")
			if err != nil || len(tasks) != 0 {
				t.Fatalf("partial plan leaked tasks: %+v, %v", tasks, err)
			}
			memory, err := f.state.DecisionMemory(f.cfg.GitHubRepo)
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
			if delay := int64(2 * f.cfg.CycleIntervalSeconds); mode == model.OperatingModeContinuous && (control.NextCycleAt < before+delay || control.NextCycleAt > after+delay) {
				t.Fatalf("failed Continuous planning scheduled the next cycle at %d; want the streak's backoff %d after %d..%d", control.NextCycleAt, delay, before, after)
			}
			if events := planningErrors(t, f.state, failed.ID); len(events) != 1 || events[0].Message != *failed.Error {
				t.Fatalf("failed pass logged %+v; want one planning_error with the cycle error", events)
			}
			assertNoOpenClients(t, f.script)
		})
	}
}

func TestConsolidationCoversEveryProposal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plan := completePlan(t, f)
	plan.consolidation = runnertest.Reply{Answer: `{"proposals": []}`}
	plan.queue(f)
	app := f.pausedApp(t)
	cycleID, err := app.startAudit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cycle := waitCycle(t, f.state, cycleID)
	if cycle.Status != model.CycleFailed || cycle.Error == nil || !strings.Contains(*cycle.Error, "omitted or invented") || !strings.Contains(*cycle.Error, `omitted "d0-feature"`) {
		t.Fatalf("incomplete consolidation was accepted or its error does not name the omitted proposal: %+v", cycle)
	}
	tasks, err := store.List[model.Task](f.state, "task")
	if err != nil || len(tasks) != 0 {
		t.Fatalf("incomplete consolidation leaked executable work: %+v, %v", tasks, err)
	}
	app.wg.Wait()
	control, err := app.Control()
	if err != nil || control.Mode != model.OperatingModePaused || control.Error == nil || !strings.Contains(*control.Error, "omitted or invented") {
		t.Fatalf("failed audit did not retain durable control evidence: %+v, %v", control, err)
	}
	if events := planningErrors(t, f.state, cycleID); len(events) != 1 || events[0].Message != *cycle.Error {
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

func TestPlanningRejectsMutatedWorkspace(t *testing.T) {
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
			f := newFixture(t)
			plan := completePlan(t, f)
			test.mutate(&plan, test.mutation.effect)
			plan.queue(f)
			app := f.pausedApp(t)
			cycleID, err := app.startAudit(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cycle := waitCycle(t, f.state, cycleID)
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
					if _, err := os.Stat(roleWorkspace(f, cycle.ID, session.Role)); !os.IsNotExist(err) {
						t.Fatalf("successful %s clone remains: %v", session.Role, err)
					}
				default:
					t.Fatalf("nonterminal planning session: %+v", session)
				}
			}
			if failed == nil || !test.inStage(failed.Role) || !strings.Contains(failed.Summary, "modified its source snapshot") {
				t.Fatalf("mutated planning role lost terminal session evidence: %+v", cycle.Sessions)
			}
			if route := f.planningRoute(failed.Role); failed.Route.String() != route.String() {
				t.Fatalf("failed %s session recorded route %s; want %s", failed.Role, failed.Route, route)
			}
			workspace := roleWorkspace(f, cycle.ID, failed.Role)
			ranThere := false
			for _, turn := range f.planningTurns() {
				if turn.Session == failed.ID && turn.Cwd == workspace {
					ranThere = true
				}
			}
			if !ranThere {
				t.Fatalf("failed session %s has no scripted turn in %s", failed.ID, workspace)
			}
			test.mutation.preserved(t, workspace)

			if want := uint64(len(cycle.Sessions)); want != test.admissions(f.cfg.DiscoveryAgents) || uint64(len(f.planningTurns())) != want {
				t.Fatalf("planning ran %d sessions and %d turns; want %d before the failure", want, len(f.planningTurns()), test.admissions(f.cfg.DiscoveryAgents))
			}
			assertAdmissions(t, f.state, test.admissions(f.cfg.DiscoveryAgents), "planning turns before the failure")
			tasks, err := store.List[model.Task](f.state, "task")
			if err != nil || len(tasks) != 0 {
				t.Fatalf("mutated planning workspace leaked executable work: %+v, %v", tasks, err)
			}
			memory, err := f.state.DecisionMemory(f.cfg.GitHubRepo)
			if err != nil || len(memory) != 0 {
				t.Fatalf("mutated planning workspace leaked decision memory: %+v, %v", memory, err)
			}
			assertNoOpenClients(t, f.script)
		})
	}
}

func TestCommitTasksQueuesAccepted(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxRepairRounds, cfg.MaxNoProgressRounds, cfg.MaxRetries = 4, 2, 3
	cfg.TaskTimeoutSeconds, cfg.SessionTimeoutSeconds, cfg.CommandTimeoutSeconds = 14400, 1800, 600
	saveSettings(t, state, cfg, model.DefaultControl())
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)

	pr := ownedPR("octomus/existing")
	pr.Head = "pr-head"
	first := proposal("first", pr.Branch)
	second := proposal("second", pr.Branch)
	second.Dependencies = []string{"first"}
	fresh := proposal("fresh", cfg.DefaultBranch)
	rejected := proposal("rejected", cfg.DefaultBranch)
	rejected.Decision = model.DecisionRejected
	runID := "batch"
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: model.ID(), Number: 1, Status: model.CycleRunning,
		StartedAt: model.Now(), Repository: cfg.GitHubRepo, RunID: &runID,
		Grounding: &model.Grounding{Revision: "main-head", PRs: []model.PullRequest{pr}},
		Proposals: []model.Proposal{first, second, rejected, fresh}, Assessments: []any{}, Sessions: []model.Session{},
	}
	if err := app.commitTasks(cfg, &cycle); err != nil {
		t.Fatal(err)
	}
	if cycle.Status != model.CycleCompleted || cycle.CompletedAt == nil {
		t.Fatalf("a plan with accepted work finished as %s (completed_at %v)", cycle.Status, cycle.CompletedAt)
	}
	if !slices.Equal(cycle.Proposals[1].Dependencies, []string{"first"}) {
		t.Fatalf("the cycle's proposal dependencies were rewritten: %v", cycle.Proposals[1].Dependencies)
	}

	tasks, err := state.TasksForCycle(cycle.ID)
	if err != nil {
		t.Fatal(err)
	}
	byProposal := map[string]model.Task{}
	for _, task := range tasks {
		byProposal[task.Proposal.ID] = task
	}
	if len(tasks) != 3 || len(byProposal) != 3 {
		t.Fatalf("queued %d tasks for proposals %v; want first, second and fresh", len(tasks), byProposal)
	}
	if _, queued := byProposal["rejected"]; queued {
		t.Fatal("a rejected proposal was queued")
	}
	ids := map[string]struct{}{}
	policy := model.AttemptPolicy{
		MaxRepairRounds: cfg.MaxRepairRounds, MaxNoProgressRounds: cfg.MaxNoProgressRounds, MaxRetries: cfg.MaxRetries,
		TaskTimeoutSeconds: cfg.TaskTimeoutSeconds, SessionTimeoutSeconds: cfg.SessionTimeoutSeconds, CommandTimeoutSeconds: cfg.CommandTimeoutSeconds,
	}
	for name, task := range byProposal {
		if _, duplicate := ids[task.ID]; duplicate || task.ID == name {
			t.Fatalf("task %s for %s does not have its own identity", task.ID, name)
		}
		ids[task.ID] = struct{}{}
		if task.Status != model.StatusQueued || task.CycleID != cycle.ID || task.RunID == nil || *task.RunID != runID {
			t.Fatalf("%s is not a queued member of the plan's batch: status=%s cycle=%s run=%s", name, task.Status, task.CycleID, optionalText(task.RunID))
		}
		if task.Route.String() != cfg.Tiers[task.Proposal.Tier].String() || task.Config.GitHubRepo != cfg.GitHubRepo {
			t.Fatalf("%s did not snapshot its tier route and configuration: %s", name, task.Route)
		}
		if task.AttemptPolicy == nil || *task.AttemptPolicy != policy {
			t.Fatalf("%s attempt policy = %+v; want %+v", name, task.AttemptPolicy, policy)
		}
		if task.DefaultRevision != "main-head" || task.ComparisonBase != "" || task.Workspace != "" || task.CreatedAt == "" || task.CreatedAt != task.UpdatedAt {
			t.Fatalf("%s snapshot: default=%s base=%q workspace=%q created=%s updated=%s", name, task.DefaultRevision, task.ComparisonBase, task.Workspace, task.CreatedAt, task.UpdatedAt)
		}
		if len(task.Sessions) != 0 || len(task.Reviews) != 0 || len(task.Verification) != 0 || len(task.SupersededBy) != 0 || len(task.Supersedes) != 0 {
			t.Fatalf("%s did not start with empty history and lineage: %+v", name, task)
		}
	}

	head, next, own := byProposal["first"], byProposal["second"], byProposal["fresh"]
	if len(head.Proposal.Dependencies) != 0 || !slices.Equal(next.Proposal.Dependencies, []string{head.ID}) {
		t.Fatalf("dependencies were not mapped to task identities: first=%v second=%v", head.Proposal.Dependencies, next.Proposal.Dependencies)
	}
	for _, task := range []model.Task{head, next} {
		if task.Branch != pr.Branch || task.SourceRevision != pr.Head || task.PRNumber == nil || *task.PRNumber != pr.Number || task.PRURL == nil || *task.PRURL != pr.URL {
			t.Fatalf("%s does not write the owned PR from its head: branch=%s source=%s pr=%v", task.Proposal.ID, task.Branch, task.SourceRevision, task.PRNumber)
		}
	}
	if own.Branch != cfg.BranchPrefix+own.ID || own.SourceRevision != "main-head" || own.PRNumber != nil || own.PRURL != nil {
		t.Fatalf("default-branch task: branch=%s source=%s pr=%v", own.Branch, own.SourceRevision, own.PRNumber)
	}
}

func TestCommitTasksIdleWithoutWork(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	rejected := proposal("rejected", cfg.DefaultBranch)
	rejected.Decision = model.DecisionRejected
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: model.ID(), Number: 1, Status: model.CycleRunning,
		StartedAt: model.Now(), Repository: cfg.GitHubRepo,
		Grounding: &model.Grounding{Revision: "main-head"},
		Proposals: []model.Proposal{rejected}, Assessments: []any{}, Sessions: []model.Session{},
	}
	if err := app.commitTasks(cfg, &cycle); err != nil {
		t.Fatal(err)
	}
	if cycle.Status != model.CycleIdle || cycle.CompletedAt == nil {
		t.Fatalf("a plan without accepted work finished as %s (completed_at %v)", cycle.Status, cycle.CompletedAt)
	}
	if tasks, err := state.TasksForCycle(cycle.ID); err != nil || len(tasks) != 0 {
		t.Fatalf("a plan without accepted work queued %d tasks, %v", len(tasks), err)
	}
}

func TestDiscoveryAgentBound(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	scopes := uint64(len(discoveryScopes))
	accepted := 0
	for agents := uint64(0); agents <= 2*scopes; agents++ {
		cfg.DiscoveryAgents = agents
		for name, validate := range map[string]func(config.Config) error{
			"Validate(false)": func(c config.Config) error { return c.Validate(false) },
			"Validate(true)":  func(c config.Config) error { return c.Validate(true) },
			"ValidateAudit":   config.Config.ValidateAudit,
		} {
			if validate(cfg) != nil {
				continue
			}
			if name == "Validate(false)" {
				accepted++
			}
			if agents > scopes {
				t.Fatalf("%s accepts %d discovery agents; only %d scopes exist", name, agents, scopes)
			}
		}
	}
	if accepted == 0 {
		t.Fatal("no discovery agent count validated, so the bound was not checked")
	}

	app := New(testStore(t), t.TempDir())
	t.Cleanup(app.Shutdown)
	cfg.DiscoveryAgents = scopes + 1
	cycle := model.Cycle{ID: model.ID(), Mode: model.CycleModeAudit, Grounding: &model.Grounding{Revision: "revision"}}
	want := fmt.Sprintf("Discovery supports at most %d agents", scopes)
	if err := app.discover(context.Background(), cfg, &cycle, "ground", "{}"); err == nil || err.Error() != want {
		t.Fatalf("discover with %d agents = %v; want %q", cfg.DiscoveryAgents, err, want)
	}
	if len(cycle.Sessions) != 0 || len(cycle.Proposals) != 0 {
		t.Fatalf("refused discovery still ran: sessions=%d proposals=%d", len(cycle.Sessions), len(cycle.Proposals))
	}
}

func TestDecisionMemoryReconsideration(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := f.cfg
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(cfg.Repository, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Repository, "docs", "guide.md"), []byte("# Guide\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, cfg.Repository, "add", "docs/guide.md")
	git(t, cfg.Repository, "commit", "-m", "Add the guide")
	recorded := git(t, cfg.Repository, "rev-parse", "HEAD")

	future := time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	decision := func(id, target, problem, verdict, cycleID, after string, paths ...string) {
		t.Helper()
		if paths == nil {
			paths = []string{}
		}
		fingerprint, err := decisionFingerprint(ctx, cfg, recorded, paths)
		if err != nil {
			t.Fatal(err)
		}
		record := map[string]any{
			"id": id, "mode": "execution", "repository": cfg.GitHubRepo, "target": target,
			"problem_key": problem, "relevant_paths": paths, "decision": verdict,
			"reason": "Recorded reason", "source_revision": recorded,
			"context_fingerprint": fingerprint, "reconsider_after": after, "cycle_id": cycleID,
		}
		if err := f.state.Put("decision", id, record); err != nil {
			t.Fatal(err)
		}
	}
	decision("readme", "main", "readme-problem", model.DecisionRejected, "cycle-1", future, "README.md")
	decision("guide", "main", "guide-problem", model.DecisionRejected, "cycle-1", future, "docs/guide.md")
	decision("whole-tree", "main", "tree-problem", model.DecisionDeferred, "cycle-1", future)
	decision("expired", "main", "expired-problem", model.DecisionRejected, "cycle-1", past, "docs/guide.md")
	decision("pr-readme", "octomus/open", "pr-problem", model.DecisionRejected, "cycle-1", future, "README.md")
	decision("closed", "octomus/closed", "closed-problem", model.DecisionRejected, "cycle-1", future, "README.md")
	decision("keyless", "main", "", model.DecisionRejected, "cycle-1", future, "docs/guide.md")
	decision("merged-accepted", "main", "merged-problem", model.DecisionAccepted, "cycle-2", future, "docs/guide.md")
	decision("merged-absorbed", "main", "merged-problem", model.DecisionRejected, "cycle-2", future, "docs/guide.md")
	decision("merged-elsewhere", "main", "merged-problem", model.DecisionRejected, "cycle-3", future, "docs/guide.md")

	cancelled := queuedTask(cfg, "cancelled-task", "main", "octomus/cancelled-task")
	cancelled.Status = model.StatusCancelled
	cancelled.RediscoveryRequested = true
	putTask(t, f, cancelled)

	app := New(f.state, f.dataDir)
	t.Cleanup(app.Shutdown)
	pr := ownedPR("octomus/open")
	pr.Head = recorded
	check := func(label, revision string, wantDue map[string]bool) {
		t.Helper()
		memory, err := app.planningMemory(ctx, cfg, model.Grounding{Revision: revision, PRs: []model.PullRequest{pr}})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		due := map[string]bool{}
		for _, record := range memory.decisions {
			if _, duplicate := due[record.ID]; duplicate {
				t.Fatalf("%s: decision %s listed twice", label, record.ID)
			}
			due[record.ID] = record.ReconsiderationDue
		}
		if fmt.Sprint(due) != fmt.Sprint(wantDue) {
			t.Fatalf("%s: reconsideration_due by decision = %v; want %v", label, due, wantDue)
		}
		if len(memory.requests) != 1 {
			t.Fatalf("%s: rediscovery requests = %+v; want one", label, memory.requests)
		}
		rediscovery := memory.requests[0]
		if rediscovery.ID != cancelled.ID || rediscovery.Target != "main" || rediscovery.entry["title"] != cancelled.Proposal.Title {
			t.Fatalf("%s: rediscovery request = %+v", label, rediscovery)
		}
	}
	check("at the recorded revision", recorded, map[string]bool{
		"readme": false, "guide": false, "whole-tree": false, "expired": true,
		"pr-readme": false, "merged-accepted": false, "merged-elsewhere": false,
	})

	if err := os.WriteFile(filepath.Join(cfg.Repository, "README.md"), []byte("# Fixture\n\nThe contract changed.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, cfg.Repository, "commit", "-am", "Change the README")
	check("after a README change", git(t, cfg.Repository, "rev-parse", "HEAD"), map[string]bool{
		"readme": true, "guide": false, "whole-tree": true, "expired": true,
		"pr-readme": false, "merged-accepted": false, "merged-elsewhere": false,
	})
}

func TestDecisionMemoryAbsorbs(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	accepted := proposal("accepted", cfg.DefaultBranch)
	accepted.ProblemKey = "stable-problem"
	accepted.RelevantPaths = nil
	rejected := accepted
	rejected.ID = "alternative"
	rejected.Decision = model.DecisionRejected
	rejected.Reason = "Absorbed by the accepted scope"
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: model.ID(), Grounding: &model.Grounding{Revision: "revision"},
		Proposals: []model.Proposal{accepted, rejected}, Repository: cfg.GitHubRepo,
	}
	records, err := a.recordDecisions(context.Background(), cfg, cycle)
	if err != nil || len(records) != 1 {
		t.Fatalf("same-cycle alternative was not absorbed: %d, %v", len(records), err)
	}
	stored := records[0].(map[string]any)
	saved, err := wirejson.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	// The saved shape is the version-7 record: mode as text, an empty path list, no kind or reconsideration_due.
	want := fmt.Sprintf(`{"context_fingerprint":%q,"cycle_id":%q,"decision":"accepted","id":%q,"mode":"execution","problem_key":"stable-problem","reason":%q,"reconsider_after":%q,"relevant_paths":[],"repository":%q,"source_revision":"revision","target":%q}`,
		stored["context_fingerprint"], cycle.ID, cycle.ID+":accepted", accepted.Reason, stored["reconsider_after"], cfg.GitHubRepo, cfg.DefaultBranch)
	if string(saved) != want {
		t.Fatalf("decision record changed:\n%s\nwant\n%s", saved, want)
	}
	recorded := decisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeExecution,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionRejected, Reason: "Current decision", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	}
	if err := validateDecisionMemory([]model.Proposal{accepted}, decisionMemory{decisions: []decisionRecord{recorded}}); err == nil {
		t.Fatal("unchanged rejected work became executable without rediscovery")
	}
	auditRecommendation := decisionRecord{
		Kind: "decision", ID: model.ID(), CycleMode: model.CycleModeAudit,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionAccepted, Reason: "Audit recommendation", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: model.ID(),
	}
	if err := validateDecisionMemory([]model.Proposal{accepted}, decisionMemory{decisions: []decisionRecord{auditRecommendation}}); err != nil {
		t.Fatalf("audit recommendation incorrectly vetoed execution: %v", err)
	}
	requestID := model.ID()
	request := rediscoveryRequest{ID: requestID, Target: cfg.DefaultBranch}
	reconsidered := accepted.Clone()
	reconsidered.Reconsiders = []string{requestID}
	if err := validateDecisionMemory([]model.Proposal{reconsidered}, decisionMemory{decisions: []decisionRecord{recorded}, requests: []rediscoveryRequest{request}}); err != nil {
		t.Fatalf("matching explicit rediscovery was rejected: %v", err)
	}
	wrong := reconsidered.Clone()
	wrong.Target = "other"
	if err := validateDecisionMemory([]model.Proposal{wrong}, decisionMemory{requests: []rediscoveryRequest{request}}); err == nil {
		t.Fatal("rediscovery with the wrong target was accepted")
	}
	oversized := accepted.Clone()
	oversized.ProblemKey = strings.Repeat("x", 201)
	if err := validateDecisionMemory([]model.Proposal{oversized}, decisionMemory{}); err == nil {
		t.Fatal("oversized decision metadata was accepted")
	}
}

func TestAdmissionBudgetIsAtomic(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = 5
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results <- state.ReserveSession(0, store.NewAdmission("cycle", nil, fmt.Sprintf("role-%d", index), cfg.Roles["discovery"]))
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, model.BlockedBudgetExhausted) {
			t.Fatalf("unexpected admission error: %v", err)
		}
	}
	if successes != 5 {
		t.Fatalf("got %d successful admissions, want exactly 5", successes)
	}
	if capacity, err := state.PlanningCapacity(); err != nil || capacity.Used != 5 {
		t.Fatalf("durable usage = %+v, %v; want 5 used", capacity, err)
	}
}

func TestUnaffordableAuditChangesNothing(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := testConfig(t.TempDir())
	cfg.MaxSessionsPerDay = cfg.PlanningCost() - 1
	original := model.DefaultControl()
	saveSettings(t, state, cfg, original)
	a := New(state, t.TempDir())
	t.Cleanup(a.Shutdown)
	if _, err := a.startAudit(context.Background()); err == nil {
		t.Fatal("unaffordable audit started")
	}
	control, err := a.Control()
	if err != nil || !reflect.DeepEqual(control, original) {
		t.Fatalf("unaffordable audit changed control: %+v, %v", control, err)
	}
	cycles, err := store.List[model.Cycle](state, "cycle")
	if err != nil || len(cycles) != 0 {
		t.Fatalf("unaffordable audit created a cycle: %d, %v", len(cycles), err)
	}
	assertAdmissions(t, state, 0, "unaffordable audit")
}
