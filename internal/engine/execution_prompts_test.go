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
	want += "\nThe orchestrator's own git computed the change set from base-sha to reviewed-sha below. " +
		"Git inside your sandbox reads configuration and shell startup files earlier turns could change, " +
		"so where it shows other changes or other content, what follows is authoritative and the difference is itself a finding. " +
		"Some differences are expected and are not findings by themselves: this account ignores every .gitattributes file and shows every file as text, " +
		"so git may show a file as binary, count its lines differently or give other hunk headers; it shows a rename as a deletion and an addition, " +
		"and a submodule entry as the commits it points at. Each byte that is not UTF-8 shows as ⟦xNN⟧, and each control, invisible or line-separator character, " +
		"a literal ⟦ included, as ⟦U+XXXX⟧. Lines end only at real newlines: an escape such as ⟦U+000D⟧ or ⟦U+2028⟧ inside a line is a character the file holds, " +
		"which some languages and tools read as a line break.\n"
	if got := reviewPrompt(task, "reviewed-sha", changeSet{}); got != want+"The orchestrator's git shows no change between base-sha and reviewed-sha." {
		t.Fatalf("review prompt without changes:\n got %q\nwant %q", got, want)
	}
	trusted := changeSet{
		totals: "2 files changed, 2 insertions(+), 1 deletion(-)",
		files:  "1\t1\tparser.go\n1\t0\t\"caf\\303\\251.txt\"\n create mode 100644 \"caf\\303\\251.txt\"",
		diff:   "diff --git a/parser.go b/parser.go\n-old\n+new\n",
	}
	want += "Totals: 2 files changed, 2 insertions(+), 1 deletion(-)\n" +
		"Changed files (git diff --numstat --summary: lines added, lines deleted and path, - for a file git counts as binary):\n" +
		"1\t1\tparser.go\n1\t0\t\"caf\\303\\251.txt\"\n create mode 100644 \"caf\\303\\251.txt\"\n"
	if got := reviewPrompt(task, "reviewed-sha", trusted); got != want+"Complete diff:\ndiff --git a/parser.go b/parser.go\n-old\n+new\n" {
		t.Fatalf("review prompt changed:\n got %q\nwant %q", got, want)
	}
	trusted.omitted = []string{`"caf\303\251.txt"`}
	omitted := "The diffs of these files do not fit, so the orchestrator has not shown you their content: read each with git diff base-sha HEAD -- <file>, " +
		"check it against the line counts above and treat it as unverified:\n\"caf\\303\\251.txt\""
	if got := reviewPrompt(task, "reviewed-sha", trusted); got != want+"Complete diffs of the smallest files, within 65536 bytes:\ndiff --git a/parser.go b/parser.go\n-old\n+new\n"+omitted {
		t.Fatalf("review prompt with a diff left out:\n%s", got)
	}
	trusted.diff, trusted.omitted = "", []string{"parser.go", `"caf\303\251.txt"`}
	if got := reviewPrompt(task, "reviewed-sha", trusted); got != want+strings.Replace(omitted, "\n", "\nparser.go\n", 1) {
		t.Fatalf("review prompt with every diff left out:\n%s", got)
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
