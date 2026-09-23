// Run-evidence read model. Every database here is an explicitly synthetic
// temporary fixture; no live state, credentials or runner accounts are used.
package evidence_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/evidence"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func cycle(id, mode string, proposals []model.Proposal, assessments []any, sessions []model.Session) model.Cycle {
	completed := "2026-09-12T01:00:00Z"
	cycleMode := model.CycleModeExecution
	if mode == "audit" {
		cycleMode = model.CycleModeAudit
	}
	if proposals == nil {
		proposals = []model.Proposal{}
	}
	if assessments == nil {
		assessments = []any{}
	}
	if sessions == nil {
		sessions = []model.Session{}
	}
	return model.Cycle{
		Mode:        cycleMode,
		ID:          id,
		Number:      7,
		Status:      model.CycleCompleted,
		StartedAt:   "2026-09-12T00:00:00Z",
		CompletedAt: &completed,
		Grounding: &model.Grounding{
			Revision:           "base0000",
			PRs:                []model.PullRequest{},
			ExternalPRs:        []model.ExternalPrContext{},
			History:            []any{},
			MaintenanceDue:     false,
			MaintenanceTargets: []string{},
		},
		Proposals:   proposals,
		Assessments: assessments,
		Sessions:    sessions,
		Repository:  "fixture/project",
	}
}

func proposal(id, decision string) model.Proposal {
	return model.Proposal{
		ID: id, Title: "Concrete improvement", Problem: "Missing behavior",
		Benefit: "Useful behavior", Scope: "one file", Evidence: []string{"README.md"},
		Category: "features", Target: "main", Tier: "M", Dependencies: []string{},
		Prompt: "SECRET-PROMPT-TEXT", Decision: decision, Reason: "Grounded reason",
		RelevantPaths: []string{}, Reconsiders: []string{},
	}
}

func reviewerSession(role, status string) model.Session {
	return model.Session{
		ID: role + "-session", Role: role, Route: config.NewRoute("fixture", "low"),
		Status: status, StartedAt: "2026-09-12T00:10:00Z", Summary: "SECRET-TRANSCRIPT",
	}
}

func batch(entries ...map[string]any) map[string]any {
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		list = append(list, entry)
	}
	return map[string]any{"assessments": list}
}

func entry(id, decision, reason string) map[string]any {
	return map[string]any{"id": id, "decision": decision, "reason": reason}
}

// task carries a private workspace path, prompt, transcript, command output
// and diagnostic text, so omission of private fields is observable.
func task(cycleID, proposalID string) model.Task {
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	cfg.CodexBinary = "/private/bin/codex"
	cfg.VerificationCommands = []string{"make check"}
	errorText := "SECRET-ERROR-DETAIL"
	return model.Task{
		ID: model.ID(), CycleID: cycleID, Proposal: proposal(proposalID, "accepted"),
		Status: model.StatusQueued, Route: config.NewRoute("fixture", "low"), Config: cfg,
		SourceRevision: "source00", ComparisonBase: "source00", DefaultRevision: "base0000",
		Branch: "octomus/work", Workspace: "/private/workspace/path",
		Sessions: []model.Session{{
			ID: "exec-1", Role: "executor", Route: config.NewRoute("fixture", "low"),
			Status: "completed", StartedAt: model.Now(), Summary: "SECRET-TRANSCRIPT",
		}},
		Reviews: []model.ReviewRound{}, Verification: []model.Verification{},
		Error:     &errorText,
		CreatedAt: model.Now(), UpdatedAt: model.Now(),
		SupersededBy: []string{}, Supersedes: []string{},
	}
}

func review(revision string, completed bool, summary string, findings ...model.Finding) model.ReviewRound {
	if findings == nil {
		findings = []model.Finding{}
	}
	return model.ReviewRound{
		SessionID: "review-1", Revision: revision, ComparisonBase: "source00",
		CreatedAt: model.Now(),
		Result:    model.Review{Completed: completed, Summary: summary, Findings: findings},
	}
}

func check(command string, success bool, revision string) model.Verification {
	return model.Verification{
		Command: command, Success: success, Output: "SECRET-COMMAND-OUTPUT",
		Revision: revision, CreatedAt: model.Now(),
	}
}

