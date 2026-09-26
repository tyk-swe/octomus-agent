package engine

// A planning pass is all-or-nothing, so a validation failure fails every
// admission it used. Each failure names the offending proposal and the
// conflicting proposal, task, request or value, while keeping the wording that
// operators, dashboards and end-to-end checks already match on.

import (
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// assertErrorNames fails unless err is non-nil and contains every fragment.
func assertErrorNames(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted; want an error naming %q", fragments)
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not contain %q", err, fragment)
		}
	}
}

func TestProposalValidationNamesTheOffendingProposal(t *testing.T) {
	cfg := testConfig(t.TempDir())
	grounding := model.Grounding{Revision: "source", PRs: []model.PullRequest{ownedPR("octomus/existing")}}
	onMain := func(id string) model.Proposal { return proposal(id, cfg.DefaultBranch) }
	onPR := func(id string, dependencies ...string) model.Proposal {
		p := proposal(id, "octomus/existing")
		p.Dependencies = append([]string{}, dependencies...)
		return p
	}
	with := func(p model.Proposal, change func(*model.Proposal)) model.Proposal {
		change(&p)
		return p
	}
	published := queuedTask(cfg, "old-task", cfg.DefaultBranch, "octomus/old-task")
	published.Status = model.StatusPublished
	published.Proposal = onMain("a")

	for _, tc := range []struct {
		name      string
		limit     uint64
		proposals []model.Proposal
		history   []model.Task
		fragments []string
	}{
		{name: "duplicate identity", proposals: []model.Proposal{onMain("a"), with(onMain("a"), func(p *model.Proposal) { p.Decision = model.DecisionRejected })},
			fragments: []string{`Duplicate proposal identity "a"`}},
		{name: "invalid decision", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Decision = "maybe" })},
			fragments: []string{"decision and rationale", `proposal "a" has invalid decision "maybe"`}},
		{name: "no rationale", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Reason = " " })},
			fragments: []string{"decision and rationale", `proposal "a" has no rationale`}},
		{name: "same accepted work", proposals: []model.Proposal{onMain("a"), with(onMain("b"), func(p *model.Proposal) { p.ProblemKey = "problem-a" })},
			fragments: []string{"Duplicate accepted", `"b" repeats the work of "a"`}},
		{name: "size limit", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Title = strings.Repeat("t", 201) })},
			fragments: []string{`Proposal "a" exceeds task size limits`, "title 201 bytes (limit 200)"}},
		{name: "missing context", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Benefit = "" })},
			fragments: []string{"missing grounding or execution context", `proposal "a" has no benefit`}},
		{name: "disabled category", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Category = "astrology" })},
			fragments: []string{`Unknown tier or disabled category for proposal "a" (tier "M", category "astrology")`}},
		{name: "ineligible target", proposals: []model.Proposal{proposal("a", "someone-elses-branch")},
			fragments: []string{`Proposal "a" target "someone-elses-branch"`, "owned open PR"}},
		{name: "recorded work", proposals: []model.Proposal{onMain("a")}, history: []model.Task{published},
			fragments: []string{`Proposal "a" duplicates recorded work (task old-task, published)`}},
		{name: "task limit", limit: 1, proposals: []model.Proposal{onMain("a"), onMain("b")},
			fragments: []string{"Accepted task limit exceeded: 2 accepted (limit 1)"}},
		{name: "dependency cycle", proposals: []model.Proposal{onPR("first", "second"), onPR("second", "first")},
			fragments: []string{`cycle detected through proposal "first"`}},
		{name: "unknown dependency", proposals: []model.Proposal{onPR("unknown", "missing")},
			fragments: []string{"Dependency must be an accepted proposal", `proposal "unknown" depends on "missing"`}},
		{name: "default-branch dependency", proposals: []model.Proposal{onPR("first"), with(onMain("dependent"), func(p *model.Proposal) { p.Dependencies = []string{"first"} })},
			fragments: []string{"default-branch", `proposal "dependent" (target "main") depends on "first" (target "octomus/existing")`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limited := cfg.Clone()
			if tc.limit != 0 {
				limited.MaxTasksPerCycle = tc.limit
			}
			assertErrorNames(t, ValidateProposals(limited, tc.proposals, grounding, tc.history), tc.fragments...)
		})
	}
	// Faults are reported in plan order, whatever the map iteration order.
	first := ValidateProposals(cfg, []model.Proposal{onPR("x", "gone"), onPR("y", "lost")}, grounding, nil)
	for i := 0; i < 20; i++ {
		again := ValidateProposals(cfg, []model.Proposal{onPR("x", "gone"), onPR("y", "lost")}, grounding, nil)
		if again == nil || first == nil || again.Error() != first.Error() {
			t.Fatalf("validation reported %v, then %v", first, again)
		}
	}
	assertErrorNames(t, first, `proposal "x" depends on "gone"`)
}

