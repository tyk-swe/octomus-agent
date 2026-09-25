package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// controlFixture names every route so
// the configuration counts as ready, with runtime scenarios that
// manipulates directly.
func controlFixture(t *testing.T, scenario string) (*App, model.Control) {
	t.Helper()
	state := testStore(t)
	app := New(state, t.TempDir())
	cfg := testConfig(t.TempDir())
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	if scenario == "continuous" {
		control.SetMode(model.OperatingModeContinuous)
	}
	if err := state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	app.runtimeMu.Lock()
	switch scenario {
	case "task":
		app.runtime.tasks["synthetic-task"] = taskJob{branch: "octomus/x", cancel: func() {}}
	case "execution":
		app.runtime.cycle = &cycleJob{id: "cycle", mode: model.CycleModeExecution, cancel: func() {}}
	case "audit":
		app.runtime.cycle = &cycleJob{id: "cycle", mode: model.CycleModeAudit, cancel: func() {}}
	}
	app.runtimeMu.Unlock()
	return app, control
}

func TestAuditControlsConflictWhileAuditRuns(t *testing.T) {
	app, _ := controlFixture(t, "audit")
	for _, check := range []struct {
		action      string
		explanation string
	}{
		{"audit", "Audits require paused operation with no active work"},
		{"cycle", "Run once requires paused operation with no active work"},
		{"resume", "Wait for the audit to finish before starting continuous operation"},
	} {
		_, err := app.ControlAction(check.action)
		if err == nil || !IsActionConflict(err) {
			t.Fatalf("%s: %v", check.action, err)
		}
		if !strings.HasPrefix(err.Error(), check.explanation) {
			t.Fatalf("%s message: %q", check.action, err.Error())
		}
	}
}

func TestResumePreservesAuditAndBaselineConflicts(t *testing.T) {
	for _, active := range []string{"audit", "audit preflight", "baseline"} {
		for _, action := range []string{"direct", "control action"} {
			t.Run(active+"/"+action, func(t *testing.T) {
				app, control := controlFixture(t, "idle")
				control.NextCycleAt = 1234567890
				message := "earlier planning failure"
				control.Error = &message
				if err := app.Store.SaveControl(control); err != nil {
					t.Fatal(err)
				}
				app.runtimeMu.Lock()
				switch active {
				case "audit":
					app.runtime.cycle = &cycleJob{id: "audit", mode: model.CycleModeAudit, cancel: func() {}}
				case "audit preflight":
					app.runtime.preflight = true
					app.runtime.preflightMode = model.CycleModeAudit
				case "baseline":
					app.runtime.baseline = &baselineJob{id: "baseline", cancel: func() {}}
				}
				app.runtimeMu.Unlock()
				var err error
				if action == "direct" {
					err = app.Resume()
				} else {
					_, err = app.ControlAction("resume")
					if err != nil && !IsActionConflict(err) {
						t.Fatalf("control action should report a conflict: %v", err)
					}
				}
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.Split(active, " ")[0]) {
					t.Fatalf("resume during %s: %v", active, err)
				}
				saved, loadErr := app.Control()
				if loadErr != nil || saved.Mode != model.OperatingModePaused || saved.NextCycleAt != control.NextCycleAt || saved.Error == nil || *saved.Error != message {
					t.Fatalf("rejected resume changed control: %+v, %v", saved, loadErr)
				}
			})
		}
	}
}

