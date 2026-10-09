package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestReviewMaintenanceBaseRetargetBetweenReadAndMutation(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.newApp(t)
	remote := filepath.Join(f.root, "remote.git")
	original := git(t, f.root, "--git-dir", remote, "rev-parse", "main")
	git(t, f.root, "--git-dir", remote, "update-ref", "refs/heads/release", original)
	if reason, waiting, terminal, revoke := app.mergeBlocked(t.Context(), f.cfg, saved, *observation.AutoMerge); reason != "" || waiting || terminal || revoke {
		t.Fatalf("fixture not eligible before retarget: %q %v %v %v", reason, waiting, terminal, revoke)
	}
	// gh.py retargets inside PUT, after both remote prechecks and the ruleset
	// read. It implements the real update restriction with the merger identity;
	// there is no imaginary original_base_ref/head_ref API guard.
	if err := os.WriteFile(filepath.Join(f.root, "merge-retarget.json"), []byte(`{"base":"release"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	app.mergeAttempt(t.Context(), saved, observation, false)
	app.wg.Wait()
	for _, branch := range []string{"main", "release"} {
		if revision := git(t, f.root, "--git-dir", remote, "rev-parse", branch); revision != original {
			t.Fatalf("retargeted mutation changed %s: %s -> %s", branch, original, revision)
		}
	}
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge.Status != model.AutoMergeManual || merge.ResultSource != nil || merge.MergeCommit != nil ||
		!strings.Contains(merge.Reason, "protected ref") || len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("retargeted merge was not refused at the server: %+v", merge)
	}
}

func TestMergeUnverifiablePolicyStaysManualWithoutMutation(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.newApp(t)
	if err := os.WriteFile(filepath.Join(f.root, "merge-ruleset.json"), []byte(`{"id":17,"enforcement":"disabled"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	app.mergeAttempt(t.Context(), saved, observation, false)
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge.Status != model.AutoMergeManual || !merge.Authorized || !strings.Contains(merge.Reason, "destination is not enforced") || len(mergeAttempts(t, f)) != 0 {
		t.Fatalf("missing enforcement did not stop before mutation: %+v", merge)
	}
}

func TestMergeAcknowledgementNeedsMatchingRemoteOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, file, content string }{
		{"wrong commit", "merge-ack.json", `{"sha":"` + strings.Repeat("d", 40) + `"}`},
		{"unreadable status", "merge-status-unreadable", ""},
		{"forbidden status", "merge-status-unreadable", "403"},
		{"missing status", "merge-status-unreadable", "404"},
		{"disallowed status", "merge-status-unreadable", "405"},
		{"invalid status", "merge-status-unreadable", "422"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := maintenanceFixture(t)
			saved, observation := publishedMaintenanceTask(t, f)
			app := f.newApp(t)
			path := filepath.Join(f.root, test.file)
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			app.mergeAttempt(t.Context(), saved, observation, false)
			current := prObservation(t, f, *saved.PRNumber)
			if current.AutoMerge.Status != model.AutoMergeUncertain || current.AutoMerge.ResultSource != nil {
				t.Fatalf("unverified acknowledgement recorded as confirmed: %+v", current.AutoMerge)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			app.reconcileMerge(t.Context(), f.cfg, current)
			merge := prObservation(t, f, *saved.PRNumber).AutoMerge
			if merge.Status != model.AutoMergeMerged || merge.ResultSource == nil || *merge.ResultSource != "observed" || len(mergeAttempts(t, f)) != 1 {
				t.Fatalf("outcome was not reconciled without a second write: %+v", merge)
			}
		})
	}
}
