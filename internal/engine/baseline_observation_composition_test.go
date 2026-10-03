package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestBaselineRetainsPinnedEvidenceAfterRemoteChanges(t *testing.T) {
	for _, change := range []string{"advance", "missing"} {
		t.Run(change, func(t *testing.T) {
			f := newPlanningFixture(t)
			hold, entered := filepath.Join(f.root, "verification-hold"), filepath.Join(f.root, "verification-entered")
			if err := os.WriteFile(hold, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			f.cfg.VerificationCommands = []string{fmt.Sprintf("touch %q; while [ -e %q ]; do sleep 0.02; done; echo pinned", entered, hold)}
			if err := f.state.Put("settings", "config", f.cfg); err != nil {
				t.Fatal(err)
			}
			a := New(f.state, f.dataDir)
			t.Cleanup(a.Shutdown)
			t.Cleanup(func() { _ = os.Remove(hold) })
			fingerprint, err := f.cfg.Fingerprint()
			if err != nil {
				t.Fatal(err)
			}
			original := git(t, f.repo, "rev-parse", "HEAD")
			check, err := a.StartBaseline(fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			waitForFixtureFile(t, entered, "baseline verification did not start")
			patch := map[string]json.RawMessage{"max_retries": json.RawMessage(`3`)}
			if _, err := a.SaveConfig(fingerprint, patch); !IsActionConflict(err) {
				t.Fatalf("configuration changed during held verification: %v", err)
			}
			if change == "advance" {
				git(t, f.repo, "commit", "--allow-empty", "-m", "advance remote")
				git(t, f.repo, "push", "origin", "main")
			} else {
				git(t, f.root, "--git-dir", filepath.Join(f.root, "remote.git"), "update-ref", "-d", "refs/heads/main")
			}
			if err := a.observeRemote(context.Background(), f.cfg); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(hold); err != nil {
				t.Fatal(err)
			}
			a.wg.Wait()
			view, err := a.BaselineView(nil)
			if err != nil {
				t.Fatal(err)
			}
			saved := view["check"].(*model.BaselineCheck)
			if saved.ID != check.ID || saved.Status != model.BaselineStatusPassed || saved.Revision == nil || *saved.Revision != original || len(saved.Commands) != 1 || !saved.Commands[0].Success || !saved.WorkspaceRemoved || view["config_matches"] != true {
				t.Fatalf("pinned baseline evidence changed: %+v", view)
			}
			want := "stale"
			if change == "missing" {
				want = "unknown"
			}
			if view["revision_status"] != want {
				t.Fatalf("remote %s: revision_status = %v; want %s", change, view["revision_status"], want)
			}
			if change == "missing" {
				if observation, _ := view["default_observation"].(*model.DefaultBranchObservation); observation != nil {
					t.Fatalf("missing remote branch still exposed a revision: %+v", observation)
				}
			}
			if _, err := a.SaveConfig(fingerprint, patch); err != nil {
				t.Fatal(err)
			}
			view, err = a.BaselineView(nil)
			if err != nil || view["config_matches"] != false || view["revision_status"] != want {
				t.Fatalf("configuration and revision freshness were not independent: %+v, %v", view, err)
			}
		})
	}
}

func TestMissingDefaultObservationPreventsOlderReadsRestoringReadiness(t *testing.T) {
	a, cfg := baselineApp(t)
	t.Cleanup(a.Shutdown)
	old := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	missing := model.Now()
	if err := a.observeDefaultBranch(cfg, "", missing); err != nil {
		t.Fatal(err)
	}
	if err := a.observeDefaultBranch(cfg, strings.Repeat("a", 40), old); err != nil {
		t.Fatal(err)
	}
	a.runtimeMu.Lock()
	observation := *a.runtime.defaultObservation
	a.runtimeMu.Unlock()
	if observation.Revision != "" || observation.ObservedAt != missing {
		t.Fatalf("older read restored a missing branch: %+v", observation)
	}
	if err := a.observeDefaultBranch(cfg, strings.Repeat("b", 40), time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	a.runtimeMu.Lock()
	observation = *a.runtime.defaultObservation
	a.runtimeMu.Unlock()
	if observation.Revision != strings.Repeat("b", 40) {
		t.Fatalf("newer read could not observe a restored branch: %+v", observation)
	}
}

func TestMissingDefaultBranchInvalidatesEveryObservationProducer(t *testing.T) {
	for _, producer := range []string{"housekeeping", "baseline", "grounding"} {
		for _, change := range []string{"missing", "transport failure"} {
			t.Run(producer+"/"+change, func(t *testing.T) {
				f := newScriptedPlanningFixture(t)
				a := f.pausedApp(t)
				fingerprint, err := f.cfg.Fingerprint()
				if err != nil {
					t.Fatal(err)
				}
				check, err := a.StartBaseline(fingerprint)
				if err != nil {
					t.Fatal(err)
				}
				a.wg.Wait()
				before, err := a.BaselineView(&check.ID)
				if err != nil || before["revision_status"] != "matches_last_observation" {
					t.Fatalf("initial baseline observation: %+v, %v", before, err)
				}
				original := before["check"].(*model.BaselineCheck)
				if original.Status != model.BaselineStatusPassed {
					t.Fatalf("initial baseline did not pass: %+v", original)
				}
				if change == "missing" {
					git(t, f.root, "--git-dir", filepath.Join(f.root, "remote.git"), "update-ref", "-d", "refs/heads/main")
				} else {
					fixtureGit := filepath.Join(repositoryRoot(t), "tests", "fixtures", "git.sh")
					script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = ls-remote ] && [ \"$2\" = --heads ]; then echo 'synthetic remote read failure' >&2; exit 1; fi\nexec /bin/sh %q \"$@\"\n", fixtureGit)
					if err := os.WriteFile(filepath.Join(f.root, "bin", "git"), []byte(script), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				var producerErr error
				switch producer {
				case "housekeeping":
					producerErr = a.observeRemote(context.Background(), f.cfg)
				case "baseline":
					if _, err := a.StartBaseline(fingerprint); err != nil {
						t.Fatal(err)
					}
					a.wg.Wait()
					latest, err := f.state.LatestBaseline()
					if err != nil || latest == nil || latest.Status != model.BaselineStatusFailed || latest.Error == nil {
						t.Fatalf("failed baseline result: %+v, %v", latest, err)
					}
					producerErr = fmt.Errorf("%s", *latest.Error)
				case "grounding":
					cycle := groundingCycle(t, f, model.CycleModeAudit)
					_, producerErr = a.captureGrounding(context.Background(), f.cfg, &cycle)
				}
				if change == "missing" && producer != "housekeeping" && (producerErr == nil || !strings.Contains(producerErr.Error(), "Default branch missing")) {
					t.Fatalf("missing branch not reported: %v", producerErr)
				}
				if change == "transport failure" && (producerErr == nil || !strings.Contains(producerErr.Error(), "synthetic remote read failure")) {
					t.Fatalf("remote failure not reported: %v", producerErr)
				}
				view, err := a.BaselineView(&check.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := "unknown"
				if change == "transport failure" {
					want = "matches_last_observation"
				}
				if view["revision_status"] != want {
					t.Fatalf("historical pass after %s: revision_status = %v; want %s", change, view["revision_status"], want)
				}
				if saved := view["check"].(*model.BaselineCheck); saved.Status != original.Status || saved.Revision == nil || *saved.Revision != *original.Revision || view["config_matches"] != true {
					t.Fatalf("historical pass changed after %s: %+v", change, view)
				}
			})
		}
	}
}
