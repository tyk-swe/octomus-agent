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

func TestFollowUpRequiresObservedDeliveryMarker(t *testing.T) {
	t.Parallel()
	for _, fault := range []string{"removed-comment", "comments-unavailable"} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			c, root := fixtureRoot(t)
			c.VerificationCommands = []string{"make test"}
			task, commit := publishableTask(t, c, root, "task-followup-marker")
			number := uint64(42)
			task.PRNumber = &number
			realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"), "update-ref", "refs/heads/"+task.Branch, task.SourceRevision)
			entry := prContextOwned(int(number), task.Branch)
			entry["html_url"] = "https://github.com/fixture/project/pull/42"
			writeFile(t, filepath.Join(root, "prs.json"), prContextPage(t, entry))
			original := filepath.Join(root, "bin", "gh-original")
			if err := os.Rename(filepath.Join(root, "bin", "gh"), original); err != nil {
				t.Fatal(err)
			}
			// Simulate a follow-up comment disappearing after a successful write, or
			// its subsequent observation failing. The PR's original ownership marker
			// stays intact, so it cannot stand in for this task's delivery marker.
			wrapper := `#!/usr/bin/env python3
import json
import os
from pathlib import Path
import subprocess
import sys
root = Path(os.environ['OCTOMUS_FIXTURE'])
args = sys.argv[1:]
original = [sys.executable, str(root / 'bin/gh-original'), *args]
fault = (root / 'comment-fault').read_text()
if args[:2] == ['pr', 'comment']:
    subprocess.run(original, check=True)
    (root / 'comment-written').touch()
    if fault == 'removed-comment':
        path = root / 'prs.json'
        prs = json.loads(path.read_text())
        prs[0]['comments'] = []
        path.write_text(json.dumps(prs))
elif args[0] == 'api' and '/comments' in args[-1] and (root / 'comment-written').exists() and fault == 'comments-unavailable':
    print('Fixture comments temporarily unavailable', file=sys.stderr)
    sys.exit(1)
else:
    os.execv(sys.executable, original)
`
			if err := os.WriteFile(filepath.Join(root, "bin", "gh"), []byte(wrapper), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, "comment-fault"), fault)
			ctx := context.Background()
			pr, err := git.Publish(ctx, task)
			if err == nil {
				t.Fatalf("follow-up was reported delivered without observable task marker: %+v", pr)
			}
			if reason := model.BlockedReasonFromError(err); reason != model.BlockedReasonPublicationUncertain {
				t.Fatalf("reason = %v; want publication_uncertain: %v", reason, err)
			}
			if remote := realGit(t, root, "--git-dir", filepath.Join(root, "remote.git"), "rev-parse", task.Branch); remote != commit {
				t.Fatalf("reviewed commit was not pushed before the comment fault: %s", remote)
			}
			writeFile(t, filepath.Join(root, "comment-fault"), "")
			pr, err = git.Publish(ctx, task)
			if err != nil || pr.Number != number {
				t.Fatalf("retry did not reconcile delivery: %+v, %v", pr, err)
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
				t.Fatalf("retry must leave exactly one delivery marker: %s", data)
			}
		})
	}
}
