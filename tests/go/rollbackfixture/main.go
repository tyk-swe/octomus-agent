// Command rollbackfixture writes and reopens synthetic Go state for the
// cross-language storage checks in tests/go_storage.py and the upgrade
// rehearsal in tests/go_upgrade.py. It is a test helper, not part of the
// shipped executable: every record it writes is synthetic and the state
// directory it receives is a temporary fixture.
//
//	rollbackfixture write <state.db>     writes a Go-authored state database
//	rollbackfixture reopen <state.db>    reopens an existing database read-mostly
//	rollbackfixture rehearsal <state.db> extends an already-migrated database
//	                                     with publication-checkpoint tasks
//
// All modes print one JSON object describing what they saw on stdout.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	cycleID    = "33333333-3333-4333-8333-333333333333"
	taskID     = "44444444-4444-4444-8444-444444444444"
	decisionID = "55555555-5555-4555-8555-555555555555"

	rehearsalCycleID     = "66666666-6666-4666-8666-666666666666"
	rehearsalPublishing  = "77777777-7777-4777-8777-777777777777"
	rehearsalUncertainID = "88888888-8888-4888-8888-888888888888"
	rehearsalDecisionID  = "99999999-9999-4999-8999-999999999999"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: rollbackfixture write|reopen <state.db>")
		os.Exit(2)
	}
	var (
		summary map[string]any
		err     error
	)
	switch os.Args[1] {
	case "write":
		summary, err = write(os.Args[2])
	case "reopen":
		summary, err = reopen(os.Args[2])
	case "rehearsal":
		summary, err = rehearsal(os.Args[2])
	default:
		err = fmt.Errorf("unknown mode %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}

func ptr[T any](v T) *T { return &v }

func proposal(id, decision string) model.Proposal {
	return model.Proposal{
		ID: id, Title: "Synthetic " + id, Problem: "Missing behavior",
		Benefit: "Useful behavior", Scope: "one file", Evidence: []string{"README.md"},
		Category: "features", Target: "main", Tier: "M", Dependencies: []string{},
		Prompt: "SYNTHETIC-PRIVATE-PROMPT", Decision: decision, Reason: "Grounded reason",
		ProblemKey: "synthetic-" + id, RelevantPaths: []string{}, Reconsiders: []string{},
	}
}

func session(id, role, status string) model.Session {
	return model.Session{ID: id, Role: role, Route: config.NewRoute("fixture", "low"),
		Status: status, StartedAt: "2026-09-19T10:00:00Z", Summary: "SYNTHETIC-PRIVATE-TRANSCRIPT"}
}

func batch(entries ...map[string]any) map[string]any {
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		list = append(list, entry)
	}
	return map[string]any{"assessments": list}
}

// write authors state through the same store paths the Go service will use:
// a committed plan, admissions, an event, a PR observation and a reservation.
func write(path string) (map[string]any, error) {
	s, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	cfg.BranchPrefix = "tyk/"
	cfg.VerificationCommands = []string{"synthetic required check"}
	if err := s.Put("settings", "config", cfg); err != nil {
		return nil, err
	}
	if err := s.SaveControl(model.DefaultControl()); err != nil {
		return nil, err
	}
	accepted := proposal("p1", model.DecisionAccepted)
	deferred := proposal("p2", model.DecisionDeferred)
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: cycleID, Number: 1, Status: model.CycleCompleted,
		StartedAt: "2026-09-19T10:00:00Z", CompletedAt: ptr("2026-09-19T10:30:00Z"),
		Grounding: &model.Grounding{Revision: "base0000", PRs: []model.PullRequest{},
			ExternalPRs: []model.ExternalPrContext{}, History: []any{}, MaintenanceTargets: []string{}},
		Proposals: []model.Proposal{accepted, deferred},
		Assessments: []any{
			batch(map[string]any{"id": "p1", "decision": "accepted", "reason": "a accepts"},
				map[string]any{"id": "p2", "decision": "deferred", "reason": "a defers"}),
			batch(map[string]any{"id": "p1", "decision": "accepted", "reason": "b accepts"},
				map[string]any{"id": "p2", "decision": "rejected", "reason": "b rejects"}),
		},
		Sessions: []model.Session{
			session("adversary-a-session", model.ReviewerSlots()[0], model.SessionCompleted),
			session("adversary-b-session", model.ReviewerSlots()[1], model.SessionCompleted),
		},
		Repository:     "fixture/project",
		DecisionMemory: []any{map[string]any{"id": decisionID, "repository": "fixture/project", "title": "Synthetic p2", "decision": "deferred"}},
		// A previously discarded planning workspace keeps startup housekeeping
		// in either implementation from changing this record.
		Lifecycle: model.WorkspaceLifecycle{DiscardedAt: ptr("2026-09-19T10:31:00Z")},
	}
	reason := model.BlockedReasonVerificationFailed
	task := model.Task{
		ID: taskID, CycleID: cycleID, Proposal: accepted, Status: model.StatusBlocked,
		Route: config.NewRoute("fixture", "low"), Config: cfg,
		SourceRevision: "source00", ComparisonBase: "source00", DefaultRevision: "base0000",
		Branch: "tyk/synthetic-go", Workspace: "/synthetic-private-workspace",
		Sessions: []model.Session{session("exec-1", "executor", model.SessionCompleted)},
		Reviews: []model.ReviewRound{{SessionID: "review-1", Revision: "out00001", ComparisonBase: "source00",
			Result: model.Review{Completed: true, Summary: "SYNTHETIC-PRIVATE-REVIEW", Findings: []model.Finding{}}, CreatedAt: "2026-09-19T10:40:00Z"}},
		Verification: []model.Verification{{Command: "synthetic required check", Success: false,
			Output: "SYNTHETIC-PRIVATE-OUTPUT", Revision: "out00001", CreatedAt: "2026-09-19T10:41:00Z"}},
		OutputCommit: ptr("out00001"), Attempts: 1, Error: ptr("SYNTHETIC-PRIVATE-ERROR"),
		CreatedAt: "2026-09-19T10:31:00Z", UpdatedAt: "2026-09-19T10:42:00Z",
		BlockedReason: &reason, SupersededBy: []string{}, Supersedes: []string{},
	}
	if err := s.CommitPlan(cycle, []model.Task{task}); err != nil {
		return nil, err
	}
	planning := store.NewAdmission(cycleID, nil, "discovery", config.NewRoute("fixture", "low"))
	planning.At = "2026-09-19T10:00:00Z"
	if err := s.ReserveSession(0, planning); err != nil {
		return nil, err
	}
	execution := store.NewAdmission(cycleID, ptr(taskID), "executor", config.NewRoute("fixture", "low"))
	execution.At = "2026-09-19T10:31:00Z"
	if err := s.ReserveSession(0, execution); err != nil {
		return nil, err
	}
	if err := s.Event(taskID, "status", "Blocked after ghp_syntheticsecrettoken0000 was rejected"); err != nil {
		return nil, err
	}
	pr := model.PullRequest{Number: 7, Title: "Synthetic p1", Branch: "tyk/synthetic-go", Head: "out00001",
		Base: "main", URL: "https://github.com/fixture/project/pull/7", State: "open",
		CreatedAt: "2026-09-19T10:43:00Z", Owned: true}
	if err := s.RecordPrObservation("fixture/project", pr, true); err != nil {
		return nil, err
	}
	if err := s.Put("settings", "pr_inventory", model.OpenPrInventory{Repository: "fixture/project",
		ObservedAt: "2026-09-19T10:44:00Z", PRs: []model.PullRequest{pr}}); err != nil {
		return nil, err
	}
	if err := s.SeedPrReservation(task); err != nil {
		return nil, err
	}
	return describe(s)
}