func fixture(t *testing.T, cycles []model.Cycle, tasks []model.Task) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, c := range cycles {
		if err := s.Put("cycle", c.ID, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range tasks {
		if err := s.Put("task", task.ID, task); err != nil {
			t.Fatal(err)
		}
	}
	return s, path
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func export(t *testing.T, s *store.Store, cycleID string) map[string]any {
	t.Helper()
	value, err := evidence.RunEvidence(s, cycleID)
	must(t, err)
	return value
}

func text(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	must(t, err)
	return string(data)
}

func get(value any, path ...any) any {
	for _, key := range path {
		switch typed := value.(type) {
		case map[string]any:
			value = typed[key.(string)]
		case []any:
			index := key.(int)
			if index < 0 || index >= len(typed) {
				return nil
			}
			value = typed[index]
		default:
			return nil
		}
	}
	return value
}

func list(value any, path ...any) []any {
	items, _ := get(value, path...).([]any)
	return items
}

func number(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		n, _ := typed.Int64()
		return n
	case float64:
		return int64(typed)
	case uint32:
		return int64(typed)
	case int64:
		return typed
	}
	return -1
}

func findProposal(t *testing.T, value map[string]any, id string) map[string]any {
	t.Helper()
	for _, item := range list(value, "proposals") {
		if p, ok := item.(map[string]any); ok && p["id"] == id {
			return p
		}
	}
	t.Fatalf("proposal %s missing from export", id)
	return nil
}

func joined(items []any) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.(string))
	}
	return strings.Join(parts, " | ")
}

