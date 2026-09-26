package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// holdRemoteRevisionRead replaces the fixture's git shim with one that, while
// the returned hold file exists, parks the remote default-branch head read
// after touching the returned entered file.
func holdRemoteRevisionRead(t *testing.T, f *planningFixture) (hold, entered string) {
	t.Helper()
	fixtures := filepath.Join(repositoryRoot(t), "tests", "fixtures")
	hold = filepath.Join(f.root, "hold-remote-revision")
	entered = filepath.Join(f.root, "remote-revision-entered")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os, runpy, sys, time
from pathlib import Path
os.environ['OCTOMUS_FIXTURE'] = %[1]q
sys.path.insert(0, %[2]q)
args = sys.argv[1:]
hold = Path(%[3]q)
if hold.exists() and args[:2] == ['ls-remote', '--heads'] and args[-1] == 'refs/heads/main':
    Path(%[4]q).touch()
    while hold.exists():
        time.sleep(0.02)
runpy.run_path(%[5]q, run_name='__main__')
`, f.root, fixtures, hold, entered, filepath.Join(fixtures, "git.py"))
	if err := os.WriteFile(filepath.Join(f.root, "bin", "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return hold, entered
}

// A configuration saved for another remote while a housekeeping observation
// is in flight makes that observation obsolete: it finishes without an error
// (so no housekeeping_error) and commits none of its parts, neither the
// default-branch revision nor the context fingerprint.
func TestObservationObsoletedByARemoteChangeCommitsNothing(t *testing.T) {
	fixture := newPlanningFixture(t)
	hold, entered := holdRemoteRevisionRead(t, fixture)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)

	done := make(chan error, 1)
	go func() { done <- app.observeRemote(context.Background(), fixture.cfg) }()
	waitForFixtureFile(t, entered, "observation did not read the remote default branch")
	changed := fixture.cfg.Clone()
	changed.DefaultBranch = "develop"
	if err := fixture.state.Put("settings", "config", changed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an obsolete observation was reported as failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("observation did not finish")
	}
	app.runtimeMu.Lock()
	observation := app.runtime.defaultObservation
	app.runtimeMu.Unlock()
	if observation != nil {
		t.Fatalf("obsolete observation recorded a default-branch revision: %+v", observation)
	}
	control, err := app.Control()
	if err != nil || control.ContextFingerprint != "" {
		t.Fatalf("obsolete observation recorded a context fingerprint: %+v, %v", control, err)
	}
}

// A current observation commits the default-branch revision together with the
// context fingerprint.
func TestObservationRecordsTheDefaultBranchWithItsContext(t *testing.T) {
	fixture := newPlanningFixture(t)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	if err := app.observeRemote(context.Background(), fixture.cfg); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/usr/bin/git", "--git-dir", filepath.Join(fixture.root, "remote.git"), "rev-parse", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(out))
	app.runtimeMu.Lock()
	observation := app.runtime.defaultObservation
	app.runtimeMu.Unlock()
	if observation == nil || observation.Revision != head || !observation.Describes(fixture.cfg) {
		t.Fatalf("default-branch observation = %+v; want %s for the configured remote", observation, head)
	}
	control, err := app.Control()
	if err != nil || control.ContextFingerprint == "" {
		t.Fatalf("observation did not record the context fingerprint: %+v, %v", control, err)
	}
}

// A changed remote context ends the idle streak and pulls only an extended
// backoff forward to the ordinary interval; the first observation and an
// unchanged context leave the schedule alone.
func TestContextFingerprintShortensOnlyExtendedIdleBackoff(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	const interval = 1800
	day := now.Unix() + 86400
	for _, tc := range []struct {
		name               string
		previous           string
		streak, wantStreak uint32
		next, wantNext     int64
	}{
		{name: "first observation", previous: "", streak: 3, next: day, wantStreak: 3, wantNext: day},
		{name: "unchanged context", previous: "observed", streak: 3, next: day, wantStreak: 3, wantNext: day},
		{name: "changed without a streak", previous: "earlier", streak: 0, next: day, wantStreak: 0, wantNext: day},
		{name: "changed after one idle plan", previous: "earlier", streak: 1, next: day, wantStreak: 0, wantNext: day},
		{name: "changed during extended backoff", previous: "earlier", streak: 3, next: day, wantStreak: 0, wantNext: now.Unix() + interval},
		{name: "changed with a sooner next cycle", previous: "earlier", streak: 3, next: now.Unix() + 60, wantStreak: 0, wantNext: now.Unix() + 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			control := model.DefaultControl()
			control.ContextFingerprint, control.IdleStreak, control.NextCycleAt = tc.previous, tc.streak, tc.next
			applyContextFingerprint(&control, "observed", now, interval)
			if control.ContextFingerprint != "observed" || control.IdleStreak != tc.wantStreak || control.NextCycleAt != tc.wantNext {
				t.Fatalf("fingerprint=%q streak=%d next=%d; want observed, %d, %d", control.ContextFingerprint, control.IdleStreak, control.NextCycleAt, tc.wantStreak, tc.wantNext)
			}
		})
	}
}
