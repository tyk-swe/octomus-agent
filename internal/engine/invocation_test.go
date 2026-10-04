package engine

// Role invocation: one admission per turn, the reviewer's diff, and redaction of every recorded summary.

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

func admissionsByRole(t *testing.T, state *store.Store) map[string]int {
	t.Helper()
	counts := map[string]int{}
	err := state.Snapshot(func(c *sql.Conn) error {
		rows, err := c.QueryContext(context.Background(), "SELECT data FROM admissions ORDER BY at,id")
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

func TestInvocationAdmitsOncePerTurn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
	routes, script := f.routes, f.script
	script.FailStart(routes.Executor, errors.New("scripted executor start failure"))
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)
	app := f.newApp(t)

	saved := driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedReasonRunnerUnavailable {
		t.Fatalf("failed start outcome = %+v", saved)
	}
	if saved.ExecutionSession != nil || len(saved.Sessions) != 0 {
		t.Fatalf("a failed start recorded a session: %+v", saved)
	}
	if got := admissionsByRole(t, f.state); len(got) != 1 || got["executor"] != 1 {
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
	saved = driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("retried delivery = %+v", saved)
	}

	want := map[string]int{"executor": 2, "reviewer": 3, "repair": 2}
	if got := admissionsByRole(t, f.state); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("admissions by role = %+v; want %+v (the failed start plus one per turn)", got, want)
	}
	assertAdmissions(t, f.state, 7, "one per attempt and turn")
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

// The fresh reviewer shares the executor's home, which can change what git inside its sandbox shows, so it is given
// the change set as the orchestrator's own git sees it.
func TestReviewerGetsOrchestratorDiff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Wrote feature.txt", Effect: writeFile("feature.txt", "fixed output\n")})
	script.Answer(routes.Reviewer, cleanReview("Reviewed"))
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)
	saved := driveTask(t, f, f.newApp(t), task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task = %+v; want it published", saved)
	}
	turns := script.Turns(routes.Reviewer)
	if len(turns) != 1 {
		t.Fatalf("reviewer turns = %d", len(turns))
	}
	for _, want := range []string{"\n1\t0\tfeature.txt\n", "Complete diff:\ndiff --git a/feature.txt b/feature.txt", "+fixed output", "is authoritative"} {
		if !strings.Contains(turns[0].Prompt, want) {
			t.Fatalf("reviewer prompt lacks %q:\n%s", want, turns[0].Prompt)
		}
	}
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

func TestSummariesAreRedacted(t *testing.T) {
	t.Parallel()
	t.Run("task roles", func(t *testing.T) {
		f := newFixture(t)
		f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
		routes, script := f.routes, f.script
		script.Queue(routes.Executor, runnertest.Reply{Answer: "Drafted with " + secretToken, Effect: writeFile("feature.txt", "draft\n")})
		script.Answer(routes.Reviewer, cleanReview("Reviewed with "+secretToken), cleanReview("Re-reviewed with "+secretToken))
		script.Queue(routes.Repair, runnertest.Reply{Answer: "Repaired with " + secretToken, Effect: writeFile("feature.txt", "fixed output\n")})
		task := executionTask(t, f, f.cfg.DefaultBranch)
		putTask(t, f, task)

		saved := driveTask(t, f, f.newApp(t), task.ID)
		if saved.Status != model.StatusPublished {
			t.Fatalf("scripted task did not publish: %+v", saved)
		}
		assertRedactedSummaries(t, saved.Sessions, []string{"executor", "reviewer", "repair"})
	})

	t.Run("planning roles", func(t *testing.T) {
		f := newFixture(t)
		routes, script := f.routes, f.script
		ids := []string{}
		for i := uint64(0); i < f.cfg.DiscoveryAgents; i++ {
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

		app := f.pausedApp(t)
		cycleID, err := app.StartAudit(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cycle := waitCycle(t, f.state, cycleID)
		if cycle.Status == model.CycleFailed || cycle.Status == model.CycleRunning {
			t.Fatalf("scripted audit did not finish cleanly: %+v", cycle)
		}
		roles := []string{"grounding", "adversary-a", "adversary-b", "consolidation"}
		for i := uint64(0); i < f.cfg.DiscoveryAgents; i++ {
			roles = append(roles, fmt.Sprintf("discovery-%d", i))
		}
		if len(cycle.Sessions) != len(roles) {
			t.Fatalf("planning sessions = %+v; want one per role %v", cycle.Sessions, roles)
		}
		assertRedactedSummaries(t, cycle.Sessions, roles)
	})
}