func containsText(items []any, needle string) bool {
	for _, item := range items {
		if s, ok := item.(string); ok && strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func TestCompleteCycleReportsReviewersTasksRevisionsAndChecks(t *testing.T) {
	delivered := task("cycle-a", "p1")
	delivered.Status = model.StatusPublished
	output := "out00001"
	delivered.OutputCommit = &output
	number3 := uint64(3)
	delivered.PRNumber = &number3
	url := "https://github.com/fixture/project/pull/3"
	delivered.PRURL = &url
	delivered.Reviews = []model.ReviewRound{review("out00001", true, "Reviewed the complete change set")}
	delivered.Verification = []model.Verification{check("make check", true, "out00001")}
	c := cycle("cycle-a", "execution",
		[]model.Proposal{proposal("p1", "accepted"), proposal("p2", "deferred")},
		[]any{
			batch(entry("p1", "accepted", "a accepts"), entry("p2", "deferred", "a defers")),
			batch(entry("p1", "accepted", "b accepts"), entry("p2", "rejected", "b rejects")),
		},
		[]model.Session{reviewerSession("adversary-a", "completed"), reviewerSession("adversary-b", "completed")})
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{delivered})
	value := export(t, s, "cycle-a")

	if number(value["schema_version"]) != int64(evidence.SchemaVersion) || value["review_required_before_sharing"] != true || value["kind"] != "recorded_review_check_evidence" {
		t.Fatalf("%v", value)
	}
	if generated, _ := value["generated_at"].(string); len(generated) <= 10 {
		t.Fatalf("generated_at %q", generated)
	}
	planning := get(value, "cycle", "planning").(map[string]any)
	if planning["creates_execution_queue"] != true || planning["planning_finished"] != true {
		t.Fatalf("%v", planning)
	}
	if number(get(planning, "decisions", "accepted")) != 1 || number(get(planning, "decisions", "deferred")) != 1 {
		t.Fatalf("%v", planning["decisions"])
	}
	if get(value, "cycle", "grounding_revision") != "base0000" {
		t.Fatalf("%v", value["cycle"])
	}

	p1 := findProposal(t, value, "p1")
	verdicts := list(p1, "reviewer_verdicts")
	if get(verdicts, 0, "reviewer") != "adversary-a" || get(verdicts, 0, "state") != "recorded" || get(verdicts, 0, "decision") != "accepted" {
		t.Fatalf("%v", verdicts)
	}
	if get(verdicts, 1, "reviewer") != "adversary-b" || get(verdicts, 1, "reason") != "b accepts" {
		t.Fatalf("%v", verdicts)
	}

	// Deferred stays deferred and is never folded into rejected.
	p2 := findProposal(t, value, "p2")
	if p2["final_decision"] != "deferred" || get(p2, "reviewer_verdicts", 0, "decision") != "deferred" || get(p2, "reviewer_verdicts", 1, "decision") != "rejected" || len(list(p2, "linked_tasks")) != 0 {
		t.Fatalf("%v", p2)
	}

	linked := get(p1, "linked_tasks", 0).(map[string]any)
	if linked["id"] != delivered.ID || linked["proposal_id"] != "p1" || linked["cycle_id"] != "cycle-a" || linked["status"] != "published" {
		t.Fatalf("%v", linked)
	}
	if get(linked, "revisions", "output") != "out00001" || get(linked, "revisions", "comparison_base") != "source00" {
		t.Fatalf("%v", linked["revisions"])
	}
	if get(linked, "latest_review", "clean") != true || get(linked, "latest_review", "clean_at_output_revision") != true || get(linked, "latest_review", "latest", "summary_present") != true {
		t.Fatalf("%v", linked["latest_review"])
	}
	commands := linked["required_commands"].(map[string]any)
	if commands["state"] != "recorded" || get(commands, "commands", 0, "state") != "passed" || commands["all_passed_at_output_revision"] != true {
		t.Fatalf("%v", commands)
	}
	if number(get(linked, "pull_request", "number")) != 3 || get(linked, "pull_request", "source") != "recorded_task_reference" {
		t.Fatalf("%v", linked["pull_request"])
	}
	if get(linked, "sessions", 0, "role") != "executor" || get(linked, "sessions", 0, "requested_route", "model") != "fixture" {
		t.Fatalf("%v", linked["sessions"])
	}
	if gaps := list(linked, "gaps"); len(gaps) != 0 {
		t.Fatalf("%v", gaps)
	}
	if !containsText(list(value, "limitations"), "Published is not merged") {
		t.Fatalf("%v", value["limitations"])
	}
}

func TestPrivateFieldsAreOmittedFromTheExport(t *testing.T) {
	tk := task("cycle-a", "p1")
	output := "out00001"
	tk.OutputCommit = &output
	tk.Reviews = []model.ReviewRound{review("out00001", true, "SECRET-REVIEW-SUMMARY",
		model.Finding{Title: "Finding title", File: "internal/x/x.go", Detail: "Finding rationale", Priority: "high"})}
	tk.Verification = []model.Verification{check("make check", true, "out00001")}
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{tk})
	value := export(t, s, "cycle-a")
	rendered := text(t, value)
	for _, private := range []string{
		"/private/workspace/path",
		"/private/bin/codex",
		"SECRET-PROMPT-TEXT",
		"SECRET-TRANSCRIPT",
		"SECRET-COMMAND-OUTPUT",
		"SECRET-ERROR-DETAIL",
		"SECRET-REVIEW-SUMMARY",
	} {
		if strings.Contains(rendered, private) {
			t.Fatalf("export leaked %s", private)
		}
	}
	// The facts about those records survive without the private text.
	linked := get(findProposal(t, value, "p1"), "linked_tasks", 0).(map[string]any)
	if linked["error_recorded"] != true || get(linked, "latest_review", "latest", "summary_present") != true {
		t.Fatalf("%v", linked)
	}
	finding := get(linked, "latest_review", "latest", "findings", 0).(map[string]any)
	if finding["title"] != "Finding title" || finding["file"] != "internal/x/x.go" || finding["priority"] != "high" {
		t.Fatalf("%v", finding)
	}
	if !strings.Contains(rendered, "requires manual review before sharing") || strings.Contains(rendered, "safe to publish") {
		t.Fatal("review requirement text changed")
	}
}

func TestPartialCycleReportsGapsWithoutInventingOutcomes(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	c.Status = model.CycleRunning
	c.CompletedAt = nil
	c.Grounding = nil
	s, _ := fixture(t, []model.Cycle{c}, nil)
	value := export(t, s, "cycle-a")
	if get(value, "cycle", "planning", "planning_finished") != false || get(value, "cycle", "grounding_revision") != nil {
		t.Fatalf("%v", value["cycle"])
	}
	gaps := joined(list(value, "gaps"))
	if !strings.Contains(gaps, "still recorded as running") || !strings.Contains(gaps, "no saved grounding revision") {
		t.Fatal(gaps)
	}
	// Both reviewers are explicitly missing rather than inferred from the decision.
	p1 := findProposal(t, value, "p1")
	for slot := 0; slot < 2; slot++ {
		if get(p1, "reviewer_verdicts", slot, "state") != "missing" || get(p1, "reviewer_verdicts", slot, "decision") != nil {
			t.Fatalf("%v", p1["reviewer_verdicts"])
		}
	}
	// Accepted planning without a task is a gap, never a claim of completed work.
	if !containsText(list(p1, "gaps"), "acceptance is not execution") {
		t.Fatalf("%v", p1["gaps"])
	}
}