func TestControlConflictsExplainTheRequestedOperationWithoutChangingEligibility(t *testing.T) {
	for _, scenario := range []string{"continuous", "task", "execution", "idle"} {
		for _, action := range []string{"audit", "cycle", "resume", "pause"} {
			if scenario == "idle" && action == "audit" {
				// An accepted audit launches planning; integration coverage lives elsewhere.
				continue
			}
			t.Run(scenario+"/"+action, func(t *testing.T) {
				app, control := controlFixture(t, scenario)
				body, err := app.ControlAction(action)
				rejected := scenario != "idle" && (action == "audit" || action == "cycle")
				if rejected {
					if err == nil || !IsActionConflict(err) {
						t.Fatalf("%s/%s: %v", scenario, action, err)
					}
					operation := "Audits"
					if action == "cycle" {
						operation = "Run once"
					}
					if !strings.HasPrefix(err.Error(), operation) || !strings.Contains(err.Error(), "paused operation with no active work") {
						t.Fatalf("%s/%s: %q", scenario, action, err.Error())
					}
					if after, _ := app.Control(); after.Mode != control.Mode {
						t.Fatal("rejected control changed the durable mode")
					}
					return
				}
				if err != nil {
					t.Fatalf("%s/%s: %v", scenario, action, err)
				}
				expected := map[string]string{"cycle": "run_once", "resume": "continuous"}[action]
				if expected == "" {
					expected = "paused"
				}
				if body["mode"] != expected {
					t.Fatalf("%s/%s mode: %v", scenario, action, body["mode"])
				}
			})
		}
	}
}

func TestBaselineGateBlocksControlsConfigAndReconcile(t *testing.T) {
	app, cfg := baselineApp(t)
	// A synthetic live slot exercises every gate deterministically; the real
	// worker's lifecycle is covered by the check-lifecycle tests.
	app.runtimeMu.Lock()
	app.runtime.baseline = &baselineJob{id: "synthetic", cancel: func() {}}
	app.runtimeMu.Unlock()
	for _, action := range []string{"cycle", "resume", "audit"} {
		if _, err := app.ControlAction(action); err == nil || !IsActionConflict(err) {
			t.Fatalf("%s during baseline: %v", action, err)
		}
	}
	if _, err := app.SaveConfig("", nil); err == nil || !IsActionConflict(err) {
		t.Fatalf("config save during baseline: %v", err)
	}
	if _, err := app.ControlAction("pause"); err != nil {
		t.Fatalf("pause during baseline: %v", err)
	}
	// The seeded publication-uncertain task makes reconcile a live action.
	reason := model.BlockedReasonPublicationUncertain
	task := model.Task{
		ID: "task-seed", CycleID: "cycle-seed",
		Status: model.StatusBlocked, BlockedReason: &reason,
		Config: cfg.Clone(), Branch: "octomus/seed",
		Sessions: []model.Session{}, Reviews: []model.ReviewRound{}, Verification: []model.Verification{},
		OutputCommit: stringPointer("o"),
		CreatedAt:    model.Now(), UpdatedAt: model.Now(),
	}
	if err := app.Store.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	if err := app.TaskAction(context.Background(), task.ID, "reconcile"); err == nil || !IsActionConflict(err) {
		t.Fatalf("reconcile during baseline: %v", err)
	}
}

