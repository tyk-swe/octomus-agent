package git_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/git"
)

func TestSnapshotIncludesWhitespaceOnlyPath(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"add", "modify", "delete"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			c, _ := fixtureRoot(t)
			ctx := context.Background()
			if operation != "add" {
				writeFile(t, filepath.Join(c.Repository, " "), "original\n")
				realGit(t, c.Repository, "add", "--", " ")
				realGit(t, c.Repository, "commit", "-m", "Track whitespace path")
			}
			base := realGit(t, c.Repository, "rev-parse", "HEAD")
			workspace := filepath.Join(t.TempDir(), "workspace")
			if err := git.CloneAt(ctx, c, workspace, base); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(workspace, " ")
			if operation == "delete" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFile(t, path, operation+"\n")
			}
			commit, err := git.Snapshot(ctx, c, workspace, "Snapshot "+operation)
			if err != nil {
				t.Fatal(err)
			}
			if commit == base {
				t.Fatal("snapshot omitted the only changed file and returned the previous commit")
			}
			if clean, err := git.At(ctx, c, workspace, commit); err != nil || !clean {
				t.Fatalf("snapshot left changes uncommitted: clean=%v err=%v", clean, err)
			}
		})
	}
}