func TestMalformedAndMissingReviewerBatchesNeverShiftIdentities(t *testing.T) {
	// Reviewer A's batch is unusable; reviewer B's is valid and must stay B's.
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")},
		[]any{
			map[string]any{"unexpected": "not an assessment list"},
			batch(entry("p1", "rejected", "b rejects"), entry("", "accepted", "unreadable identity")),
		},
		[]model.Session{reviewerSession("adversary-a", "completed"), reviewerSession("adversary-b", "completed")})
	s, _ := fixture(t, []model.Cycle{c}, nil)
	value := export(t, s, "cycle-a")
	verdicts := list(findProposal(t, value, "p1"), "reviewer_verdicts")
	if get(verdicts, 0, "reviewer") != "adversary-a" || get(verdicts, 0, "state") != "malformed" || get(verdicts, 0, "decision") != nil {
		t.Fatalf("%v", verdicts)
	}
	if get(verdicts, 1, "reviewer") != "adversary-b" || get(verdicts, 1, "state") != "recorded" || get(verdicts, 1, "decision") != "rejected" {
		t.Fatalf("%v", verdicts)
	}
	gaps := joined(list(value, "gaps"))
	for _, needle := range []string{"is malformed", "not reassigned", "unreadable entries"} {
		if !strings.Contains(gaps, needle) {
			t.Fatal(gaps)
		}
	}
}

func TestUnconfirmedAndDuplicateReviewerEvidenceStaysExplicit(t *testing.T) {
	// A recorded but failed reviewer session cannot confirm the batch's identity.
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")},
		[]any{batch(entry("p1", "accepted", "first"), entry("p1", "rejected", "second"))},
		[]model.Session{reviewerSession("adversary-a", "failed")})
	s, _ := fixture(t, []model.Cycle{c}, nil)
	value := export(t, s, "cycle-a")
	verdicts := list(findProposal(t, value, "p1"), "reviewer_verdicts")
	if get(verdicts, 0, "state") != "duplicate" || get(verdicts, 0, "decision") != nil {
		t.Fatalf("%v", verdicts)
	}
	note, _ := get(verdicts, 0, "note").(string)
	if !strings.Contains(note, "inconsistent") || !strings.Contains(note, "unconfirmed") {
		t.Fatal(note)
	}
	if get(verdicts, 1, "state") != "missing" {
		t.Fatalf("%v", verdicts)
	}
}

func TestRepeatedProposalIDsAcrossCyclesDoNotCrossRuns(t *testing.T) {
	first := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	second := cycle("cycle-b", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	a := task("cycle-a", "p1")
	b := task("cycle-b", "p1")
	s, _ := fixture(t, []model.Cycle{first, second}, []model.Task{a, b})
	for id, expected := range map[string]model.Task{"cycle-a": a, "cycle-b": b} {
		value := export(t, s, id)
		linked := list(findProposal(t, value, "p1"), "linked_tasks")
		if len(linked) != 1 {
			t.Fatalf("%s joined across cycles", id)
		}
		if get(linked, 0, "id") != expected.ID || get(linked, 0, "cycle_id") != id {
			t.Fatalf("%v", linked)
		}
	}
}

func TestMultipleTaskMatchesArePreservedAndNoneIsSelected(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	newest := task("cycle-a", "p1")
	newest.UpdatedAt = "2099-01-01T00:00:00Z"
	newest.Status = model.StatusPublished
	older := task("cycle-a", "p1")
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{newest, older})
	value := export(t, s, "cycle-a")
	p1 := findProposal(t, value, "p1")
	linked := list(p1, "linked_tasks")
	if len(linked) != 2 {
		t.Fatalf("%v", linked)
	}
	ids := []string{get(linked, 0, "id").(string), get(linked, 1, "id").(string)}
	sort.Strings(ids)
	expected := []string{newest.ID, older.ID}
	sort.Strings(expected)
	if ids[0] != expected[0] || ids[1] != expected[1] {
		t.Fatalf("%v", ids)
	}
	if !containsText(list(p1, "gaps"), "2 tasks match") {
		t.Fatalf("%v", p1["gaps"])
	}
}