// rehearsal extends an already-migrated database with the state an upgrade
// rehearsal needs: a committed second cycle plus two publication-checkpoint
// tasks that carry native session identities. One task is left mid-`publishing`
// — the durable shape a crash between output commit and PR confirmation leaves —
// so each service's startup recovery rewrites it to a blocked checkpoint. The
// other is already blocked with `publication_uncertain`, an output commit, a
// recorded PR observation and a live pr_reservations row. Both reservations
// survive because the release trigger only fires when no output commit exists.
func rehearsal(path string) (map[string]any, error) {
	s, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var cfg config.Config
	found, err := s.Get("settings", "config", &cfg)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("rehearsal requires a migrated database with saved config")
	}
	third := proposal("p3", model.DecisionAccepted)
	fourth := proposal("p4", model.DecisionAccepted)
	cycle := model.Cycle{
		Mode: model.CycleModeExecution, ID: rehearsalCycleID, Number: 2, Status: model.CycleCompleted,
		StartedAt: "2026-09-21T09:00:00Z", CompletedAt: ptr("2026-09-21T09:30:00Z"),
		Grounding: &model.Grounding{Revision: "base1111", PRs: []model.PullRequest{{
			Number: 7, Title: "Synthetic p4", Branch: "tyk/synthetic-uncertain", Head: "cccc0003",
			Base: "main", URL: "https://github.com/fixture/project/pull/7", State: "open",
			ChangedLines: 40, CreatedAt: "2026-09-21T08:00:00Z", Owned: true,
			HeadRepository: "fixture/project", BaseRepository: "fixture/project"}},
			ExternalPRs: []model.ExternalPrContext{{
				Number: 9, URL: "https://github.com/fixture/project/pull/9", Title: "External work",
				Body: "context", Branch: "ext-work", Head: "eeee0009", Base: "main",
				HeadRepository: "contributor/project", BaseRepository: "fixture/project"}},
			PRCoverage: model.PrCoverage{Complete: true, TotalOpen: 2, TotalExternal: 1, IncludedExternal: 1, MaxExternal: 5, MaxTitleChars: 200, MaxBodyChars: 2000, MaxContextBytes: 65536},
			History:    []any{}, MaintenanceTargets: []string{}},
		Proposals: []model.Proposal{third, fourth},
		Assessments: []any{
			batch(map[string]any{"id": "p3", "decision": "accepted", "reason": "a accepts"},
				map[string]any{"id": "p4", "decision": "accepted", "reason": "a accepts"}),
		},
		Sessions: []model.Session{
			session("codex-thread-discovery-2", "discovery", model.SessionCompleted),
			session("codex-thread-adversary-a2", model.ReviewerSlots()[0], model.SessionCompleted),
			session("codex-thread-adversary-b2", model.ReviewerSlots()[1], model.SessionCompleted),
		},
		Repository:     cfg.GitHubRepo,
		DecisionMemory: []any{map[string]any{"id": rehearsalDecisionID, "repository": cfg.GitHubRepo, "title": "Synthetic p4", "decision": "accepted"}},
		// A previously discarded planning workspace keeps startup housekeeping
		// in either implementation from changing this record.
		Lifecycle: model.WorkspaceLifecycle{DiscardedAt: ptr("2026-09-21T09:31:00Z")},
	}
	// The in-flight publication checkpoint: an output commit exists, the repair
	// thread was still open, and publication never confirmed a PR number. The
	// first service that opens this state recovers it to a blocked checkpoint —
	// deterministically in either implementation — while keeping the reservation.
	publishing := model.Task{
		ID: rehearsalPublishing, CycleID: rehearsalCycleID, Proposal: third,
		Status: model.StatusPublishing, Route: config.NewRoute("fixture", "medium"), Config: cfg,
		SourceRevision: "base1111", ComparisonBase: "base1111", DefaultRevision: "base1111",
		Branch: "tyk/synthetic-publishing", Workspace: "/synthetic-private-workspace-pub",
		ExecutionSession: ptr("codex-thread-exec-7"), RepairSession: ptr("codex-thread-repair-7"),
		Sessions: []model.Session{
			session("codex-thread-exec-7", "executor", model.SessionCompleted),
			session("codex-thread-repair-7", "repair", model.SessionRunning),
		},
		Reviews: []model.ReviewRound{{SessionID: "codex-thread-review-7a", Revision: "bbbb0002", ComparisonBase: "base1111",
			Result: model.Review{Completed: true, Summary: "SYNTHETIC-PRIVATE-REVIEW", Findings: []model.Finding{}}, CreatedAt: "2026-09-21T09:40:00Z"}},
		Verification: []model.Verification{{Command: "synthetic required check", Success: true,
			Output: "ok", Revision: "bbbb0002", CreatedAt: "2026-09-21T09:45:00Z"}},
		OutputCommit: ptr("bbbb0002"), Attempts: 1,
		CreatedAt: "2026-09-21T09:31:00Z", UpdatedAt: "2026-09-21T09:50:00Z",
		SupersededBy: []string{}, Supersedes: []string{},
	}
	// The operator-visible checkpoint: publication result uncertain after the
	// service observed its own PR record. It stays reconcile-eligible and keeps
	// its reservation through every service that opens the state.
	uncertain := model.Task{
		ID: rehearsalUncertainID, CycleID: rehearsalCycleID, Proposal: fourth,
		Status: model.StatusBlocked, Route: config.NewRoute("fixture", "medium"), Config: cfg,
		SourceRevision: "base1111", ComparisonBase: "base1111", DefaultRevision: "base1111",
		Branch: "tyk/synthetic-uncertain", Workspace: "/synthetic-private-workspace-unc",
		ExecutionSession: ptr("codex-thread-exec-9"),
		Sessions:         []model.Session{session("codex-thread-exec-9", "executor", model.SessionCompleted)},
		Reviews: []model.ReviewRound{
			{SessionID: "codex-thread-review-9a", Revision: "cccc0002", ComparisonBase: "base1111",
				Result: model.Review{Completed: true, Summary: "round one", Findings: []model.Finding{}}, CreatedAt: "2026-09-21T09:35:00Z"},
			{SessionID: "codex-thread-review-9b", Revision: "cccc0003", ComparisonBase: "base1111",
				Result: model.Review{Completed: true, Summary: "round two", Findings: []model.Finding{}}, CreatedAt: "2026-09-21T09:55:00Z"}},
		Verification: []model.Verification{{Command: "synthetic required check", Success: true,
			Output: "ok", Revision: "cccc0003", CreatedAt: "2026-09-21T09:58:00Z"}},
		OutputCommit: ptr("cccc0003"), PRNumber: ptr(uint64(7)),
		PRURL: ptr("https://github.com/fixture/project/pull/7"), Attempts: 1,
		Error: ptr("Publication result is uncertain; reconcile the preserved output commit"),
		AttemptPolicy: &model.AttemptPolicy{MaxRepairRounds: 4, MaxNoProgressRounds: 2,
			MaxRetries: 2, TaskTimeoutSeconds: 14400, SessionTimeoutSeconds: 1800, CommandTimeoutSeconds: 600},
		ReviewBaseline: 1, BlockedReason: ptr(model.BlockedReasonPublicationUncertain),
		CreatedAt: "2026-09-21T09:31:00Z", UpdatedAt: "2026-09-21T10:00:00Z",
		SupersededBy: []string{}, Supersedes: []string{},
	}
	if err := s.CommitPlan(cycle, []model.Task{publishing, uncertain}); err != nil {
		return nil, err
	}
	planning := store.NewAdmission(rehearsalCycleID, nil, "discovery", config.NewRoute("fixture", "low"))
	planning.At = "2026-09-21T09:00:00Z"
	if err := s.ReserveSession(0, planning); err != nil {
		return nil, err
	}
	execution := store.NewAdmission(rehearsalCycleID, ptr(rehearsalPublishing), "executor", config.NewRoute("fixture", "medium"))
	execution.At = "2026-09-21T09:31:00Z"
	if err := s.ReserveSession(0, execution); err != nil {
		return nil, err
	}
	if err := s.Event(rehearsalUncertainID, "status", "Blocked: publication result uncertain after PR observation"); err != nil {
		return nil, err
	}
	pr := model.PullRequest{Number: 7, Title: "Synthetic p4", Branch: "tyk/synthetic-uncertain", Head: "cccc0003",
		Base: "main", URL: "https://github.com/fixture/project/pull/7", State: "open",
		ChangedLines: 40, CreatedAt: "2026-09-21T09:59:00Z", Owned: true,
		HeadRepository: "fixture/project", BaseRepository: "fixture/project"}
	if err := s.RecordPrObservation(cfg.GitHubRepo, pr, true); err != nil {
		return nil, err
	}
	if err := s.Put("settings", "pr_inventory", model.OpenPrInventory{Repository: cfg.GitHubRepo,
		ObservedAt: "2026-09-21T10:01:00Z", PRs: []model.PullRequest{pr}}); err != nil {
		return nil, err
	}
	if err := s.SeedPrReservation(publishing); err != nil {
		return nil, err
	}
	if err := s.SeedPrReservation(uncertain); err != nil {
		return nil, err
	}
	summary, err := describe(s)
	if err != nil {
		return nil, err
	}
	summary["cycle_id"] = rehearsalCycleID
	summary["publishing_task_id"] = rehearsalPublishing
	summary["uncertain_task_id"] = rehearsalUncertainID
	summary["decision_id"] = rehearsalDecisionID
	// The write-mode identity keys describe records this mode never created.
	delete(summary, "task_id")
	return summary, nil
}

