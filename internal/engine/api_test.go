package engine

// Operator controls and views: control conflicts, the state view, the doctor and configuration saves.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
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

func TestAuditControlConflicts(t *testing.T) {
	t.Parallel()
	app, _ := controlFixture(t, "audit")
	for _, action := range []string{"audit", "cycle", "resume"} {
		if _, err := app.ControlAction(action); err == nil || !IsActionConflict(err) {
			t.Fatalf("%s during an audit: %v; want a conflict", action, err)
		}
	}
}

func TestStateViewStatus(t *testing.T) {
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
				control.Error = new("recorded planning failure")
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

func TestDoctorDiagnostics(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	app := f.pausedApp(t)
	result, warnings, err := app.DoctorFor(f.cfg, model.CycleModeExecution)
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
	connect := f.script.Connector()
	mismatched := func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
		client, err := connect(ctx, backend, cfg, cwd)
		if err != nil {
			return nil, err
		}
		return mismatchAdapter{Adapter: client, warning: warning}, nil
	}
	app = f.pausedApp(t, WithRunnerConnector(mismatched))
	result, warnings, err = app.DoctorFor(f.cfg, model.CycleModeAudit)
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
	f.script.SetCatalog()
	result, warnings, err = app.DoctorFor(f.cfg, model.CycleModeAudit)
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
	app = f.pausedApp(t, WithRunnerConnector(catalogFailing))
	result, warnings, err = app.DoctorFor(f.cfg, model.CycleModeAudit)
	if err == nil || err.Error() != "Codex: model/list: unexpected response shape" || result != nil || len(warnings) != 1 || warnings[0] != warning {
		t.Fatalf("doctor with a failing catalog = %v, %v, warnings %q; want the warning with the failure", result, err, warnings)
	}
}

func TestSaveConfigKeepsPinnedIdentity(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"repository", "github_repo"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			state := testStore(t)
			deployment := Deployment{Repository: t.TempDir(), GitHubRepo: "fixture/project"}
			app := New(state, t.TempDir(), WithDeployment(deployment))
			cfg := deployment.pin(config.Default())
			if err := state.Put("settings", "config", cfg); err != nil {
				t.Fatal(err)
			}
			loaded, err := app.Settings()
			if err != nil {
				t.Fatal(err)
			}
			value := deployment.Repository + "/other"
			if field == "github_repo" {
				value = "fixture/other"
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.SaveConfig(loaded.Revision, map[string]json.RawMessage{
				field:         raw,
				"max_retries": json.RawMessage(`3`),
			}); err == nil {
				t.Fatal("save accepted a different deployment identity")
			}
			stored, err := store.Get[config.Config](state, "settings", "config")
			if err != nil {
				t.Fatal(err)
			}
			if !wirejson.Equal(stored, cfg) {
				t.Fatal("rejected save changed persisted configuration")
			}
		})
	}
}