func TestAuditAcceptanceWithoutTasksIsNotExecution(t *testing.T) {
	c := cycle("cycle-a", "audit", []model.Proposal{proposal("p1", "accepted")},
		[]any{batch(entry("p1", "accepted", "a accepts"))},
		[]model.Session{reviewerSession("adversary-a", "completed")})
	s, _ := fixture(t, []model.Cycle{c}, nil)
	value := export(t, s, "cycle-a")
	if get(value, "cycle", "mode") != "audit" || get(value, "cycle", "planning", "creates_execution_queue") != false {
		t.Fatalf("%v", value["cycle"])
	}
	p1 := findProposal(t, value, "p1")
	if p1["final_decision"] != "accepted" || len(list(p1, "linked_tasks")) != 0 {
		t.Fatalf("%v", p1)
	}
	// An audit recommendation without a task is expected, not a gap.
	if containsText(list(p1, "gaps"), "acceptance is not execution") {
		t.Fatalf("%v", p1["gaps"])
	}
	if !containsText(list(value, "limitations"), "Audit acceptance") {
		t.Fatalf("%v", value["limitations"])
	}
}

func TestLatestReviewGovernsAndIncompleteOrEmptySummariesAreNotClean(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	output := "out00001"
	// A clean round followed by a later unclean one: the latest saved review governs.
	regressed := task("cycle-a", "p1")
	regressed.OutputCommit = &output
	regressed.Reviews = []model.ReviewRound{
		review("out00001", true, "clean earlier round"),
		review("out00001", true, "later round found a problem", model.Finding{Title: "t", File: "f", Detail: "d", Priority: "high"}),
	}
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{regressed})
	value := export(t, s, "cycle-a")
	linked := get(findProposal(t, value, "p1"), "linked_tasks", 0).(map[string]any)
	latest := linked["latest_review"].(map[string]any)
	if number(latest["rounds_recorded"]) != 2 || latest["clean"] != false || latest["clean_at_output_revision"] != false || len(list(latest, "latest", "findings")) != 1 {
		t.Fatalf("%v", latest)
	}

	for _, round := range []struct {
		completed bool
		summary   string
	}{{false, "interrupted"}, {true, "   "}} {
		tk := task("cycle-a", "p1")
		tk.OutputCommit = &output
		tk.Reviews = []model.ReviewRound{review("out00001", round.completed, round.summary)}
		s, _ := fixture(t, []model.Cycle{c}, []model.Task{tk})
		value := export(t, s, "cycle-a")
		latest := get(findProposal(t, value, "p1"), "linked_tasks", 0, "latest_review").(map[string]any)
		if latest["clean"] != false || get(latest, "latest", "completed") != round.completed || get(latest, "latest", "summary_present") != (strings.TrimSpace(round.summary) != "") {
			t.Fatalf("%v %q: %v", round.completed, round.summary, latest)
		}
	}
}

func TestLaterFailuresMissingResultsAndMismatchedRevisionsAreNotPassing(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	tk := task("cycle-a", "p1")
	output := "out00001"
	tk.OutputCommit = &output
	tk.Config.VerificationCommands = []string{"make check", "make test", "make never-run"}
	tk.Reviews = []model.ReviewRound{review("out00001", true, "clean review")}
	tk.Verification = []model.Verification{
		// A newer failure invalidates the older pass.
		check("make check", true, "out00001"),
		check("make check", false, "out00001"),
		// A pass recorded against a different revision does not transfer.
		check("make test", true, "stale000"),
	}
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{tk})
	value := export(t, s, "cycle-a")
	linked := get(findProposal(t, value, "p1"), "linked_tasks", 0).(map[string]any)
	commands := linked["required_commands"].(map[string]any)
	byName := func(name string) map[string]any {
		for _, item := range list(commands, "commands") {
			if c := item.(map[string]any); c["command"] == name {
				return c
			}
		}
		t.Fatalf("%s missing", name)
		return nil
	}
	checkResult := byName("make check")
	if checkResult["state"] != "failed" || number(checkResult["results_recorded"]) != 2 || checkResult["latest_success"] != false {
		t.Fatalf("%v", checkResult)
	}
	testResult := byName("make test")
	if testResult["state"] != "passed_at_other_revision" || testResult["matches_output_revision"] != false || testResult["latest_revision"] != "stale000" {
		t.Fatalf("%v", testResult)
	}
	missing := byName("make never-run")
	if missing["state"] != "no_result" || missing["latest_success"] != nil {
		t.Fatalf("%v", missing)
	}
	if commands["all_passed_at_output_revision"] != false {
		t.Fatalf("%v", commands)
	}
	if !containsText(list(linked, "gaps"), "every required command") {
		t.Fatalf("%v", linked["gaps"])
	}
}

