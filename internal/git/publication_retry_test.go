package git_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestFollowUpRetryAfterLostCommentResponse(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"unchanged", "closed", "merged", "reset-to-source", "advanced"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			c, root := fixtureRoot(t)
			c.VerificationCommands = []string{"make test"}
			task, commit := publishableTask(t, c, root, "task-followup-retry")
			number := uint64(42)
			task.PRNumber = &number
			remote := filepath.Join(root, "remote.git")
			ref := "refs/heads/" + task.Branch
			realGit(t, root, "--git-dir", remote, "update-ref", ref, task.SourceRevision)
			entry := prContextOwned(int(number), task.Branch)
			entry["html_url"] = "https://github.com/fixture/project/pull/42"
			writeFile(t, filepath.Join(root, "prs.json"), prContextPage(t, entry))
			original := filepath.Join(root, "bin", "gh-original")
			if err := os.Rename(filepath.Join(root, "bin", "gh"), original); err != nil {
				t.Fatal(err)
			}
			// GitHub accepts the comment, but the caller loses its response.
			wrapper := `#!/usr/bin/env python3
import os
from pathlib import Path
import subprocess
import sys
root = Path(os.environ['OCTOMUS_FIXTURE'])
args = sys.argv[1:]
original = [sys.executable, str(root / 'bin/gh-original'), *args]
if args[:2] == ['pr', 'comment']:
    subprocess.run(original, check=True)
    print('Fixture comment response interrupted', file=sys.stderr)
    sys.exit(1)
os.execv(sys.executable, original)
`
			if err := os.WriteFile(filepath.Join(root, "bin", "gh"), []byte(wrapper), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := git.Publish(ctx, task); model.BlockedReasonFromError(err) != model.BlockedReasonPublicationUncertain {
				t.Fatalf("lost comment response = %v; want publication_uncertain", err)
			}
			if head := realGit(t, root, "--git-dir", remote, "rev-parse", ref); head != commit {
				t.Fatalf("initial push = %s; want %s", head, commit)
			}
			data, err := os.ReadFile(filepath.Join(root, "prs.json"))
			if err != nil {
				t.Fatal(err)
			}
			var prs []map[string]any
			if err := json.Unmarshal(data, &prs); err != nil {
				t.Fatal(err)
			}
			comments, _ := prs[0]["comments"].([]any)
			if len(comments) != 1 || !strings.Contains(comments[0].(map[string]any)["body"].(string), "<!-- octomus:task:"+task.ID+" -->") {
				t.Fatalf("comment was not delivered before the lost response: %s", data)
			}
			// A maintainer can edit the PR while the service is interrupted.
			prs[0]["body"] = "Maintainer notes retained.\n\n" + prs[0]["body"].(string)
			switch change {
			case "closed":
				prs[0]["state"] = "closed"
			case "merged":
				prs[0]["state"] = "closed"
				prs[0]["merged_at"] = "2026-10-02T12:00:00Z"
			case "reset-to-source":
				realGit(t, root, "--git-dir", remote, "update-ref", ref, task.SourceRevision)
			case "advanced":
				tree := realGit(t, root, "--git-dir", remote, "rev-parse", commit+"^{tree}")
				head := realGit(t, root, "--git-dir", remote, "-c", "user.name=Maintainer", "-c", "user.email=maintainer@example.com", "commit-tree", tree, "-p", commit, "-m", "Later maintainer work")
				realGit(t, root, "--git-dir", remote, "update-ref", ref, head)
			}
			before, err := json.Marshal(prs)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, "prs.json"), string(before))
			remoteBefore := realGit(t, root, "--git-dir", remote, "for-each-ref")
			pr, err := git.Publish(ctx, task)
			if change == "reset-to-source" || change == "advanced" {
				if model.BlockedReasonFromError(err) != model.BlockedReasonRemoteConflict {
					t.Errorf("retry with changed delivered head = %+v, %v; want remote_conflict", pr, err)
				}
			} else if err != nil || pr.Number != number || pr.Head != commit {
				t.Errorf("retry failed to recognize completed delivery: %+v, %v", pr, err)
			}
			if after := realGit(t, root, "--git-dir", remote, "for-each-ref"); after != remoteBefore {
				t.Errorf("retry overwrote remote refs: before %q; after %q", remoteBefore, after)
			}
			if after, err := os.ReadFile(filepath.Join(root, "prs.json")); err != nil || string(after) != string(before) {
				t.Errorf("retry changed the PR description or comments: %s, %v", after, err)
			}
			entries, err := os.ReadFile(filepath.Join(root, "publications.jsonl"))
			if err != nil || strings.Count(string(entries), `"action"`) != 1 {
				t.Errorf("retry duplicated delivery: %s, %v", entries, err)
			}
		})
	}
}
