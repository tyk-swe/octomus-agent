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

func TestGitHubObjectIDBranchRestriction(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{
		strings.Repeat("a", 40), strings.Repeat("A", 40),
		strings.Repeat("0", 40), strings.Repeat("aB09", 10),
	} {
		t.Run(branch, func(t *testing.T) {
			if ValidBranch(branch) {
				t.Error("GitHub rejects a branch name that looks like a Git object ID")
			}
			c := Default()
			c.DefaultBranch = branch
			if err := c.Validate(false); err == nil || !strings.Contains(err.Error(), "Default branch must be a valid branch name") {
				t.Fatalf("object ID default branch validation = %v", err)
			}
		})
	}
	for _, branch := range []string{
		strings.Repeat("a", 39), strings.Repeat("a", 41),
		strings.Repeat("a", 39) + "g", "release/" + strings.Repeat("a", 40),
	} {
		c := Default()
		c.DefaultBranch = branch
		if err := c.Validate(false); err != nil {
			t.Fatalf("supported branch %q rejected: %v", branch, err)
		}
	}
	c := Default()
	c.BranchPrefix = strings.Repeat("a", 40) + "/"
	if err := c.Validate(false); err != nil {
		t.Fatalf("object ID path component is a usable owned prefix: %v", err)
	}
}
