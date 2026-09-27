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
	"github.com/tyk-swe/octomus-agent/internal/testutil"
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

// holdOpenPrInventoryRead replaces the fixture's gh peer with one that, while
// the returned hold file exists, parks the open-PR inventory read after
// touching the returned entered file, and while the returned fail file
// exists, fails that read.
func holdOpenPrInventoryRead(t *testing.T, f *planningFixture) (hold, entered, fail string) {
	t.Helper()
	fixtures := filepath.Join(repositoryRoot(t), "tests", "fixtures")
	hold = filepath.Join(f.root, "hold-open-pr-inventory")
	entered = filepath.Join(f.root, "open-pr-inventory-entered")
	fail = filepath.Join(f.root, "fail-open-pr-inventory")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os, runpy, sys, time
from pathlib import Path
os.environ['OCTOMUS_FIXTURE'] = %[1]q
sys.path.insert(0, %[2]q)
args = sys.argv[1:]
if args[:1] == ['api'] and '/pulls?state=open' in args[-1]:
    hold = Path(%[3]q)
    if hold.exists():
        Path(%[4]q).touch()
        while hold.exists():
            time.sleep(0.02)
    if Path(%[5]q).exists():
        sys.exit('fixture inventory outage')
runpy.run_path(%[6]q, run_name='__main__')
`, f.root, fixtures, hold, entered, fail, filepath.Join(fixtures, "gh.py"))
	if err := os.WriteFile(filepath.Join(f.root, "bin", "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return hold, entered, fail
}

// A configuration save that changes the PR identity while a housekeeping
// observation's inventory refresh is in flight makes that refresh obsolete,
// as a pause or save makes a dispatch refresh: its result is not recorded as
// the capacity failure reason, the pass records no housekeeping_error and
// commits nothing, and the next observation under the saved policy completes.
// A genuine inventory failure in a later pass is still reported.
func TestHousekeepingRefreshObsoletedByAPolicySaveIsNotAFailure(t *testing.T) {
	fixture := newPlanningFixture(t)
	hold, entered, fail := holdOpenPrInventoryRead(t, fixture)
	app := New(fixture.state, fixture.dataDir)
	t.Cleanup(app.Shutdown)
	// Only the remote observation runs in this pass.
	app.runtimeMu.Lock()
	app.runtime.lastRetention = time.Now()
	app.runtimeMu.Unlock()
	app.maybeStartHousekeeping(fixture.cfg)
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(entered)
		return err == nil
	}) {
		t.Fatal("housekeeping refresh did not read the open-PR inventory")
	}
	live, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	revision, err := live.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	patch := map[string]json.RawMessage{"branch_prefix": json.RawMessage(`"octomus-next/"`)}
	if _, err := app.SaveConfig(revision, patch); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	waitHousekeeping(t, app)

	app.runtimeMu.Lock()
	refreshError := app.runtime.prRefreshError
	app.runtimeMu.Unlock()
	if refreshError != "" {
		t.Fatalf("obsolete refresh was recorded as an inventory failure: %q", refreshError)
	}
	capacity, err := app.PrCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if capacity.Reason == nil || strings.Contains(*capacity.Reason, "policy changed") {
		t.Fatalf("obsolete refresh became the capacity reason: %+v", capacity)
	}
	system := "system"
	events, err := fixture.state.Events(&system)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "housekeeping_error" {
			t.Fatalf("obsolete refresh was recorded as a housekeeping failure: %+v", event)
		}
	}
	if inventory, err := fixture.state.OpenPrInventory(); err != nil || inventory != nil {
		t.Fatalf("obsolete refresh saved an inventory: %+v, %v", inventory, err)
	}
	control, err := app.Control()
	if err != nil || control.ContextFingerprint != "" {
		t.Fatalf("obsolete observation recorded a context fingerprint: %+v, %v", control, err)
	}

	// The next observation under the saved policy completes normally.
	saved, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.observeRemote(context.Background(), saved); err != nil {
		t.Fatalf("observation under the saved policy failed: %v", err)
	}
	if inventory, err := fixture.state.OpenPrInventory(); err != nil || inventory == nil {
		t.Fatalf("observation under the saved policy saved no inventory: %+v, %v", inventory, err)
	}
	control, err = app.Control()
	if err != nil || control.ContextFingerprint == "" {
		t.Fatalf("observation under the saved policy recorded no context fingerprint: %+v, %v", control, err)
	}

	if err := os.WriteFile(fail, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	app.runtimeMu.Lock()
	app.runtime.lastObserve = time.Time{}
	app.runtimeMu.Unlock()
	app.maybeStartHousekeeping(saved)
	waitHousekeeping(t, app)
	app.runtimeMu.Lock()
	refreshError = app.runtime.prRefreshError
	app.runtimeMu.Unlock()
	if !strings.Contains(refreshError, "Open pull request inventory failed") {
		t.Fatalf("an inventory outage was not recorded as an inventory failure: %q", refreshError)
	}
	events, err = fixture.state.Events(&system)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, event := range events {
		if event.Kind == "housekeeping_error" {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("housekeeping reported %d failures; want the inventory outage only: %+v", failures, events)
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
	head := git(t, fixture.root, "--git-dir", filepath.Join(fixture.root, "remote.git"), "rev-parse", "main")
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
