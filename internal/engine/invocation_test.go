package engine

// Role invocation: one admission per turn, the reviewer's diff, and redaction of every recorded summary.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
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
	if saved.Status != model.StatusBlocked || saved.BlockedReason == nil || *saved.BlockedReason != model.BlockedRunnerUnavailable {
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
	if err := app.TaskAction(task.ID, "retry"); err != nil {
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
		t.Parallel()
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
		t.Parallel()
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
		cycleID, err := app.startAudit(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cycle := waitCycle(t, app, cycleID)
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

// firstTurnLoss models Codex's response after a disconnect before turn/start was
// accepted. The scripted adapter handles every later, normally accepted turn.
type releaseFailureClient struct {
	runner.Adapter
}

func (c *releaseFailureClient) Close() error {
	return errors.Join(c.Adapter.Close(), errors.New("Sandbox removal was not confirmed"))
}

type firstTurnLoss struct {
	mu      sync.Mutex
	route   config.Route
	missing string
	resumes int
}

type losingClient struct {
	runner.Adapter
	loss *firstTurnLoss
}

func (c *losingClient) SandboxEvidence() *model.SandboxRecord {
	return c.Adapter.(interface{ SandboxEvidence() *model.SandboxRecord }).SandboxEvidence()
}

func (c *losingClient) Start(route config.Route, cwd string, resume *string) (string, error) {
	c.loss.mu.Lock()
	missing := resume != nil && *resume == c.loss.missing
	if missing {
		c.loss.resumes++
	}
	c.loss.mu.Unlock()
	if missing {
		return "", runner.ErrSessionMissing
	}
	return c.Adapter.Start(route, cwd, resume)
}

func (c *losingClient) Turn(session string, route config.Route, cwd, prompt string, schema schemas.Schema, started func() error) (string, error) {
	c.loss.mu.Lock()
	lose := route.Model == c.loss.route.Model && c.loss.missing == ""
	if lose {
		c.loss.missing = session
	}
	c.loss.mu.Unlock()
	if lose {
		return "", errors.New("Disconnected before first-turn acceptance")
	}
	return c.Adapter.Turn(session, route, cwd, prompt, schema, started)
}

func TestUnstartedSessionRecovery(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"executor", "repair"} {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
			route := f.routes.Executor
			initial := "fixed output\n"
			if role == "repair" {
				route, initial = f.routes.Repair, "draft\n"
			}
			loss := &firstTurnLoss{route: route}
			connect := f.script.Connector()
			wrapped := func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
				client, err := connect(ctx, backend, cfg, cwd)
				if err != nil {
					return nil, err
				}
				return &losingClient{Adapter: client, loss: loss}, nil
			}
			f.script.RecordSandbox(&model.SandboxRecord{ImageID: "sha256:first-turn", Runs: 1})
			f.script.Queue(f.routes.Executor, runnertest.Reply{Answer: "Implemented feature", Effect: writeFile("feature.txt", initial)})
			f.script.Answer(f.routes.Reviewer, cleanReview("Reviewed"), cleanReview("Reviewed retry"), cleanReview("Reviewed repair"))
			f.script.Queue(f.routes.Repair, runnertest.Reply{Answer: "Repaired feature", Effect: writeFile("feature.txt", "fixed output\n")})
			task := executionTask(t, f, f.cfg.DefaultBranch)
			putTask(t, f, task)
			app := f.newApp(t, WithRunnerConnector(wrapped))
			blocked := driveTask(t, f, app, task.ID)
			sessions := sessionByRole(blocked, role)
			if blocked.Status != model.StatusBlocked || len(sessions) != 1 || sessions[0].FirstTurnStarted == nil || *sessions[0].FirstTurnStarted {
				t.Fatalf("first-turn failure was not retained as unstarted: %+v", blocked)
			}
			old := sessions[0]
			if old.Sandbox == nil || old.Sandbox.Runs != 1 {
				t.Fatalf("first attempt evidence = %+v", old)
			}
			marker := filepath.Join(blocked.Workspace, "retained.txt")
			if err := os.WriteFile(marker, []byte("retain workspace\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := app.TaskAction(task.ID, "retry"); err != nil {
				t.Fatal(err)
			}
			saved := driveTask(t, f, app, task.ID)
			if saved.Status != model.StatusPublished || saved.Attempts != 1 || saved.Workspace != blocked.Workspace {
				t.Fatalf("retry did not recover in its existing workspace: %+v", saved)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "retain workspace\n" {
				t.Fatalf("workspace was replaced: %q, %v", data, err)
			}
			sessions = sessionByRole(saved, role)
			if len(sessions) != 2 || sessions[0].ID != old.ID || sessions[0].Status != model.SessionFailed ||
				!strings.Contains(sessions[0].Summary, old.Summary) || sessions[0].Sandbox.Runs != 2 ||
				sessions[1].ID == old.ID || sessions[1].Status != model.SessionCompleted || sessions[1].FirstTurnStarted == nil || !*sessions[1].FirstTurnStarted {
				t.Fatalf("recovery lost failed evidence or durable session identity: %+v", sessions)
			}
			if got := admissionsByRole(t, f.state)[role]; got != 3 {
				t.Fatalf("admissions = %d, want failed turn, failed resume, replacement", got)
			}
			if loss.resumes != 1 {
				t.Fatalf("missing thread resumed %d times", loss.resumes)
			}
			assertNoOpenClients(t, f.script)
		})
	}
}

func TestSessionRecoveryPreservesEstablishedAndUnknownThreads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		started      *bool
		startErr     error
		cleanupFails bool
	}{
		{"established", new(true), runner.ErrSessionMissing, false},
		{"legacy unknown", nil, runner.ErrSessionMissing, false},
		{"transport failure", new(false), errors.New("no rollout found during transport disconnect"), false},
		{"unconfirmed cleanup", new(false), runner.ErrSessionMissing, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			task := executionTask(t, f, f.cfg.DefaultBranch)
			session := model.NewSession("retained-thread", "repair", f.routes.Repair)
			session.FirstTurnStarted = tc.started
			session.MarkFailed("Earlier failure evidence")
			task.Sessions = append(task.Sessions, session)
			task.RepairSession = &session.ID
			putTask(t, f, task)
			f.script.FailStart(f.routes.Repair, tc.startErr)
			app := f.pausedApp(t)
			connect := f.script.Connector()
			if tc.cleanupFails {
				plain := connect
				connect = func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
					client, err := plain(ctx, backend, cfg, cwd)
					if err != nil {
						return nil, err
					}
					return &releaseFailureClient{Adapter: client}, nil
				}
			}
			clients := runner.New(context.Background(), f.cfg, connect)
			defer clients.Close()
			_, err := app.invoke(context.Background(), clients, invocation{
				cycleID: task.CycleID, task: &task, role: "repair", route: f.routes.Repair, workspace: f.repo,
				resume: task.RepairSession, keep: func(id *string) { task.RepairSession = id },
			})
			if !errors.Is(err, tc.startErr) || task.RepairSession == nil || *task.RepairSession != session.ID || len(task.Sessions) != 1 || len(f.script.Starts(f.routes.Repair)) != 1 {
				t.Fatalf("unsafe fresh-session fallback: task=%+v, error=%v", task, err)
			}
			if task.Sessions[0].Summary != session.Summary {
				t.Fatal("original failure evidence changed")
			}
			assertNoOpenClients(t, f.script)
		})
	}
}

