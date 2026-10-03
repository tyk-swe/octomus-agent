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
