// The read-only RunEvidenceV1 export: what a cycle reports, what never leaves the state, and the one redaction pass every export passes through.

package export

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
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
			ExternalPRs:        []model.ExternalPRContext{},
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

func reviewerSession(role string, status model.SessionStatus) model.Session {
	return model.Session{
		ID: role + "-session", Role: role, Route: config.NewRoute("fixture", "low"),
		Status: status, StartedAt: "2026-09-12T00:10:00Z", Summary: "SECRET-TRANSCRIPT",
	}
}

func savedBatch(entries ...model.Assessment) model.AssessmentDocument {
	return model.AssessmentDocument{Assessments: append([]model.Assessment{}, entries...)}
}

func savedEntry(id, decision, reason string) model.Assessment {
	return model.Assessment{ID: id, Decision: decision, Reason: reason}
}

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
			Status: model.SessionCompleted, StartedAt: model.Now(), Summary: "SECRET-TRANSCRIPT",
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

func exported(t *testing.T, s *store.Store, cycleID string) map[string]any {
	t.Helper()
	value, err := RunEvidence(s, cycleID)
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

func containsText(items []any, needle string) bool {
	for _, item := range items {
		if s, ok := item.(string); ok && strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

func TestCompleteCycleExport(t *testing.T) {
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
			savedBatch(savedEntry("p1", "accepted", "a accepts"), savedEntry("p2", "deferred", "a defers")),
			savedBatch(savedEntry("p1", "accepted", "b accepts"), savedEntry("p2", "rejected", "b rejects")),
		},
		[]model.Session{reviewerSession("adversary-a", model.SessionCompleted), reviewerSession("adversary-b", model.SessionCompleted)})
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{delivered})
	value := exported(t, s, "cycle-a")

	if number(value["schema_version"]) != int64(SchemaVersion) || value["review_required_before_sharing"] != true || value["kind"] != "recorded_review_check_evidence" {
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

func TestAssembleTaskMatching(t *testing.T) {
	namedTask := func(id, cycleID, proposalID string) model.Task {
		task := task(cycleID, proposalID)
		task.ID = id
		return task
	}
	const missingTask = "The proposal was accepted but no task is linked in this cycle; acceptance is not execution."
	const multipleTasks = "2 tasks match this proposal in this cycle; every match is preserved and none is selected."
	const rejectedTask = "The proposal is recorded as rejected yet 1 task(s) are linked; the saved records are inconsistent."
	for _, tc := range []struct {
		name         string
		mode         string
		proposals    []model.Proposal
		tasks        []model.Task
		linked       [][]string
		proposalGaps [][]string
		runGaps      []string
	}{
		{name: "empty cycle", mode: "execution"},
		{
			name: "accepted without execution", mode: "execution",
			proposals: []model.Proposal{proposal("p1", "accepted")},
			linked:    [][]string{{}}, proposalGaps: [][]string{{missingTask}},
		},
		{
			name: "audit acceptance queues nothing", mode: "audit",
			proposals: []model.Proposal{proposal("p1", "accepted")},
			linked:    [][]string{{}}, proposalGaps: [][]string{{}},
		},
		{
			name: "every match in input order", mode: "execution",
			proposals: []model.Proposal{proposal("p1", "accepted")},
			tasks: []model.Task{
				namedTask("task-z", "cycle-a", "p1"),
				namedTask("task-a", "cycle-a", "p1"),
			},
			linked: [][]string{{"task-z", "task-a"}}, proposalGaps: [][]string{{multipleTasks}},
		},
		{
			name: "foreign tasks excluded before unmatched count", mode: "execution",
			proposals: []model.Proposal{proposal("p1", "accepted")},
			tasks: []model.Task{
				namedTask("foreign-match", "cycle-b", "p1"),
				namedTask("unmatched", "cycle-a", "unknown"),
				namedTask("matched", "cycle-a", "p1"),
				namedTask("foreign-unmatched", "cycle-b", "unknown"),
			},
			linked: [][]string{{"matched"}}, proposalGaps: [][]string{{}},
			runGaps: []string{
				"2 saved task records name a different cycle and are excluded from this run.",
				"1 task records in this cycle have no matching saved proposal identity.",
			},
		},
		{
			name: "tasks without proposals", mode: "execution",
			tasks:   []model.Task{namedTask("unmatched", "cycle-a", "unknown")},
			runGaps: []string{"1 task records in this cycle have no matching saved proposal identity."},
		},
		{
			name: "linked rejected proposal", mode: "execution",
			proposals: []model.Proposal{proposal("p1", "rejected")},
			tasks:     []model.Task{namedTask("matched", "cycle-a", "p1")},
			linked:    [][]string{{"matched"}}, proposalGaps: [][]string{{rejectedTask}},
		},
		{
			name: "duplicate proposal identities remain distinct", mode: "execution",
			proposals: []model.Proposal{
				proposal("p1", "accepted"), proposal("p2", "deferred"), proposal("p1", "rejected"),
			},
			tasks:  []model.Task{namedTask("matched", "cycle-a", "p1")},
			linked: [][]string{{"matched"}, {}, {"matched"}}, proposalGaps: [][]string{{}, {}, {rejectedTask}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := assemble(cycle("cycle-a", tc.mode, tc.proposals, nil, nil), tc.tasks)
			if result.Proposals == nil || len(result.Proposals) != len(tc.proposals) {
				t.Fatalf("proposals = %+v; want %d recorded proposals", result.Proposals, len(tc.proposals))
			}
			for i, p := range result.Proposals {
				if p.ID != tc.proposals[i].ID || p.FinalDecision != tc.proposals[i].Decision {
					t.Fatalf("proposal %d identity or decision changed: %+v", i, p)
				}
				if p.Evidence == nil || p.ReviewerVerdicts == nil || p.LinkedTasks == nil || p.Gaps == nil {
					t.Fatalf("proposal %d contains a null evidence array: %+v", i, p)
				}
				var ids []string
				for _, task := range p.LinkedTasks {
					ids = append(ids, task.ID)
				}
				if !slices.Equal(ids, tc.linked[i]) || !slices.Equal(p.Gaps, tc.proposalGaps[i]) {
					t.Fatalf("proposal %d links/gaps = %v/%v; want %v/%v", i, ids, p.Gaps, tc.linked[i], tc.proposalGaps[i])
				}
			}
			wantGaps := slices.Concat([]string{
				"Reviewer adversary-a has neither a completed session nor a saved assessment batch.",
				"Reviewer adversary-b has neither a completed session nor a saved assessment batch.",
			}, tc.runGaps)
			if !slices.Equal(result.Gaps, wantGaps) {
				t.Fatalf("run gaps = %v; want %v", result.Gaps, wantGaps)
			}
		})
	}
}

func TestMalformedSavedAssessmentBatches(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"assessments":null}`} {
		for slot := range model.ReviewerSlots() {
			t.Run(fmt.Sprintf("%s/slot-%d", raw, slot), func(t *testing.T) {
				c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")},
					[]any{savedBatch(savedEntry("p1", "accepted", "a accepts")), savedBatch(savedEntry("p1", "accepted", "b accepts"))},
					[]model.Session{reviewerSession("adversary-a", model.SessionCompleted), reviewerSession("adversary-b", model.SessionCompleted)})
				c.Assessments[slot] = json.RawMessage(raw)
				s, path := fixture(t, []model.Cycle{c}, []model.Task{task(c.ID, "p1")})
				readOnly, err := Run(path, c.ID)
				must(t, err)
				for name, value := range map[string]map[string]any{"run export": readOnly, "evidence API": exported(t, s, c.ID)} {
					t.Run(name, func(t *testing.T) {
						p := findProposal(t, value, "p1")
						if get(p, "reviewer_verdicts", slot, "state") != "malformed" || get(p, "reviewer_verdicts", slot, "reviewer") != model.ReviewerSlots()[slot] {
							t.Fatalf("malformed reviewer slot was lost: %v", p["reviewer_verdicts"])
						}
						if get(p, "reviewer_verdicts", 1-slot, "state") != "recorded" || get(p, "reviewer_verdicts", 1-slot, "decision") != "accepted" || len(list(p, "linked_tasks")) != 1 {
							t.Fatalf("valid evidence was lost or reassigned: %v", p)
						}
						if !containsText(list(value, "gaps"), fmt.Sprintf("Saved assessment batch %d (%s) is malformed", slot, model.ReviewerSlots()[slot])) {
							t.Fatalf("malformed batch gap missing: %v", value["gaps"])
						}
					})
				}
			})
		}
	}
}

func TestPrivateFieldsOmitted(t *testing.T) {
	tk := task("cycle-a", "p1")
	output := "out00001"
	tk.OutputCommit = &output
	tk.Reviews = []model.ReviewRound{review("out00001", true, "SECRET-REVIEW-SUMMARY",
		model.Finding{Title: "Finding title", File: "internal/x/x.go", Detail: "Finding rationale", Priority: "high"})}
	tk.Verification = []model.Verification{check("make check", true, "out00001")}
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{tk})
	value := exported(t, s, "cycle-a")
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

func TestLatestReviewGoverns(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	output := "out00001"
	regressed := task("cycle-a", "p1")
	regressed.OutputCommit = &output
	regressed.Reviews = []model.ReviewRound{
		review("out00001", true, "clean earlier round"),
		review("out00001", true, "later round found a problem", model.Finding{Title: "t", File: "f", Detail: "d", Priority: "high"}),
	}
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{regressed})
	value := exported(t, s, "cycle-a")
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
		value := exported(t, s, "cycle-a")
		latest := get(findProposal(t, value, "p1"), "linked_tasks", 0, "latest_review").(map[string]any)
		if latest["clean"] != false || get(latest, "latest", "completed") != round.completed || get(latest, "latest", "summary_present") != (strings.TrimSpace(round.summary) != "") {
			t.Fatalf("%v %q: %v", round.completed, round.summary, latest)
		}
	}
}

func TestNoConfiguredChecks(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	tk := task("cycle-a", "p1")
	tk.Config.VerificationCommands = []string{}
	output := "out00001"
	tk.OutputCommit = &output
	s, _ := fixture(t, []model.Cycle{c}, []model.Task{tk})
	value := exported(t, s, "cycle-a")
	commands := get(findProposal(t, value, "p1"), "linked_tasks", 0, "required_commands").(map[string]any)
	if commands["state"] != "not_configured" || len(list(commands, "commands")) != 0 || commands["all_passed_at_output_revision"] != false {
		t.Fatalf("%v", commands)
	}
}

func TestRunExport(t *testing.T) {
	c := cycle("cycle-a", "execution", []model.Proposal{proposal("p1", "accepted")}, nil, nil)
	s, path := fixture(t, []model.Cycle{c}, []model.Task{task("cycle-a", "p1")})
	must(t, s.Close())
	value, err := Run(path, "cycle-a")
	must(t, err)
	if get(value, "cycle", "id") != "cycle-a" || len(list(findProposal(t, value, "p1"), "linked_tasks")) != 1 {
		t.Fatalf("%v", value)
	}
	if _, err := Run(path, "cycle-b"); err == nil || !strings.Contains(err.Error(), "No saved cycle cycle-b") {
		t.Fatalf("missing cycle = %v; want an explicit error", err)
	}
	if _, err := Run(filepath.Join(t.TempDir(), "missing.db"), "cycle-a"); err == nil {
		t.Fatal("a missing state database was exported")
	}
}

func TestRedactedValue(t *testing.T) {
	t.Parallel()
	whitespace := "\t\n\v\f\r \u0085                 　"
	for _, separator := range whitespace {
		t.Run(fmt.Sprintf("U+%04X", separator), func(t *testing.T) {
			input := "before bEaReR" + string(separator) + "\t" + "synthetic-private-credential after"
			value, err := redacted(map[string]any{"nested": []any{input}, "count": 7})
			must(t, err)
			if got := text(t, value); got != `{"count":7,"nested":["before [redacted] after"]}` {
				t.Fatalf("redacted export = %s", got)
			}
		})
	}
}

func TestRunEvidenceIncludesSavedMergeEvidence(t *testing.T) {
	delivered := task("cycle-a", "p1")
	delivered.Status = model.StatusPublished
	delivered.Config.DeliveryMode = config.DeliveryModeMaintenance
	output := "out00001"
	delivered.OutputCommit = &output
	number3 := uint64(3)
	delivered.PRNumber = &number3
	url := "https://github.com/fixture/project/pull/3"
	delivered.PRURL = &url
	delivered.Reviews = []model.ReviewRound{review("out00001", true, "Reviewed the complete change set")}
	delivered.Verification = []model.Verification{check("make check", true, "out00001")}
	c := cycle("cycle-a", "execution",
		[]model.Proposal{proposal("p1", "accepted")},
		[]any{savedBatch(savedEntry("p1", "accepted", "a accepts")), savedBatch(savedEntry("p1", "accepted", "b accepts"))},
		[]model.Session{reviewerSession("adversary-a", model.SessionCompleted), reviewerSession("adversary-b", model.SessionCompleted)})
	s, dbPath := fixture(t, []model.Cycle{c}, []model.Task{delivered})

	merge := &model.AutoMergeState{
		TaskID: delivered.ID, Head: "out00001", ComparisonBase: "source00",
		HeadBranch: delivered.Branch, BaseBranch: delivered.Config.DefaultBranch,
		PolicyRevision: "policy", Authorized: true, Status: model.AutoMergeMerged,
		Reason: "Squash merged by Octomus", ObservedAt: model.Now(),
		ResultSource: new("confirmed"), MergeCommit: new("merge01"),
	}
	observation := model.PRObservation{
		Repository: "fixture/project", ObservedAt: model.Now(), DeliveredHead: &output,
		PR: model.PullRequest{Number: 3, URL: url, State: "merged", Owned: true,
			Head: "out00001", Branch: delivered.Branch, Base: "main",
			HeadRepository: "fixture/project", BaseRepository: "fixture/project"},
		AutoMerge: merge,
	}
	must(t, s.Put("pr", "fixture/project:3", observation))
	value := exported(t, s, "cycle-a")
	linked := get(findProposal(t, value, "p1"), "linked_tasks", 0).(map[string]any)
	evidence := linked["auto_merge"].(map[string]any)
	if evidence["status"] != "merged" || evidence["result_source"] != "confirmed" ||
		evidence["merge_commit"] != "merge01" || evidence["head"] != "out00001" ||
		evidence["head_branch"] != delivered.Branch || evidence["base_branch"] != "main" {
		t.Fatalf("exported merge evidence = %v", evidence)
	}
	full, err := Run(dbPath, "cycle-a")
	if err != nil {
		t.Fatal(err)
	}
	if get(findProposal(t, full, "p1"), "linked_tasks", 0, "auto_merge", "status") != "merged" {
		t.Fatal("the read-only run export lost the saved merge evidence")
	}

	for name, mutate := range map[string]func(*model.AutoMergeState){
		"wrong-task":   func(m *model.AutoMergeState) { m.TaskID = "other-task" },
		"wrong-head":   func(m *model.AutoMergeState) { m.Head = "other000" },
		"wrong-base":   func(m *model.AutoMergeState) { m.ComparisonBase = "otherbase" },
		"wrong-branch": func(m *model.AutoMergeState) { m.HeadBranch = "octomus/other" },
		"unbound":      func(m *model.AutoMergeState) { m.HeadBranch = ""; m.BaseBranch = "" },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := merge.Clone()
			mutate(&mutated)
			record := observation
			record.AutoMerge = &mutated
			must(t, s.Put("pr", "fixture/project:3", record))
			value := exported(t, s, "cycle-a")
			linked := get(findProposal(t, value, "p1"), "linked_tasks", 0).(map[string]any)
			if linked["auto_merge"] != nil {
				t.Fatalf("%s leaked mismatched evidence: %v", name, linked["auto_merge"])
			}
		})
	}
	must(t, s.Put("pr", "fixture/project:3", observation))
	value = exported(t, s, "cycle-a")
	if get(findProposal(t, value, "p1"), "linked_tasks", 0, "auto_merge", "status") != "merged" {
		t.Fatal("the exact-bound record did not restore the evidence")
	}
}