func TestFirstTurnCheckpointFailureStopsWork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)
	if err := f.state.Snapshot(func(c *sql.Conn) error {
		_, err := c.ExecContext(context.Background(), `CREATE TRIGGER refuse_first_turn BEFORE UPDATE ON records
			WHEN NEW.kind='task' AND EXISTS (SELECT 1 FROM json_each(NEW.data,'$.sessions') WHERE json_extract(value,'$.first_turn_started')=1)
			BEGIN SELECT RAISE(FAIL,'first-turn checkpoint refused'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.script.Queue(f.routes.Executor, runnertest.Reply{Answer: "Should not execute", Effect: writeFile("unexpected.txt", "must not exist")})
	app := f.pausedApp(t)
	clients := runner.New(context.Background(), f.cfg, f.script.Connector())
	defer clients.Close()
	_, err := app.invoke(context.Background(), clients, invocation{
		cycleID: task.CycleID, task: &task, role: "executor", route: f.routes.Executor, workspace: f.repo,
		keep: func(id *string) { task.ExecutionSession = id },
	})
	if err == nil || !strings.Contains(err.Error(), "first-turn checkpoint refused") || errors.Is(err, model.BlockedRunnerUnavailable) {
		t.Fatalf("failed checkpoint did not stop invocation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "unexpected.txt")); !os.IsNotExist(err) {
		t.Fatalf("work continued after checkpoint failure: %v", err)
	}
	assertNoOpenClients(t, f.script)
}

func TestSessionReplacementRequiresAdmission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.MaxSessionsPerDay = 1 })
	task := executionTask(t, f, f.cfg.DefaultBranch)
	session := model.NewSession("unstarted-thread", "repair", f.routes.Repair)
	session.FirstTurnStarted = new(false)
	session.MarkFailed("First-turn transport failure")
	task.Sessions = append(task.Sessions, session)
	task.RepairSession = &session.ID
	putTask(t, f, task)
	f.script.FailStart(f.routes.Repair, runner.ErrSessionMissing)
	app := f.pausedApp(t)
	clients := runner.New(context.Background(), f.cfg, f.script.Connector())
	defer clients.Close()
	_, err := app.invoke(context.Background(), clients, invocation{
		cycleID: task.CycleID, task: &task, role: "repair", route: f.routes.Repair, workspace: f.repo,
		resume: task.RepairSession, keep: func(id *string) { task.RepairSession = id },
	})
	if err == nil || len(f.script.Starts(f.routes.Repair)) != 1 || len(f.script.Turns(f.routes.Repair)) != 0 {
		t.Fatalf("replacement bypassed exhausted admission budget: %v", err)
	}
	saved := loadTask(t, f.state, task.ID)
	if saved.RepairSession != nil || len(saved.Sessions) != 1 || !strings.Contains(saved.Sessions[0].Summary, session.Summary) {
		t.Fatalf("missing session remained active or lost its evidence: %+v", saved)
	}
	assertAdmissions(t, f.state, 1, "only the failed resume was admitted")
	assertNoOpenClients(t, f.script)
}
