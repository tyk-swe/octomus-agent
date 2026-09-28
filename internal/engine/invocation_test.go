package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func admissionsByRole(t *testing.T, state *store.Store) map[string]int {
	t.Helper()
	counts := map[string]int{}
	err := state.Snapshot(func(c *sql.Conn) error {
		rows, err := c.QueryContext(store.Background(), "SELECT data FROM admissions ORDER BY at,id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				return err
			}
			var admission store.Admission
			if err := json.Unmarshal([]byte(data), &admission); err != nil {
				return err
			}
			counts[admission.Role]++
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestInvocationAdmitsExactlyOncePerTurn(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t, withGitHubIdentity())
	fixture.configure(t, func(cfg *config.Config) {
		cfg.VerificationCommands = []string{"grep -q fixed feature.txt"}
	})
	routes, script := fixture.routes, fixture.script
	script.FailStart(routes.Executor, errors.New("scripted executor start failure"))
	task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
	saveExecutionTask(t, fixture.planningFixture, task)
	app := fixture.newApp(t)

	saved := driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonRunnerUnavailable {
		t.Fatalf("failed start outcome = %+v", saved)
	}
	if saved.ExecutionSession != nil || len(saved.Sessions) != 0 {
		t.Fatalf("a failed start recorded a session: %+v", saved)
	}
	if got := admissionsByRole(t, fixture.state); len(got) != 1 || got["executor"] != 1 {
		t.Fatalf("admissions after failed start = %+v; want the one reserved executor admission", got)
	}

	script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted feature.txt", Effect: writeFile("feature.txt", "draft\n")})
	script.Answer(routes.Reviewer, cleanReview("Draft reviewed"), cleanReview("Second draft reviewed"), cleanReview("Fix reviewed"))
	script.Queue(routes.Repair,
		runnertest.Reply{Answer: "Rewrote the draft", Effect: writeFile("feature.txt", "second draft\n")},
		runnertest.Reply{Answer: "Wrote the fixed output", Effect: writeFile("feature.txt", "fixed output\n")})
	if err := app.TaskAction(context.Background(), task.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	saved = driveTask(t, fixture.planningFixture, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("retried delivery = %+v", saved)
	}

	want := map[string]int{"executor": 2, "reviewer": 3, "repair": 2}
	if got := admissionsByRole(t, fixture.state); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("admissions by role = %+v; want %+v (the failed start plus one per turn)", got, want)
	}
	assertAdmissions(t, fixture.state, 7, "one per attempt and turn")
	for route, turns := range map[config.Route]int{routes.Executor: 1, routes.Reviewer: 3, routes.Repair: 2} {
		if n := len(script.Turns(route)); n != turns {
			t.Fatalf("%s turns = %d, want %d", route, n, turns)
		}
	}
	repairStarts := script.Starts(routes.Repair)
	if len(repairStarts) != 2 || repairStarts[0].Resume != nil || repairStarts[1].Resume == nil ||
		*repairStarts[1].Resume != repairStarts[0].Session || saved.RepairSession == nil || *saved.RepairSession != repairStarts[0].Session {
		t.Fatalf("the repair thread must persist and resume: %+v (recorded %v)", repairStarts, saved.RepairSession)
	}
	if repairs := sessionByRole(saved, "repair"); len(repairs) != 1 || repairs[0].Status != model.SessionCompleted {
		t.Fatalf("repair session records = %+v", repairs)
	}
	for _, start := range script.Starts(routes.Executor) {
		if start.Resume != nil {
			t.Fatalf("a fresh executor session was resumed: %+v", start)
		}
	}
}

func TestAdmissionMeasuresPastUnreadableWorkspaceDirectories(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	state := testStore(t)
	data := t.TempDir()
	app := New(state, data)
	t.Cleanup(app.Shutdown)
	cfg := testConfig(t.TempDir())
	saveSettings(t, state, cfg, model.DefaultControl())
	workspaceDir := filepath.Join(data, "tasks", "t1", "workspace")
	locked := filepath.Join(workspaceDir, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	readable := []byte("readable workspace bytes")
	if err := os.WriteFile(filepath.Join(workspaceDir, "notes.txt"), readable, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "inner", "hidden.txt"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	if err := app.admit("cycle-1", nil, "discovery", cfg.Roles["discovery"]); err != nil {
		t.Fatalf("admission with an unreadable workspace directory = %v; want a reserved session", err)
	}
	if used, err := state.SessionsToday(); err != nil || used != 1 {
		t.Fatalf("sessions today = %d, %v; want 1", used, err)
	}
	if err := app.measureStorage(cfg); err != nil {
		t.Fatalf("storage measurement with an unreadable workspace directory = %v", err)
	}
	saved, err := store.Get[storageUsage](state, "settings", "storage")
	if err != nil || saved == nil {
		t.Fatalf("saved storage = %+v, %v", saved, err)
	}
	if saved.TaskBytes != uint64(len(readable)) || saved.ApplicationBytes < saved.TaskBytes {
		t.Fatalf("saved storage = %+v; want %d readable task bytes", saved, len(readable))
	}
	if info, err := os.Lstat(locked); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("measurement changed the locked directory: %v, %v; want mode 0", info, err)
	}
}

func TestInvocationRejectsReservedResume(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	script := runnertest.New()
	clients := runner.New(context.Background(), config.Default(), script.Connector())
	t.Cleanup(func() { _ = clients.Close() })
	thread := "executor-thread"
	task := &model.Task{ID: "reserved-resume", CycleID: "cycle", ExecutionSession: &thread,
		Sessions: []model.Session{{ID: thread, Role: "executor", Status: model.SessionRunning}}}

	_, err := app.invoke(context.Background(), clients, invocation{
		cycleID: task.CycleID, task: task, role: "executor", route: config.NewRoute("scripted-executor", "medium"),
		workspace: t.TempDir(), resume: task.ExecutionSession, prompt: "unused", reserved: true,
	})
	if err == nil || !strings.Contains(err.Error(), "reserved admission") {
		t.Fatalf("reserved resume error = %v", err)
	}
	if calls := script.Calls(); len(calls) != 0 {
		t.Fatalf("a refused turn reached the runner: %+v", calls)
	}
	assertAdmissions(t, state, 0, "a refused turn admits nothing")
}

func TestInvocationSkipsCancelledOwner(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	script := runnertest.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clients := runner.New(ctx, config.Default(), script.Connector())
	prepared := false

	answer, err := app.invoke(ctx, clients, invocation{
		cycleID: "cycle", role: "discovery-0", route: config.NewRoute("scripted-discovery", "medium"),
		workspace: t.TempDir(), prompt: "unused", ownsClients: true,
		prepare: func() error { prepared = true; return nil },
	})
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "Operation cancelled") {
		t.Fatalf("cancelled owner error = %v", err)
	}
	if answer != "" || prepared {
		t.Fatalf("a cancelled turn answered %q or prepared its workspace (%v)", answer, prepared)
	}
	if calls := script.Calls(); len(calls) != 0 {
		t.Fatalf("a cancelled turn reached the runner: %+v", calls)
	}
	assertAdmissions(t, state, 0, "a cancelled owner admits nothing")
	assertNoOpenClients(t, script)
}

func TestInvocationCloseFailureFailsPlanningTurn(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	app := New(state, t.TempDir())
	t.Cleanup(app.Shutdown)
	cycleID := "close-failure-cycle"
	cycle := model.Cycle{
		Mode: model.CycleModeAudit, ID: cycleID, Number: 1, Status: model.CycleRunning, StartedAt: model.Now(),
		Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
	}
	if err := state.Put("cycle", cycleID, cycle); err != nil {
		t.Fatal(err)
	}
	route := config.NewRoute("scripted-discovery", "medium")
	script := runnertest.New(runnertest.CatalogFor(route)...)
	script.Answer(route, "Discovered")
	script.FailClose(route.Backend, errors.New("fixture close"))
	clients := runner.New(context.Background(), config.Default(), script.Connector())

	answer, err := app.invoke(context.Background(), clients, invocation{
		cycleID: cycleID, role: "discovery-0", route: route, workspace: t.TempDir(),
		prompt: "Discover", ownsClients: true,
	})
	if err == nil || !strings.Contains(err.Error(), "fixture close") {
		t.Fatalf("close failure error = %v", err)
	}
	if answer != "" {
		t.Fatalf("a turn whose clients failed to close kept its answer %q", answer)
	}
	turns := script.Turns(route)
	if len(turns) != 1 {
		t.Fatalf("turns = %+v; want the one discovery turn", turns)
	}
	saved, err := store.Get[model.Cycle](state, "cycle", cycleID)
	if err != nil || saved == nil {
		t.Fatalf("load cycle: %+v, %v", saved, err)
	}
	if len(saved.Sessions) != 1 || saved.Sessions[0].ID != turns[0].Session || saved.Sessions[0].Role != "discovery-0" ||
		saved.Sessions[0].Status != model.SessionFailed || !strings.Contains(saved.Sessions[0].Summary, "fixture close") {
		t.Fatalf("cycle sessions = %+v; want the discovery session failed with the close error", saved.Sessions)
	}
	assertAdmissions(t, state, 1, "the one discovery turn")
	assertNoOpenClients(t, script)
}

const secretToken = "ghp_invocationSecret0123456789"

func assertRedactedSummaries(t *testing.T, sessions []model.Session, roles []string) {
	t.Helper()
	seen := map[string]int{}
	for _, session := range sessions {
		seen[session.Role]++
		if strings.Contains(session.Summary, "ghp_") || !strings.Contains(session.Summary, "[redacted]") {
			t.Fatalf("%s session summary was not redacted: %q", session.Role, session.Summary)
		}
	}
	for _, role := range roles {
		if seen[role] == 0 {
			t.Fatalf("no %s session recorded: %+v", role, sessions)
		}
	}
}

func scriptedProposal(id, decision string) map[string]any {
	proposal := fixtureProposal(decision, "Found with "+secretToken)
	proposal["id"], proposal["title"], proposal["problem_key"] = id, "Scripted proposal "+id, "scripted-"+id
	return proposal
}

func TestInvocationRedactsEveryRoleSummary(t *testing.T) {
	t.Parallel()
	t.Run("task roles", func(t *testing.T) {
		fixture := newScriptedFixture(t, withGitHubIdentity())
		fixture.configure(t, func(cfg *config.Config) {
			cfg.VerificationCommands = []string{"grep -q fixed feature.txt"}
		})
		routes, script := fixture.routes, fixture.script
		script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted with " + secretToken, Effect: writeFile("feature.txt", "draft\n")})
		script.Answer(routes.Reviewer, cleanReview("Reviewed with "+secretToken), cleanReview("Re-reviewed with "+secretToken))
		script.Queue(routes.Repair, runnertest.Reply{Answer: "Repaired with " + secretToken, Effect: writeFile("feature.txt", "fixed output\n")})
		task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
		saveExecutionTask(t, fixture.planningFixture, task)

		saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
		if saved.Status != model.StatusPublished {
			t.Fatalf("scripted task did not publish: %+v", saved)
		}
		assertRedactedSummaries(t, saved.Sessions, []string{"executor", "reviewer", "repair"})
	})

	t.Run("planning roles", func(t *testing.T) {
		fixture := newScriptedFixture(t, withGitHubIdentity())
		routes, script := fixture.routes, fixture.script
		ids := []string{}
		for i := uint64(0); i < fixture.cfg.DiscoveryAgents; i++ {
			id := fmt.Sprintf("d%d-scripted", i)
			ids = append(ids, id)
			script.Answer(routes.Discovery, mustJSON(t, map[string]any{"proposals": []any{scriptedProposal(id, "candidate")}}))
		}
		assessments, consolidated := []any{}, []any{}
		for _, id := range ids {
			assessments = append(assessments, map[string]any{"id": id, "decision": "rejected", "reason": "Rejected with " + secretToken})
			consolidated = append(consolidated, scriptedProposal(id, "rejected"))
		}
		script.Answer(routes.Orchestrator,
			mustJSON(t, map[string]any{"context": "Grounded with " + secretToken}),
			mustJSON(t, map[string]any{"proposals": consolidated}))
		review := mustJSON(t, map[string]any{"assessments": assessments})
		script.Answer(routes.ProposalReviewer, review, review)

		app := fixture.pausedApp(t)
		cycleID, err := app.StartAudit(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cycle := waitCycle(t, fixture.state, cycleID)
		if cycle.Status == model.CycleFailed || cycle.Status == model.CycleRunning {
			t.Fatalf("scripted audit did not finish cleanly: %+v", cycle)
		}
		roles := []string{"grounding", "adversary-a", "adversary-b", "consolidation"}
		for i := uint64(0); i < fixture.cfg.DiscoveryAgents; i++ {
			roles = append(roles, fmt.Sprintf("discovery-%d", i))
		}
		if len(cycle.Sessions) != len(roles) {
			t.Fatalf("planning sessions = %+v; want one per role %v", cycle.Sessions, roles)
		}
		assertRedactedSummaries(t, cycle.Sessions, roles)
	})
}
