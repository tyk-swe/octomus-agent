package engine

// Plan rules: proposal validation, consolidation accounting, the dispatch-time task plan check and target
// resolution.

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

func TestValidateProposals(t *testing.T) {
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
		fragments []string // empty: the plan is valid
	}{
		{name: "same-PR chain", proposals: []model.Proposal{onPR("first"), onPR("second", "first")}},
		{name: "rejected unowned target is retained", proposals: []model.Proposal{with(proposal("rejected", "someone-elses-branch"), func(p *model.Proposal) { p.Decision = model.DecisionRejected })}},
		{name: "distinct problem keys", proposals: []model.Proposal{onMain("a"), with(onMain("b"), func(p *model.Proposal) { p.ProblemKey = "parser:length-header" })}},
		{name: "duplicate identity", proposals: []model.Proposal{onMain("a"), with(onMain("a"), func(p *model.Proposal) { p.Decision = model.DecisionRejected })},
			fragments: []string{`Duplicate proposal identity "a"`}},
		{name: "invalid decision", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Decision = "maybe" })},
			fragments: []string{`proposal "a" has invalid decision "maybe"`}},
		{name: "no rationale", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Reason = " " })},
			fragments: []string{`proposal "a" has no rationale`}},
		{name: "same accepted work", proposals: []model.Proposal{onMain("a"), with(onMain("b"), func(p *model.Proposal) { p.ProblemKey = "problem-a" })},
			fragments: []string{"Duplicate accepted", `"b" repeats the work of "a"`}},
		{name: "size limit", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Title = strings.Repeat("t", 201) })},
			fragments: []string{`Proposal "a" exceeds task size limits`}},
		{name: "missing context", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Benefit = "" })},
			fragments: []string{`proposal "a" has no benefit`}},
		{name: "disabled category", proposals: []model.Proposal{with(onMain("a"), func(p *model.Proposal) { p.Category = "astrology" })},
			fragments: []string{`Unknown tier or disabled category for proposal "a"`}},
		{name: "ineligible target", proposals: []model.Proposal{proposal("a", "someone-elses-branch")},
			fragments: []string{`Proposal "a" target "someone-elses-branch"`, "owned open PR"}},
		{name: "recorded work", proposals: []model.Proposal{onMain("a")}, history: []model.Task{published},
			fragments: []string{`Proposal "a" duplicates recorded work (task old-task, published)`}},
		{name: "task limit", limit: 1, proposals: []model.Proposal{onMain("a"), onMain("b")},
			fragments: []string{"Accepted task limit exceeded: 2 accepted (limit 1)"}},
		{name: "dependency cycle", proposals: []model.Proposal{onPR("first", "second"), onPR("second", "first")},
			fragments: []string{`Dependency cycle through proposal "first"`}},
		{name: "unknown dependency", proposals: []model.Proposal{onPR("unknown", "missing")},
			fragments: []string{`proposal "unknown" depends on "missing"`, "not an accepted proposal"}},
		{name: "default-branch dependency", proposals: []model.Proposal{onPR("first"), with(onMain("dependent"), func(p *model.Proposal) { p.Dependencies = []string{"first"} })},
			fragments: []string{"Default-branch work cannot depend on another proposal", `proposal "dependent" depends on "first"`}},
		{name: "cross-target dependency", proposals: []model.Proposal{onMain("first"), onPR("second", "first")},
			fragments: []string{`proposal "second" (target "octomus/existing") depends on "first" (target "main")`}},
		{name: "forked writers", proposals: []model.Proposal{onPR("first"), onPR("second", "first"), onPR("fork", "first")},
			fragments: []string{"complete dependency order"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limited := cfg.Clone()
			if tc.limit != 0 {
				limited.MaxTasksPerCycle = tc.limit
			}
			err := validateProposals(limited, tc.proposals, grounding, tc.history)
			if len(tc.fragments) == 0 {
				if err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				return
			}
			assertErrorNames(t, err, tc.fragments...)
		})
	}
	first := validateProposals(cfg, []model.Proposal{onPR("x", "gone"), onPR("y", "lost")}, grounding, nil)
	for i := 0; i < 20; i++ {
		again := validateProposals(cfg, []model.Proposal{onPR("x", "gone"), onPR("y", "lost")}, grounding, nil)
		if again == nil || first == nil || again.Error() != first.Error() {
			t.Fatalf("validation reported %v, then %v", first, again)
		}
	}
	assertErrorNames(t, first, `proposal "x" depends on "gone"`)
}

