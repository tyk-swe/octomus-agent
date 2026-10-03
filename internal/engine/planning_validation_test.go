package engine

import (
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

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
	t.Parallel()
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
	first := ValidateProposals(cfg, []model.Proposal{onPR("x", "gone"), onPR("y", "lost")}, grounding, nil)
	for i := 0; i < 20; i++ {
		again := ValidateProposals(cfg, []model.Proposal{onPR("x", "gone"), onPR("y", "lost")}, grounding, nil)
		if again == nil || first == nil || again.Error() != first.Error() {
			t.Fatalf("validation reported %v, then %v", first, again)
		}
	}
	assertErrorNames(t, first, `proposal "x" depends on "gone"`)
}

func TestConsolidationNamesTheOffendingProposal(t *testing.T) {
	t.Parallel()
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
}

func TestDiscoveryProposalLimitSharesTheRemainingCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		seeded int
		agents uint64
		want   int
	}{
		{seeded: 0, agents: 9, want: 11}, {seeded: 0, agents: 10, want: 10}, {seeded: 10, agents: 9, want: 10},
		{seeded: 92, agents: 9, want: 0}, {seeded: 100, agents: 8, want: 0}, {seeded: 150, agents: 8, want: 0},
		{seeded: 0, agents: 0, want: 0}, {seeded: 0, agents: ^uint64(0), want: 0},
	} {
		if got := discoveryProposalLimit(tc.seeded, tc.agents); got != tc.want {
			t.Fatalf("discoveryProposalLimit(%d, %d) = %d; want %d", tc.seeded, tc.agents, got, tc.want)
		}
	}
}