// TestSaveConfigRevisionGatePreservesCanonicalValues covers the optimistic
// concurrency contract: a stale revision conflicts without touching the saved
// record, a partial patch replaces only the named fields, and the returned
// view carries the new canonical revision plus display-transformation metadata.
func TestSaveConfigRevisionGatePreservesCanonicalValues(t *testing.T) {
	app, _ := controlFixture(t, "idle")
	live, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	revision, err := live.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("0", len(revision))
	patch := map[string]json.RawMessage{"default_branch": json.RawMessage(`"other"`)}
	if _, err := app.SaveConfig(stale, patch); err == nil || !IsActionConflict(err) {
		t.Fatalf("stale revision: %v", err)
	}
	if after, err := app.Config(); err != nil || after.DefaultBranch != live.DefaultBranch {
		t.Fatalf("stale save changed config: %v", err)
	}
	// A partial patch replaces only the named fields; canonical values the
	// operator did not touch survive byte-for-byte, including values whose
	// served display form is transformed.
	command := "echo ghp_syntheticsecrettoken123"
	patch = map[string]json.RawMessage{
		"verification_commands": json.RawMessage(`["` + command + `"]`),
		"max_sessions_per_day":  json.RawMessage(`200`),
	}
	view, err := app.SaveConfig(revision, patch)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	saved, err := app.Config()
	if err != nil {
		t.Fatal(err)
	}
	if saved.VerificationCommands[0] != command || saved.MaxSessionsPerDay != 200 ||
		saved.DefaultBranch != live.DefaultBranch || saved.GitHubRepo != live.GitHubRepo {
		t.Fatalf("merged config: %+v", saved)
	}
	newRevision, err := saved.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != newRevision || view.Revision == revision {
		t.Fatalf("view revision: %q", view.Revision)
	}
	if view.Config["verification_commands"].([]any)[0] != "echo [redacted]" {
		t.Fatalf("display command: %v", view.Config["verification_commands"])
	}
	fields := map[string][]string{}
	for _, entry := range view.TransformedFields {
		fields[entry.Field] = entry.Kinds
	}
	if got := fields["verification_commands"]; len(got) != 1 || got[0] != "redacted" {
		t.Fatalf("transforms: %+v", view.TransformedFields)
	}
	// Replaying the consumed revision conflicts; the saved record stays put.
	if _, err := app.SaveConfig(revision, patch); err == nil || !IsActionConflict(err) {
		t.Fatalf("replayed revision: %v", err)
	}
	// Unknown fields and duplicate keys inside the patch are typed rejections.
	for name, body := range map[string]map[string]json.RawMessage{
		"unknown field":  {"nonsense": json.RawMessage(`1`)},
		"duplicate keys": {"runner_storage_paths": json.RawMessage(`{"codex":"/a","codex":"/b"}`)},
	} {
		if _, err := app.SaveConfig(newRevision, body); err == nil {
			t.Fatalf("%s accepted", name)
		} else {
			var patchErr *ConfigPatchError
			if !errors.As(err, &patchErr) {
				t.Fatalf("%s error kind: %v", name, err)
			}
		}
	}
	if after, err := app.Config(); err != nil || !after.SameRemoteIdentity(saved) || after.MaxSessionsPerDay != saved.MaxSessionsPerDay {
		t.Fatalf("rejected patches changed config: %v", err)
	}
}

// stringPointer is shared with baseline_test.go's fixtures.
func TestStateViewReportsBaselineSummaryWithoutCommands(t *testing.T) {
	app, cfg := baselineApp(t)
	fingerprint, err := BaselineFingerprint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	revision := "abc"
	completed := model.Now()
	check := makeCheck(cfg, model.BaselineStatusPassed)
	check.ConfigFingerprint = fingerprint
	check.Revision = &revision
	check.CompletedAt = &completed
	check.WorkspaceRemoved = true
	check.Commands = append(check.Commands, model.BaselineCommand{
		Command: "true", Success: true, Output: "", CreatedAt: model.Now(),
	})
	if err := app.Store.Put("baseline", check.ID, check); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.Put("settings", "baseline_latest", check.ID); err != nil {
		t.Fatal(err)
	}
	view, err := app.StateView()
	if err != nil {
		t.Fatal(err)
	}
	baseline, ok := view["baseline"].(map[string]any)
	if !ok {
		t.Fatalf("baseline summary: %v", view["baseline"])
	}
	if baseline["id"] != check.ID || baseline["config_matches"] != true {
		t.Fatalf("summary: %v", baseline)
	}
	if baseline["config_revision"] != fingerprint {
		t.Fatalf("config revision: %v", baseline["config_revision"])
	}
	if _, ok := baseline["commands"]; ok {
		t.Fatal("the overview baseline summary must not include commands")
	}
	if view["baseline_active"] != false {
		t.Fatal("finished check must not read as active")
	}
}

