package engine

import (
	"slices"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// commitTasks queues one task per accepted proposal of a finished execution
// plan. Dependencies map to the new task identities while the cycle keeps the
// proposal identities; a task on an owned PR writes that PR's branch from its
// head, and a default-branch task gets a fresh owned branch from the grounded
// revision. Every task snapshots the tier route, configuration and attempt
// policy, and joins the plan's Run once batch.
func TestCommitTasksQueuesEachAcceptedProposal(t *testing.T) {
	state := testStore(t)
	cfg := testConfig(t.TempDir())
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
			t.Fatalf("%s is not a queued member of the plan's batch: status=%s cycle=%s run=%v", name, task.Status, task.CycleID, task.RunID)
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

// A plan that accepts nothing finishes idle and queues nothing.
func TestCommitTasksFinishesAPlanWithoutAcceptedWorkIdle(t *testing.T) {
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

// Each planned task owns its attempt policy and batch identity: adjusting one
// task's copy changes neither another task's nor the cycle's.
func TestPlannedTasksOwnTheirSnapshots(t *testing.T) {
	cfg := testConfig(t.TempDir())
	runID := "batch"
	cycle := &model.Cycle{ID: model.ID(), RunID: &runID, Grounding: &model.Grounding{Revision: "main-head"}}
	first := newPlannedTask(cfg, cycle, proposal("first", cfg.DefaultBranch), proposal("first", cfg.DefaultBranch), "task-first", nil)
	second := newPlannedTask(cfg, cycle, proposal("second", cfg.DefaultBranch), proposal("second", cfg.DefaultBranch), "task-second", nil)
	first.AttemptPolicy.MaxRetries++
	*first.RunID = "changed"
	if second.AttemptPolicy.MaxRetries != cfg.MaxRetries || *second.RunID != "batch" || runID != "batch" {
		t.Fatalf("planned tasks share snapshots: second policy retries=%d, second run=%s, cycle run=%s", second.AttemptPolicy.MaxRetries, *second.RunID, runID)
	}
}