func TestReviewerAssessmentsNameTheOffendingProposal(t *testing.T) {
	candidates := []model.Proposal{{ID: "a"}, {ID: "b"}}
	assessed := func(id, decision, reason string) assessment {
		return assessment{ID: id, Decision: decision, Reason: reason}
	}
	for _, tc := range []struct {
		name        string
		assessments []assessment
		fragment    string
	}{
		{name: "invented", assessments: []assessment{assessed("a", "accepted", "ok"), assessed("b", "rejected", "no"), assessed("c", "accepted", "ok")}, fragment: `adversary-a invented proposal "c"`},
		{name: "assessed twice", assessments: []assessment{assessed("a", "accepted", "ok"), assessed("a", "rejected", "no")}, fragment: `adversary-a assessed proposal "a" more than once`},
		{name: "invalid decision", assessments: []assessment{assessed("a", "maybe", "ok")}, fragment: `adversary-a gave proposal "a" an invalid decision "maybe"`},
		{name: "no rationale", assessments: []assessment{assessed("a", "deferred", " ")}, fragment: `adversary-a gave proposal "a" no rationale`},
		{name: "omitted", assessments: []assessment{assessed("a", "accepted", "ok")}, fragment: `adversary-a omitted proposal "b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertErrorNames(t, checkAssessments("adversary-a", candidates, tc.assessments), "Adversarial reviewer", tc.fragment)
		})
	}
	complete := []assessment{assessed("b", "deferred", "later"), assessed("a", "accepted", "ok")}
	if err := checkAssessments("adversary-b", candidates, complete); err != nil {
		t.Fatalf("complete assessment rejected: %v", err)
	}
}

func TestConsolidationNamesTheOffendingProposal(t *testing.T) {
	candidates := []model.Proposal{{ID: "a"}, {ID: "b"}}
	for _, tc := range []struct {
		name     string
		returned []model.Proposal
		fragment string
	}{
		{name: "invented", returned: []model.Proposal{{ID: "a"}, {ID: "b"}, {ID: "c"}}, fragment: `invented "c"`},
		{name: "returned twice", returned: []model.Proposal{{ID: "a"}, {ID: "a"}, {ID: "b"}}, fragment: `returned "a" twice`},
		{name: "omitted", returned: []model.Proposal{{ID: "a"}}, fragment: `omitted "b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertErrorNames(t, checkConsolidation(candidates, tc.returned), "Orchestrator omitted or invented proposal IDs", tc.fragment)
		})
	}
	if err := checkConsolidation(candidates, []model.Proposal{{ID: "b"}, {ID: "a"}}); err != nil {
		t.Fatalf("complete consolidation rejected: %v", err)
	}
	assertErrorNames(t, checkConsolidation([]model.Proposal{{ID: "a"}, {ID: "a"}}, nil), `duplicate proposal IDs: "a"`)
}

func TestDecisionMemoryAndRediscoveryNameTheOffendingProposal(t *testing.T) {
	cfg := testConfig(t.TempDir())
	request := map[string]any{"kind": "rediscovery", "id": "request-1", "target": cfg.DefaultBranch}
	reconsidering := func(target string, requests ...string) model.Proposal {
		p := proposal("a", target)
		p.Reconsiders = requests
		return p
	}
	assertErrorNames(t, ValidateDecisionMemory([]model.Proposal{reconsidering(cfg.DefaultBranch, "request-2")}, []any{request}),
		"does not match a pending request", `proposal "a" reconsiders "request-2", which is not pending`)
	assertErrorNames(t, ValidateDecisionMemory([]model.Proposal{reconsidering("octomus/existing", "request-1")}, []any{request}),
		"does not match a pending request", `proposal "a" targets "octomus/existing" but request "request-1" targets "main"`)

	accepted := proposal("a", cfg.DefaultBranch)
	recorded := recordToMap(decisionRecord{
		Kind: "decision", ID: "cycle-1:a", CycleMode: model.CycleModeExecution,
		Repository: cfg.GitHubRepo, Target: cfg.DefaultBranch, ProblemKey: accepted.ProblemIdentity(),
		Decision: model.DecisionRejected, Reason: "Current decision", SourceRevision: "revision",
		ContextFingerprint: "revision", ReconsiderAfter: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), CycleID: "cycle-1",
	})
	assertErrorNames(t, ValidateDecisionMemory([]model.Proposal{accepted}, []any{recorded}),
		"repeats a current recorded decision", `(proposal "a", decision "cycle-1:a")`)

	requests := []map[string]any{request}
	assertErrorNames(t, checkRediscoveryDecisions(requests, []model.Proposal{proposal("a", cfg.DefaultBranch)}),
		"exactly one fresh decision (request request-1 had 0)")
	twice := []model.Proposal{reconsidering(cfg.DefaultBranch, "request-1"), reconsidering(cfg.DefaultBranch, "request-1")}
	assertErrorNames(t, checkRediscoveryDecisions(requests, twice), "exactly one fresh decision (request request-1 had 2)")
	if err := checkRediscoveryDecisions(requests, twice[:1]); err != nil {
		t.Fatalf("one decision per request rejected: %v", err)
	}
}