// reopen exercises the migration path and the indexed views on a database
// another implementation wrote, without changing any record.
func reopen(path string) (map[string]any, error) {
	s, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return describe(s)
}

func describe(s *store.Store) (map[string]any, error) {
	dashboard, err := s.Dashboard()
	if err != nil {
		return nil, err
	}
	tasks, err := s.HistoryPage("task", store.HistoryQuery{})
	if err != nil {
		return nil, err
	}
	cycles, err := s.HistoryPage("cycle", store.HistoryQuery{})
	if err != nil {
		return nil, err
	}
	reservations, err := s.PrReservations("fixture/project")
	if err != nil {
		return nil, err
	}
	capacity, err := s.PlanningCapacity()
	if err != nil {
		return nil, err
	}
	ids := func(items []json.RawMessage) []string {
		out := []string{}
		for _, item := range items {
			var summary struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(item, &summary); err == nil {
				out = append(out, summary.ID)
			}
		}
		return out
	}
	return map[string]any{
		"cycle_id":         cycleID,
		"task_id":          taskID,
		"decision_id":      decisionID,
		"tasks":            ids(tasks.Items),
		"cycles":           ids(cycles.Items),
		"counts":           dashboard.Counts,
		"attention_tasks":  ids(dashboard.AttentionTasks),
		"events":           len(dashboard.Events),
		"reservations":     len(reservations),
		"sessions_today":   dashboard.SessionsToday,
		"capacity_used":    capacity.Used,
		"schema_supported": store.SupportedSchemaVersion,
	}, nil
}
