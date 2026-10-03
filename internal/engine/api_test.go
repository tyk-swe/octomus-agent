package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
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
