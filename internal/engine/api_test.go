package engine

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

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
	t.Parallel()
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
	t.Parallel()
	for _, active := range []string{"audit", "audit preflight", "baseline"} {
		t.Run(active, func(t *testing.T) {
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
				app.runtime.startPreflight(model.CycleModeAudit)
			case "baseline":
				app.runtime.baseline = &baselineJob{id: "baseline", cancel: func() {}}
			}
			app.runtimeMu.Unlock()
			_, err := app.ControlAction("resume")
			if err == nil || !IsActionConflict(err) || !strings.Contains(strings.ToLower(err.Error()), strings.Split(active, " ")[0]) {
				t.Fatalf("resume during %s = %v; want a conflict naming it", active, err)
			}
			saved, loadErr := app.Control()
			if loadErr != nil || saved.Mode != model.OperatingModePaused || saved.NextCycleAt != control.NextCycleAt || saved.Error == nil || *saved.Error != message {
				t.Fatalf("rejected resume changed control: %+v, %v", saved, loadErr)
			}
		})
	}
}

func TestControlConflictsExplainTheRequestedOperationWithoutChangingEligibility(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"continuous", "task", "execution", "idle"} {
		for _, action := range []string{"audit", "cycle", "resume", "pause"} {
			if scenario == "idle" && action == "audit" {
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

func TestAuditAndRunOnceRefusalsAfterTheGateCheckAreConflicts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		scenario string
		want     error
	}{
		{"audit preflight", ErrBusy},
		{"task", ErrBusy},
		{"continuous", ErrNotPaused},
	} {
		t.Run(test.scenario, func(t *testing.T) {
			app, control := controlFixture(t, test.scenario)
			if test.scenario == "audit preflight" {
				app.runtimeMu.Lock()
				app.runtime.startPreflight(model.CycleModeAudit)
				app.runtimeMu.Unlock()
			}
			if _, err := app.StartAudit(context.Background()); !errors.Is(err, test.want) || !IsActionConflict(err) {
				t.Fatalf("StartAudit = %v; want the %q conflict", err, test.want)
			}
			if _, err := app.ControlAction("cycle"); err == nil || !IsActionConflict(err) || !strings.HasPrefix(err.Error(), "Run once requires paused operation with no active work") {
				t.Fatalf("run once = %v; want the paused, idle conflict", err)
			}
			if saved, err := app.Control(); err != nil || saved.Mode != control.Mode || saved.Batch != nil {
				t.Fatalf("refused launch changed control: %+v, %v", saved, err)
			}
		})
	}
}

func TestOperatorPanicUnderTheGateReleasesIt(t *testing.T) {
	t.Parallel()
	for name, request := range map[string]func(*App){
		"control action": func(app *App) { _, _ = app.ControlAction("pause") },
		"task action":    func(app *App) { _ = app.TaskAction(context.Background(), "task", "archive") },
	} {
		t.Run(name, func(t *testing.T) {
			app := New(nil, t.TempDir())
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("the request did not panic")
					}
				}()
				request(app)
			}()
			stopped := make(chan struct{})
			go func() {
				app.Shutdown()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(30 * time.Second):
				t.Fatal("the gate stayed locked after the request panicked")
			}
		})
	}
}

func TestAuditControlRecordsOneOperatorEvent(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t, withGitHubIdentity())
	app := fixture.pausedApp(t)
	body, err := app.ControlAction("audit")
	if err != nil {
		t.Fatal(err)
	}
	if body["mode"] != "paused" {
		t.Fatalf("audit response mode = %v", body["mode"])
	}
	cycles, err := store.List[model.Cycle](fixture.state, "cycle")
	if err != nil || len(cycles) != 1 || cycles[0].Mode != model.CycleModeAudit {
		t.Fatalf("audit cycles = %+v, %v", cycles, err)
	}
	waitCycle(t, fixture.state, cycles[0].ID)
	events, err := fixture.state.Events(nil)
	if err != nil {
		t.Fatal(err)
	}
	operator := []model.Event{}
	for _, event := range events {
		if event.Kind == "operator" {
			operator = append(operator, event)
		}
	}
	if len(operator) != 1 || operator[0].EntityID != cycles[0].ID || operator[0].Message != "Audit started" {
		t.Fatalf("operator events = %+v; want one \"Audit started\" on cycle %s", operator, cycles[0].ID)
	}
}

