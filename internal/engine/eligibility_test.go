package engine

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestCapabilitiesShareControlBlockers(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	control := model.Control{Paused: true, Mode: model.OperatingModePaused}
	capacity := model.PlanningCapacity{Status: model.CapacityReady}
	for _, test := range []struct {
		name     string
		activity operationActivity
		recovery *string
		expected map[string]bool
	}{
		{"idle", operationActivity{idle: true}, nil, map[string]bool{"pause": true, "resume": true, "cycle": true, "audit": true, "configure": true}},
		{"execution", operationActivity{}, nil, map[string]bool{"pause": true, "resume": true}},
		{"audit", operationActivity{audit: true}, nil, map[string]bool{}},
		{"baseline", operationActivity{baseline: true}, nil, map[string]bool{"pause": true}},
		{"recovery", operationActivity{idle: true}, new("Settlement pending"), map[string]bool{"pause": true, "configure": true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := capabilities(control, cfg, test.activity, test.recovery, capacity)
			for action, capability := range map[string]Capability{"pause": got.Pause, "resume": got.Resume, "cycle": got.Cycle, "audit": got.Audit, "configure": got.Configure} {
				if capability.Allowed != test.expected[action] {
					t.Errorf("%s=%v; want %v (%s)", action, capability.Allowed, test.expected[action], capability.Reason)
				}
				if !capability.Allowed && capability.Reason == "" {
					t.Errorf("%s has no blocker", action)
				}
				if capability.Allowed && action != "configure" {
					var controlAction model.ControlAction
					if err := controlAction.UnmarshalText([]byte(action)); err != nil {
						t.Fatal(err)
					}
					if err := controlBlocker(controlAction, control, test.activity); err != nil {
						t.Errorf("advertised %s refused by shared policy: %v", action, err)
					}
				}
			}
		})
	}
	for _, mode := range []model.OperatingMode{model.OperatingModeContinuous, model.OperatingModeRunOnce} {
		got := capabilities(model.Control{Mode: mode}, cfg, operationActivity{idle: true}, nil, capacity)
		if got.Cycle.Allowed || got.Audit.Allowed || got.Configure.Allowed || !got.Pause.Allowed || !got.Resume.Allowed {
			t.Fatalf("mode %s: %+v", mode, got)
		}
	}
	for _, status := range []model.CapacityStatus{model.CapacityExhausted, model.CapacityTooLow} {
		got := capabilities(control, cfg, operationActivity{idle: true}, nil, model.PlanningCapacity{Status: status})
		if got.Cycle.Allowed || got.Audit.Allowed || !got.Resume.Allowed || !got.Pause.Allowed || !got.Configure.Allowed {
			t.Fatalf("capacity %s: %+v", status, got)
		}
	}
	auditOnly := cfg.Clone()
	executor := auditOnly.Tiers["XS"]
	executor.Model = ""
	auditOnly.Tiers["XS"] = executor
	gotAudit := capabilities(control, auditOnly, operationActivity{idle: true}, nil, capacity)
	if !gotAudit.Audit.Allowed || gotAudit.Cycle.Allowed || gotAudit.Resume.Allowed {
		t.Fatalf("audit-only configuration: %+v", gotAudit)
	}

	// Configuration failures cannot hide the operator's ability to stop admission.
	got := capabilities(control, config.Default(), operationActivity{idle: true}, nil, capacity)
	if !got.Pause.Allowed || got.Resume.Allowed || got.Cycle.Allowed || got.Audit.Allowed {
		t.Fatalf("invalid configuration: %+v", got)
	}
}

func TestStateViewPreflightCapabilitiesMatchControls(t *testing.T) {
	t.Parallel()
	for _, mode := range []model.CycleMode{model.CycleModeAudit, model.CycleModeExecution} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			app, _ := controlFixture(t, "idle")
			app.runtimeMu.Lock()
			app.runtime.startPreflight(mode)
			app.runtimeMu.Unlock()

			view, err := app.StateView()
			if err != nil {
				t.Fatal(err)
			}
			if view.Capabilities.Configure.Allowed || view.Capabilities.Configure.Reason == "" {
				t.Fatalf("configuration during %s preflight: %+v", mode, view.Capabilities.Configure)
			}
			if _, err := app.SaveConfig("", nil); ActionCode(err) != ActiveWorkConflict || err.Error() != view.Capabilities.Configure.Reason {
				t.Fatalf("configuration during %s preflight: %v; want advertised active-work blocker", mode, err)
			}
			for action, capability := range map[model.ControlAction]Capability{model.ControlCycle: view.Capabilities.Cycle, model.ControlAudit: view.Capabilities.Audit} {
				if capability.Allowed || capability.Reason == "" {
					t.Fatalf("%s during %s preflight: %+v", action, mode, capability)
				}
				if _, err := app.ControlAction(action); ActionCode(err) != ActiveWorkConflict || err.Error() != capability.Reason {
					t.Fatalf("%s during %s preflight: %v; want advertised active-work blocker", action, mode, err)
				}
			}

			// Execution preflight remains interruptible. Audit's existing display
			// policy hides an already-paused no-op, but the Pause handler accepts it.
			if mode == model.CycleModeExecution && !view.Capabilities.Pause.Allowed {
				t.Fatalf("pause unavailable during execution preflight: %+v", view.Capabilities.Pause)
			}
			if _, err := app.ControlAction(model.ControlPause); err != nil {
				t.Fatalf("pause during %s preflight: %v", mode, err)
			}
		})
	}
}