func TestStateViewRunningCycleAndActivityUseOneSnapshot(t *testing.T) {
	app, _ := controlFixture(t, "idle")
	cycle := model.Cycle{
		Mode: model.CycleModeAudit, ID: "changing-cycle", Number: 1,
		Status: model.CycleRunning, StartedAt: model.Now(),
		Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
	}
	if err := app.Store.Put("cycle", cycle.ID, cycle); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			for _, status := range []string{model.CycleCompleted, model.CycleRunning} {
				cycle.Status = status
				if err := app.Store.Put("cycle", cycle.ID, cycle); err != nil {
					done <- err
					return
				}
				runtime.Gosched()
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Errorf("update cycle: %v", err)
		}
	}()
	for i := 0; i < 150; i++ {
		view, err := app.StateView()
		if err != nil {
			t.Fatal(err)
		}
		cycles := view["cycles"].([]any)
		if len(cycles) != 1 {
			t.Fatalf("cycles: %v", cycles)
		}
		visible := cycles[0].(map[string]any)
		mode, hasMode := view["active_cycle_mode"].(*model.CycleMode)
		if visible["status"] == model.CycleRunning && (view["cycle_active"] != true || !hasMode || *mode != model.CycleModeAudit) {
			t.Fatalf("running cycle must be active in the same response: cycle=%v active=%v mode=%v", visible, view["cycle_active"], view["active_cycle_mode"])
		}
	}
}

func TestStateViewRetainsRuntimeAuditActivity(t *testing.T) {
	for _, check := range []struct {
		name        string
		preflight   bool
		cycleActive bool
	}{
		{name: "preflight", preflight: true},
		{name: "active cycle", cycleActive: true},
	} {
		t.Run(check.name, func(t *testing.T) {
			app, _ := controlFixture(t, "idle")
			app.runtimeMu.Lock()
			if check.preflight {
				app.runtime.preflight = true
				app.runtime.preflightMode = model.CycleModeAudit
			} else {
				app.runtime.cycle = &cycleJob{id: "audit", mode: model.CycleModeAudit}
			}
			app.runtimeMu.Unlock()
			view, err := app.StateView()
			if err != nil {
				t.Fatal(err)
			}
			mode, ok := view["active_cycle_mode"].(*model.CycleMode)
			if view["cycle_active"] != check.cycleActive || !ok || *mode != model.CycleModeAudit || view["status"] != "auditing" {
				t.Fatalf("runtime audit: active=%v mode=%v status=%v", view["cycle_active"], view["active_cycle_mode"], view["status"])
			}
		})
	}
}

// Recovery and the worker guard both end interrupted episodes as blocked,
// which the enabled outbox captures once per episode.
func TestRecoveryAndGuardFailuresGenerateAttention(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	app := New(state, dir)
	cfg := testConfig(t.TempDir())
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	destination := "destination-a"
	if err := state.ConfigureNotifications(&destination, "enabled", nil); err != nil {
		t.Fatal(err)
	}
	executing := func() model.Task {
		return model.Task{
			ID: model.ID(), CycleID: "cycle",
			Status:         model.StatusExecuting,
			Config:         cfg.Clone(),
			SourceRevision: "s", ComparisonBase: "s", DefaultRevision: "s",
			Branch: "octomus/task", Workspace: "",
			Sessions: []model.Session{}, Reviews: []model.ReviewRound{},
			Verification: []model.Verification{},
			CreatedAt:    model.Now(), UpdatedAt: model.Now(),
		}
	}
	task := executing()
	if err := state.Put("task", task.ID, task); err != nil {
		t.Fatal(err)
	}
	if err := app.Recover(); err != nil {
		t.Fatal(err)
	}
	pending := func() int64 {
		health, err := state.NotificationHealth()
		if err != nil {
			t.Fatal(err)
		}
		return health.Pending
	}
	if pending() != 1 {
		t.Fatal("recovery did not capture the interrupted task")
	}
	guarded := executing()
	if err := state.Put("task", guarded.ID, guarded); err != nil {
		t.Fatal(err)
	}
	// The worker guard's durable effect: a still-active task ends blocked.
	if err := app.setTaskError(&guarded, errors.New("Task worker exited unexpectedly; inspect the preserved workspace")); err != nil {
		t.Fatal(err)
	}
	if pending() != 2 {
		t.Fatal("the guard fallback did not capture the abandoned task")
	}
	if err := app.Recover(); err != nil {
		t.Fatal(err)
	}
	if pending() != 2 {
		t.Fatal("recovery re-captured already-terminal episodes")
	}
}
