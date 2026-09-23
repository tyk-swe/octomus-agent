package engine

// Role invocation tests: admission accounting and redaction hold for every
// role because every agent turn runs through the one invocation module.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// admissionsByRole counts the admission ledger per role.
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

// TestInvocationAdmitsExactlyOncePerTurn: a failed executor start keeps the one
// admission initialization reserved for it, the retry reserves exactly one more
// for the executor turn, and a two-round repair records one admission per
// executor, reviewer and repair turn, with the repair thread resumed.
func TestInvocationAdmitsExactlyOncePerTurn(t *testing.T) {
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
	if used, err := fixture.state.SessionsToday(); err != nil || used != 7 {
		t.Fatalf("admission counter = %d, %v; want 7", used, err)
	}
	// Every admission beyond the failed start paid for exactly one turn.
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

// secretToken is secret-shaped: the store's redaction replaces it.
const secretToken = "ghp_invocationSecret0123456789"

// assertRedactedSummaries requires every listed role's sessions to be recorded
// with a summary that kept the surrounding text but lost the secret.
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

// scriptedProposal is a schema-complete discovery proposal whose reason
// carries the secret.
func scriptedProposal(id, decision string) map[string]any {
	return map[string]any{
		"id": id, "title": "Scripted proposal " + id, "problem": "A scripted problem.", "benefit": "A scripted benefit.",
		"category": "features", "target": "main", "tier": "M", "scope": "Scripted scope.", "prompt": "Scripted prompt.",
		"decision": decision, "reason": "Found with " + secretToken, "problem_key": "scripted-" + id,
		"evidence": []string{"README.md"}, "dependencies": []string{}, "relevant_paths": []string{}, "reconsiders": []string{},
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestInvocationRedactsEveryRoleSummary: a secret-shaped answer is saved
// redacted in the session summary of every task role and every planning role.
func TestInvocationRedactsEveryRoleSummary(t *testing.T) {
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

		// Audits require the paused service, so the app is not resumed.
		app := New(fixture.state, fixture.dataDir, WithRunnerConnector(script.Connector()))
		t.Cleanup(app.Shutdown)
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