func TestBlockedReasonUsesTheTaskAPIVocabulary(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	tk := task("cycle-a", "p1")
	tk.Status = model.StatusBlocked
	reason := model.BlockedReasonVerificationFailed
	tk.BlockedReason = &reason
	encoded, err := json.Marshal(tk)
	must(t, err)
	var saved map[string]any
	must(t, json.Unmarshal(encoded, &saved))
	if saved["blocked_reason"] != "verification_failed" {
		t.Fatalf("%v", saved["blocked_reason"])
	}
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{tk})
	value := export(t, s, "cycle-a")
	exported := get(findProposal(t, value, "p1"), "linked_tasks", 0).(map[string]any)
	// One saved reason, one spelling: the export must match what GET /api/tasks/{id} says.
	if exported["status"] != "blocked" || exported["blocked_reason"] != saved["blocked_reason"] {
		t.Fatalf("%v", exported)
	}
}

func TestNoConfiguredChecksIsNotConfiguredRatherThanPassing(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	tk := task("cycle-a", "p1")
	tk.Config.VerificationCommands = []string{}
	output := "out00001"
	tk.OutputCommit = &output
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{tk})
	value := export(t, s, "cycle-a")
	commands := get(findProposal(t, value, "p1"), "linked_tasks", 0, "required_commands").(map[string]any)
	if commands["state"] != "not_configured" || len(list(commands, "commands")) != 0 || commands["all_passed_at_output_revision"] != false {
		t.Fatalf("%v", commands)
	}
}

func TestStoreAndCLIFactsAgreeApartFromGenerationMetadata(t *testing.T) {
	tk := task("cycle-a", "p1")
	output := "out00001"
	tk.OutputCommit = &output
	tk.Reviews = []model.ReviewRound{review("out00001", true, "clean review")}
	tk.Verification = []model.Verification{check("make check", true, "out00001")}
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")},
		[]any{batch(entry("p1", "accepted", "a accepts"))},
		[]model.Session{reviewerSession("adversary-a", "completed")})
	s, path := fixture(t, []model.Cycle{c}, []model.Task{tk})
	fromStore := export(t, s, "cycle-a")
	fromCLI, err := evidence.ExportRun(path, "cycle-a")
	must(t, err)
	for _, value := range []map[string]any{fromStore, fromCLI} {
		if _, ok := value["generated_at"].(string); !ok {
			t.Fatalf("%v", value["generated_at"])
		}
		delete(value, "generated_at")
	}
	if text(t, fromStore) != text(t, fromCLI) {
		t.Fatalf("store:\n%s\ncli:\n%s", text(t, fromStore), text(t, fromCLI))
	}
}

