package engine

import (
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func promptTask() (*model.Task, config.Config) {
	task := &model.Task{
		SourceRevision: "source-sha",
		ComparisonBase: "base-sha",
		PRURL:          stringPointer("https://github.com/fixture/project/pull/7"),
		Proposal: model.Proposal{
			Prompt:   "Fix the parser.\nKeep the API.",
			Problem:  "Truncated frames",
			Benefit:  "Reliable parsing",
			Scope:    "parser.go",
			Evidence: []string{"parser.go:12", "tab\there"},
		},
	}
	cfg := config.Config{VerificationCommands: []string{"go test ./...", `echo "quoted"`}}
	return task, cfg
}

func TestExecutorPromptIsByteStable(t *testing.T) {
	t.Parallel()
	task, cfg := promptTask()
	want := "Implement this accepted task end to end in this workspace. Source revision: source-sha. Full comparison base: base-sha. " +
		`Existing PR: Some("https://github.com/fixture/project/pull/7"). ` +
		"Preserve existing accumulated branch behavior; inspect its full diff. Do not push, publish, merge or deploy. " +
		`Required repository verification commands: ["go test ./...", "echo \"quoted\""]. ` +
		"Objective and constraints:\nFix the parser.\nKeep the API.\nProblem: Truncated frames\nBenefit: Reliable parsing\nScope: parser.go\n" +
		`Evidence: ["parser.go:12", "tab\there"]` +
		"\nReturn a concise summary of actual changes, verification and material risks or migration notes."
	if got := executorPrompt(task, cfg); got != want {
		t.Fatalf("executor prompt changed:\n got %q\nwant %q", got, want)
	}
	task.PRURL = nil
	if got := executorPrompt(task, cfg); !strings.Contains(got, "Existing PR: None. ") {
		t.Fatalf("a task without a PR must say None: %q", got)
	}
}

func TestReviewPromptIsByteStable(t *testing.T) {
	t.Parallel()
	task, _ := promptTask()
	want := "Perform a fresh code review equivalent to /review of the COMPLETE change set: git diff base-sha HEAD. Recorded HEAD: reviewed-sha. " +
		"Include all accumulated PR changes and all repairs; do not only review the last commit. " +
		"Task: Fix the parser.\nKeep the API.. Scope: parser.go. " +
		`Existing PR: Some("https://github.com/fixture/project/pull/7"). ` +
		"Inspect code and evidence, do not modify files. Report actionable correctness, regression, design or missing verification findings with file, priority and technical rationale. " +
		"Do not invent findings. Set completed=true only after completing the review. A clean review must have an explanatory summary and zero findings."
	trusted := changeSet{stat: " parser.go | 2 +-", diff: "diff --git a/parser.go b/parser.go\n-old\n+new"}
	want += "\nThe orchestrator's own git computed this change set. Git inside your sandbox reads configuration and shell startup files earlier turns could change, " +
		"so wherever its output differs from what follows, what follows is authoritative and the difference is itself a finding.\n" +
		"Changed files (git diff --stat base-sha reviewed-sha):\n parser.go | 2 +-\nComplete diff:\ndiff --git a/parser.go b/parser.go\n-old\n+new"
	if got := reviewPrompt(task, "reviewed-sha", trusted); got != want {
		t.Fatalf("review prompt changed:\n got %q\nwant %q", got, want)
	}
	trusted.diff, trusted.omitted = "", "it exceeds 262144 bytes"
	if got := reviewPrompt(task, "reviewed-sha", trusted); !strings.HasSuffix(got, "Changed files (git diff --stat base-sha reviewed-sha):\n parser.go | 2 +-\n"+
		"The complete diff is not included because it exceeds 262144 bytes: read it with git diff base-sha HEAD and check it against the files above.") {
		t.Fatalf("review prompt without the diff:\n%s", got)
	}
}

func TestRepairPromptIsByteStable(t *testing.T) {
	t.Parallel()
	task, cfg := promptTask()
	review := model.Review{Completed: true, Summary: "One finding", Findings: []model.Finding{
		{Title: "Complete the output", File: "feature.txt:1", Detail: "Must contain \"fixed\".", Priority: "P1"},
	}}
	failures := []string{"go test ./...: exit status 1\nFAIL parser"}
	want := "Repair actionable findings and verification failures for this task. Preserve useful capabilities and meaningful tests. " +
		"Do not push, publish, merge or deploy. If a finding is unsupported, explain the technical evidence in your final summary; " +
		"the next fresh reviewer must independently assess it. " +
		`Rerun relevant verification ["go test ./...", "echo \"quoted\""]. ` +
		"Full comparison base: base-sha. Task: Fix the parser.\nKeep the API.. " +
		`Findings: [{"title":"Complete the output","file":"feature.txt:1","detail":"Must contain \"fixed\".","priority":"P1"}]. ` +
		`Verification failures: ["go test ./...: exit status 1\nFAIL parser"]`
	got, err := repairPrompt(task, cfg, review, failures)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("repair prompt changed:\n got %q\nwant %q", got, want)
	}
	got, err = repairPrompt(task, cfg, model.Review{Completed: true, Summary: "Clean"}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Findings: []. Verification failures: []") {
		t.Fatalf("empty findings and failures: %q", got)
	}
}

func TestTaskPromptsKeepFixturePrefixesAndPolicy(t *testing.T) {
	t.Parallel()
	task, cfg := promptTask()
	repair, err := repairPrompt(task, cfg, model.Review{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		role, prompt, prefix string
		requires             []string
	}{
		{"executor", executorPrompt(task, cfg), "Implement this accepted task", []string{"Do not push, publish, merge or deploy", "Full comparison base: base-sha"}},
		{"reviewer", reviewPrompt(task, "reviewed-sha", changeSet{}), "Perform a fresh code review", []string{"git diff base-sha HEAD", "Recorded HEAD: reviewed-sha", "do not modify files"}},
		{"repair", repair, "Repair actionable findings", []string{"Do not push, publish, merge or deploy", "Full comparison base: base-sha"}},
	} {
		if !strings.HasPrefix(check.prompt, check.prefix) {
			t.Errorf("%s prompt lost the fixture prefix %q", check.role, check.prefix)
		}
		for _, text := range check.requires {
			if !strings.Contains(check.prompt, text) {
				t.Errorf("%s prompt lost %q", check.role, text)
			}
		}
	}
}
