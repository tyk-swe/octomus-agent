package engine

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

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
		{name: "unknown dependency", tasks: []model.Task{task("b", existing, "missing")}, want: "Task b has unknown dependency missing"},
		{name: "default-branch dependency", tasks: []model.Task{task("a", "main"), task("b", "main", "a")}, want: "Default-branch tasks cannot depend on another task"},
		{name: "cross-target dependency", tasks: []model.Task{task("a", "octomus/other"), task("b", existing, "a")}, want: "Dependent tasks must write the same existing pull request"},
		{name: "two-task cycle", tasks: []model.Task{task("a", existing, "b"), task("b", existing, "a")}, want: "Task dependency cycle"},
		{name: "self dependency", tasks: []model.Task{task("a", existing, "a")}, want: "Task dependency cycle"},
		{name: "unordered writers", tasks: []model.Task{task("a", existing), task("b", existing)}, want: "Writers to octomus/existing do not form a total dependency order"},
		{name: "forked writers", tasks: []model.Task{task("a", existing), task("b", existing, "a"), task("c", existing, "a")}, want: "Writers to octomus/existing do not form a total dependency order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTaskPlan(tc.tasks)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("ValidateTaskPlan = %v; want %q", err, tc.want)
			}
		})
	}
}