func listing(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func recordCount(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	must(t, err)
	defer db.Close()
	var n int64
	must(t, db.QueryRow("SELECT count(*) FROM records").Scan(&n))
	return n
}

func TestCLIExportIsReadOnlyAndErrorsExplicitly(t *testing.T) {
	tk := task("cycle-a", "p1")
	output := "out00001"
	tk.OutputCommit = &output
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	s, path := fixture(t, []model.Cycle{c}, []model.Task{tk})
	dir := filepath.Dir(path)
	// The service checkpoints its WAL on close; the export must see the same
	// bytes and files while the service is still open, exactly as the CLI
	// would while a service runs against the directory.
	beforeFiles := listing(t, dir)
	beforeBytes, err := os.ReadFile(path)
	must(t, err)
	beforeRecords := recordCount(t, path)

	value, err := evidence.ExportRun(path, "cycle-a")
	must(t, err)
	if get(value, "cycle", "id") != "cycle-a" {
		t.Fatalf("%v", value["cycle"])
	}

	// Unknown cycles and unknown state are explicit errors, not empty successful exports.
	if _, err := evidence.ExportRun(path, "cycle-missing"); err == nil || !strings.Contains(err.Error(), "No saved cycle cycle-missing") {
		t.Fatalf("%v", err)
	}
	absent := filepath.Join(dir, "missing-dir", "state.db")
	if _, err := evidence.ExportRun(absent, "cycle-a"); err == nil {
		t.Fatal("missing state exported")
	}
	if _, err := os.Stat(filepath.Dir(absent)); !os.IsNotExist(err) {
		t.Fatal("export created the parent directory")
	}

	afterBytes, err := os.ReadFile(path)
	must(t, err)
	if string(afterBytes) != string(beforeBytes) {
		t.Fatal("export changed the database file")
	}
	if after := listing(t, dir); strings.Join(after, ",") != strings.Join(beforeFiles, ",") {
		t.Fatalf("export changed the directory: %v -> %v", beforeFiles, after)
	}
	if recordCount(t, path) != beforeRecords {
		t.Fatal("export changed the record count")
	}
	must(t, s.Close())
}

func TestCommittedPlanAttributesVerdictsToReviewerSlots(t *testing.T) {
	// Records written through CommitPlan — the same path the engine uses — must
	// attribute each saved assessment batch to the reviewer slot session labels.
	slots := model.ReviewerSlots()
	committed := task("cycle-slots", "p1")
	output := "out00001"
	committed.OutputCommit = &output
	committed.Reviews = []model.ReviewRound{review("out00001", true, "Reviewed the complete change set")}
	committed.Verification = []model.Verification{check("make check", true, "out00001")}
	c := cycle("cycle-slots", "execution",
		[]model.Proposal{proposal("p1", "accepted"), proposal("p2", "deferred")},
		[]any{
			batch(entry("p1", "accepted", "a accepts"), entry("p2", "deferred", "a defers")),
			batch(entry("p1", "accepted", "b accepts"), entry("p2", "rejected", "b rejects")),
		},
		[]model.Session{reviewerSession(slots[0], "completed"), reviewerSession(slots[1], "completed")})
	s, _ := fixture(t, nil, nil)
	must(t, s.CommitPlan(c, []model.Task{committed}))

	value := export(t, s, "cycle-slots")
	p1 := findProposal(t, value, "p1")
	verdicts := list(p1, "reviewer_verdicts")
	if len(verdicts) != len(slots) {
		t.Fatalf("%v", verdicts)
	}
	for index, slot := range slots {
		if get(verdicts, index, "reviewer") != slot || get(verdicts, index, "state") != "recorded" || get(verdicts, index, "decision") != "accepted" {
			t.Fatalf("%v", verdicts)
		}
	}
	if get(verdicts, 0, "reason") != "a accepts" || get(verdicts, 1, "reason") != "b accepts" {
		t.Fatalf("%v", verdicts)
	}

	linked := list(p1, "linked_tasks")
	if len(linked) != 1 {
		t.Fatalf("%v", linked)
	}
	tk := linked[0].(map[string]any)
	if tk["id"] != committed.ID || tk["cycle_id"] != "cycle-slots" || tk["proposal_id"] != "p1" {
		t.Fatalf("%v", tk)
	}
	if get(tk, "latest_review", "latest", "revision") != "out00001" || get(tk, "latest_review", "clean") != true || get(tk, "latest_review", "clean_at_output_revision") != true {
		t.Fatalf("%v", tk["latest_review"])
	}
	if get(tk, "required_commands", "state") != "recorded" || get(tk, "required_commands", "commands", 0, "state") != "passed" || get(tk, "required_commands", "commands", 0, "latest_revision") != "out00001" {
		t.Fatalf("%v", tk["required_commands"])
	}

	p2 := findProposal(t, value, "p2")
	if get(p2, "reviewer_verdicts", 0, "decision") != "deferred" || get(p2, "reviewer_verdicts", 1, "decision") != "rejected" || len(list(p2, "linked_tasks")) != 0 {
		t.Fatalf("%v", p2)
	}
}

// The public showcase wrapper is the only artifact derived from this package
// that ships to readers, so every limitation and the review requirement must
// survive verbatim.
func TestPublicShowcaseWrapperKeepsEveryLimitationVerbatim(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "showcase", "synthetic.public.json"))
	must(t, err)
	wrapper := string(data)
	if !strings.Contains(wrapper, evidence.ReviewRequirement) {
		t.Fatal("The showcase wrapper drops the review requirement")
	}
	for _, limitation := range evidence.Limitations {
		if !strings.Contains(wrapper, limitation) {
			t.Fatalf("The showcase wrapper drops a limitation: %s", limitation)
		}
	}
}
