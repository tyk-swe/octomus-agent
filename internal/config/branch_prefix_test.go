package config

import (
	"strings"
	"testing"
)

func TestOwnedBranchPrefixCannotDescendFromDefaultBranch(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		defaultBranch string
		prefix        string
		valid         bool
	}{
		{"octomus", "octomus/", false},
		{"main", "main/automation/", false},
		{"release/stable", "release/stable/tasks/", false},
		{"main", "mainline/", true},
		{"release/stable", "release/stable-fixes/", true},
		{"release/stable", "release/tasks/", true},
	} {
		t.Run(test.defaultBranch+":"+test.prefix, func(t *testing.T) {
			c := Default()
			c.DefaultBranch, c.BranchPrefix = test.defaultBranch, test.prefix
			err := c.Validate(false)
			if test.valid {
				if err != nil {
					t.Fatalf("distinct Git branch paths must remain usable: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "Owned branch prefix cannot be nested under the default branch") {
				t.Fatalf("configuration permits a Git ref directory/file conflict: %v", err)
			}
		})
	}
}