func TestCheckConsolidation(t *testing.T) {
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

func TestDiscoveryProposalLimit(t *testing.T) {
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

func TestValidateTaskPlan(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	const existing = "octomus/existing"
	task := func(id, target string, dependencies ...string) model.Task {
		task := queuedTask(cfg, id, target, target)
		task.Proposal.Dependencies = append([]string{}, dependencies...)
		return task
	}
	for _, tc := range []struct {
		name  string
		tasks []model.Task
		want  string
	}{
		{name: "empty plan", tasks: nil},
		{name: "independent default-branch tasks", tasks: []model.Task{task("a", "main"), task("b", "main")}},
		{name: "linear chain on one PR", tasks: []model.Task{task("a", existing), task("b", existing, "a"), task("c", existing, "b")}},
		{name: "chain saved out of order", tasks: []model.Task{task("c", existing, "b"), task("b", existing, "a"), task("a", existing)}},
		{name: "writers to different PRs", tasks: []model.Task{task("a", existing), task("b", "octomus/other")}},
		{name: "duplicate identity", tasks: []model.Task{task("a", existing), task("a", existing)}, want: "Duplicate task identity a"},
		{name: "unknown dependency", tasks: []model.Task{task("b", existing, "missing")}, want: `task "b" depends on "missing", which is not an accepted task in this plan`},
		{name: "default-branch dependency", tasks: []model.Task{task("a", "main"), task("b", "main", "a")}, want: `Default-branch work cannot depend on another task; consolidate or defer it until the prerequisite PR has merged: task "b" depends on "a"`},
		{name: "cross-target dependency", tasks: []model.Task{task("a", "octomus/other"), task("b", existing, "a")}, want: `Dependent tasks must write the same existing pull request: task "b" (target "octomus/existing") depends on "a" (target "octomus/other")`},
		{name: "two-task cycle", tasks: []model.Task{task("a", existing, "b"), task("b", existing, "a")}, want: `Dependency cycle through task "a"`},
		{name: "self dependency", tasks: []model.Task{task("a", existing, "a")}, want: `Dependency cycle through task "a"`},
		{name: "unordered writers", tasks: []model.Task{task("a", existing), task("b", existing)}, want: "Accepted tasks on octomus/existing need a complete dependency order; unordered or forked branch plans cannot execute"},
		{name: "forked writers", tasks: []model.Task{task("a", existing), task("b", existing, "a"), task("c", existing, "a")}, want: "Accepted tasks on octomus/existing need a complete dependency order; unordered or forked branch plans cannot execute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTaskPlan(tc.tasks)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validateTaskPlan = %v; want %q", err, tc.want)
			}
		})
	}
}

func TestResolveTarget(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	pr := func(state string, owned bool, baseRepo string) model.PullRequest {
		return model.PullRequest{
			Number: 7, Title: "PR", Branch: "octomus/fix", Head: strings.Repeat("a", 40), Base: "main",
			URL: "https://example.invalid/pull/7", State: state, ChangedLines: 1, CreatedAt: "2026-08-01T00:00:00Z",
			Owned: owned, HeadRepository: "fixture/project", BaseRepository: baseRepo,
		}
	}
	open := []model.PullRequest{pr("open", true, "fixture/project")}
	bound, err := resolveTarget(cfg, open, "octomus/fix")
	if err != nil || bound == nil || bound.Number != 7 {
		t.Fatalf("owned open PR = %+v, %v; want PR 7", bound, err)
	}
	release := pr("open", true, "fixture/project")
	release.Base = "release"
	for name, prs := range map[string][]model.PullRequest{
		"external":      {pr("open", false, "fixture/project")},
		"closed":        {pr("closed", true, "fixture/project")},
		"merged":        {pr("merged", true, "fixture/project")},
		"foreign base":  {pr("open", true, "upstream/project")},
		"non-main base": {release},
	} {
		if target, err := resolveTarget(cfg, prs, "octomus/fix"); err == nil {
			t.Errorf("%s PR resolved as a target: %+v", name, target)
		}
	}
	if target, err := resolveTarget(cfg, open, "main"); err != nil || target != nil {
		t.Fatalf("default branch resolved to %+v, %v; want no PR", target, err)
	}
}