func TestBaselineGateBlocksControlsConfigAndReconcile(t *testing.T) {
	t.Parallel()
	app, cfg := baselineApp(t)
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

func TestSaveConfigRevisionGatePreservesCanonicalValues(t *testing.T) {
	t.Parallel()
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
	if _, err := app.SaveConfig(revision, patch); err == nil || !IsActionConflict(err) {
		t.Fatalf("replayed revision: %v", err)
	}
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

func TestStateViewReportsBaselineSummaryWithoutCommands(t *testing.T) {
	t.Parallel()
	app, cfg := baselineApp(t)
	fingerprint, err := cfg.Fingerprint()
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
	t.Parallel()
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
	seen := map[any]int{}
	for i := 0; i < 150 && (seen[model.CycleRunning] < 10 || seen[model.CycleCompleted] < 10); i++ {
		view, err := app.StateView()
		if err != nil {
			t.Fatal(err)
		}
		cycles := view["cycles"].([]any)
		if len(cycles) != 1 {
			t.Fatalf("cycles: %v", cycles)
		}
		visible := cycles[0].(map[string]any)
		seen[visible["status"]]++
		mode, hasMode := view["active_cycle_mode"].(*model.CycleMode)
		if visible["status"] == model.CycleRunning && (view["cycle_active"] != true || !hasMode || *mode != model.CycleModeAudit) {
			t.Fatalf("running cycle must be active in the same response: cycle=%v active=%v mode=%v", visible, view["cycle_active"], view["active_cycle_mode"])
		}
	}
	if seen[model.CycleRunning] == 0 || seen[model.CycleCompleted] == 0 {
		t.Fatalf("the reads never overlapped both writer states: %v", seen)
	}
}

func TestStateViewRetainsRuntimeAuditActivity(t *testing.T) {
	t.Parallel()
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
				app.runtime.startPreflight(model.CycleModeAudit)
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

func TestStateViewStatusPrecedence(t *testing.T) {
	t.Parallel()
	for _, check := range []struct {
		name         string
		continuous   bool
		err          bool
		runtime      string
		storedCycle  bool
		wantStatus   string
		wantActive   int
		wantCycleRun bool
	}{
		{name: "paused with error and a running task", err: true, runtime: "task", wantStatus: "paused", wantActive: 1},
		{name: "paused audit preflight", runtime: "audit preflight", wantStatus: "auditing"},
		{name: "continuous with error and a running task", continuous: true, err: true, runtime: "task", wantStatus: "unhealthy", wantActive: 1},
		{name: "continuous with a running task", continuous: true, runtime: "task", wantStatus: "running", wantActive: 1},
		{name: "continuous with a stored running cycle", continuous: true, storedCycle: true, wantStatus: "running", wantCycleRun: true},
		{name: "continuous and idle", continuous: true, wantStatus: "idle"},
		{name: "paused and idle", wantStatus: "paused"},
	} {
		t.Run(check.name, func(t *testing.T) {
			app, control := controlFixture(t, "idle")
			if check.continuous {
				control.SetMode(model.OperatingModeContinuous)
			}
			if check.err {
				control.Error = stringPointer("recorded planning failure")
			}
			if err := app.Store.SaveControl(control); err != nil {
				t.Fatal(err)
			}
			app.runtimeMu.Lock()
			switch check.runtime {
			case "task":
				app.runtime.tasks["synthetic-task"] = taskJob{branch: "octomus/x", cancel: func() {}}
			case "audit preflight":
				app.runtime.startPreflight(model.CycleModeAudit)
			}
			app.runtimeMu.Unlock()
			if check.storedCycle {
				cycle := model.Cycle{
					Mode: model.CycleModeExecution, ID: "committed-cycle", Number: 1,
					Status: model.CycleRunning, StartedAt: model.Now(),
					Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
				}
				if err := app.Store.Put("cycle", cycle.ID, cycle); err != nil {
					t.Fatal(err)
				}
			}
			view, err := app.StateView()
			if err != nil {
				t.Fatal(err)
			}
			if view["status"] != check.wantStatus || view["active_tasks"] != check.wantActive || view["cycle_active"] != check.wantCycleRun {
				t.Fatalf("status = %v, active_tasks = %v, cycle_active = %v; want %s, %d, %v",
					view["status"], view["active_tasks"], view["cycle_active"], check.wantStatus, check.wantActive, check.wantCycleRun)
			}
		})
	}
}

func TestRecoveryAndGuardFailuresGenerateAttention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := openStore(t, dir)
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

type mismatchAdapter struct {
	runner.Adapter
	warning string
}

func (m mismatchAdapter) Diagnose(cwd string) (runner.Diagnostics, error) {
	diagnostics, err := m.Adapter.Diagnose(cwd)
	diagnostics.Warning = &m.warning
	return diagnostics, err
}

type catalogFailingAdapter struct {
	mismatchAdapter
}

func (catalogFailingAdapter) Models(string) ([]runner.Model, error) {
	return nil, errors.New("model/list: unexpected response shape")
}

// probedSandbox is a Docker-mode backend whose containment probe prints report on the host, or, with hold, runs until
// its caller gives up. With image, the probe's sandbox record names that image, as the broker's does.
type probedSandbox struct {
	sandbox.Host
	dir    string
	report string
	hold   bool
	image  string
}

func (probedSandbox) Mode() sandbox.Mode { return sandbox.ModeDocker }

func (p probedSandbox) Start(ctx context.Context, spec sandbox.Spec) (sandbox.Child, error) {
	if spec.Kind != sandbox.KindProbe {
		return p.Host.Start(ctx, spec)
	}
	command := "printf '%s' '" + p.report + "'"
	if p.hold {
		command = "sleep 60"
	}
	child, err := p.Host.Start(ctx, sandbox.Spec{Kind: sandbox.KindVerify, Dir: p.dir, Command: command})
	if err != nil || p.image == "" {
		return child, err
	}
	return evidencedChild{Child: child, record: &model.SandboxRecord{ImageID: p.image, Runtime: "runsc", Runs: 1}}, nil
}

type evidencedChild struct {
	sandbox.Child
	record *model.SandboxRecord
}

func (c evidencedChild) Evidence() *model.SandboxRecord { return c.record }

func TestSelfTestNamesTheImageItsProbeRanOn(t *testing.T) {
	t.Parallel()
	contained := `{"checks":[{"id":"non_root","label":"Runs as an unprivileged user","passed":true,"detail":"uid 10001"}],"kernel":"6.1"}`
	app := New(testStore(t), t.TempDir(), WithSandbox(probedSandbox{dir: t.TempDir(), report: contained, image: "sha256:rebuilt"}))
	t.Cleanup(app.Shutdown)
	record, err := app.SelfTest(context.Background())
	if err != nil || record.ImageID != "sha256:rebuilt" || record.Runtime != "runsc" {
		t.Fatalf("self-test = %+v, %v; want it kept with the image and runtime its probe ran on", record, err)
	}
}

const uncontainedReport = `{"checks":[{"id":"non_root","label":"Runs as an unprivileged user","passed":false,"detail":"uid 0"}],"kernel":"6.1"}`

func TestDoctorRunsTheSelfTestBeforeRepositoryChecks(t *testing.T) {
	t.Parallel()
	// Without the GitHub identity fixture the origin is not a GitHub remote, so the repository check fails.
	fixture := newScriptedFixture(t)
	app := fixture.pausedApp(t, WithSandbox(probedSandbox{dir: t.TempDir(), report: uncontainedReport}))
	result, _, err := app.DoctorFor(fixture.cfg, model.CycleModeExecution)
	if err == nil || result != nil || !strings.Contains(err.Error(), "Sandbox self-test failed: Runs as an unprivileged user (uid 0)") {
		t.Fatalf("doctor with a broken sandbox and remote = %v, %v; want the self-test failure reported", result, err)
	}
	if remoteErr := gitops.ValidateRemote(context.Background(), fixture.cfg); remoteErr == nil || !strings.Contains(err.Error(), remoteErr.Error()) {
		t.Fatalf("doctor error = %v; want the repository failure %v beside the self-test", err, remoteErr)
	}
	saved, err := store.Get[SandboxSelfTest](app.Store, "settings", selfTestRecord)
	if err != nil || saved == nil || saved.Passed || len(saved.Checks) != 1 {
		t.Fatalf("saved self-test = %+v, %v; want the failed probe recorded", saved, err)
	}
	// Invalid configuration fails without a repository check, but still after the self-test.
	invalid := fixture.cfg.Clone()
	invalid.VerificationCommands = nil
	if _, _, err := app.DoctorFor(invalid, model.CycleModeExecution); err == nil || !strings.Contains(err.Error(), "Sandbox self-test failed") {
		t.Fatalf("doctor with invalid configuration = %v; want the self-test failure too", err)
	}
}

func TestDoctorReportsBackendDiagnosticsAndWarnings(t *testing.T) {
	t.Parallel()
	fixture := newScriptedFixture(t, withGitHubIdentity())
	app := fixture.pausedApp(t)
	result, warnings, err := app.DoctorFor(fixture.cfg, model.CycleModeExecution)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("doctor = %v, warnings %q", err, warnings)
	}
	backends, err := wirejson.Marshal(result["backends"])
	if want := `[{"backend":"codex","protocol_version":"scripted","version":"scripted","warning":null}]`; err != nil || string(backends) != want {
		t.Fatalf("backends = %s, %v; want %s", backends, err, want)
	}
	if result["codex_version"] != "scripted" || result["tested_codex_version"] != runner.CodexTestedVersion {
		t.Fatalf("codex versions = %v, %v", result["codex_version"], result["tested_codex_version"])
	}
	if message := result["message"]; message != "Repository, GitHub authentication, and all model routes are available." {
		t.Fatalf("message = %v", message)
	}

	warning := runner.VersionWarning(config.BackendCodex, "scripted", "tested with a fixture")
	connect := fixture.script.Connector()
	mismatched := func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
		client, err := connect(ctx, backend, cfg, cwd)
		if err != nil {
			return nil, err
		}
		return mismatchAdapter{Adapter: client, warning: warning}, nil
	}
	app = fixture.pausedApp(t, WithRunnerConnector(mismatched))
	result, warnings, err = app.DoctorFor(fixture.cfg, model.CycleModeAudit)
	if err != nil || len(warnings) != 1 || warnings[0] != warning {
		t.Fatalf("doctor = %v, warnings %q; want %q", err, warnings, warning)
	}
	backends, err = wirejson.Marshal(result["backends"])
	quoted, _ := json.Marshal(warning)
	if want := `[{"backend":"codex","protocol_version":"scripted","version":"scripted","warning":` + string(quoted) + `}]`; err != nil || string(backends) != want {
		t.Fatalf("backends = %s, %v; want %s", backends, err, want)
	}
	if message := result["message"]; message != "Repository, GitHub authentication, and planning model routes are available. Warning: "+warning {
		t.Fatalf("message = %v", message)
	}
	fixture.script.SetCatalog()
	result, warnings, err = app.DoctorFor(fixture.cfg, model.CycleModeAudit)
	if err == nil || result != nil || len(warnings) != 1 || warnings[0] != warning {
		t.Fatalf("failing doctor = %v, %v, warnings %q; want the warning with the failure", result, err, warnings)
	}

	catalogFailing := func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
		client, err := connect(ctx, backend, cfg, cwd)
		if err != nil {
			return nil, err
		}
		return catalogFailingAdapter{mismatchAdapter{Adapter: client, warning: warning}}, nil
	}
	app = fixture.pausedApp(t, WithRunnerConnector(catalogFailing))
	result, warnings, err = app.DoctorFor(fixture.cfg, model.CycleModeAudit)
	if err == nil || err.Error() != "Codex: model/list: unexpected response shape" || result != nil || len(warnings) != 1 || warnings[0] != warning {
		t.Fatalf("doctor with a failing catalog = %v, %v, warnings %q; want the warning with the failure", result, err, warnings)
	}
}
