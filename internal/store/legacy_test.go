package store_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Port of tests/runners.rs legacy_routes_load_as_codex_in_configuration_sessions_and_admissions:
// routes saved before runners had a backend load as Codex wherever they were
// persisted, and the loaded routes still validate exactly.
func TestLegacyRoutesLoadAsCodexInConfigurationSessionsAndAdmissions(t *testing.T) {
	oldRoute := json.RawMessage(`{"model":"legacy-model","effort":"medium"}`)
	var cfg config.Config
	if err := json.Unmarshal([]byte(`{"repair_route":`+string(oldRoute)+`}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.RepairRoute != config.NewRoute("legacy-model", "medium") {
		t.Fatalf("legacy repair route: %+v", cfg.RepairRoute)
	}
	if cfg.OpencodeBinary != "opencode" {
		t.Fatalf("opencode binary default: %q", cfg.OpencodeBinary)
	}

	var session model.Session
	if err := json.Unmarshal([]byte(`{"id":"legacy-session","role":"repair","route":`+string(oldRoute)+`,"status":"completed","started_at":"2026-09-09","summary":"kept"}`), &session); err != nil {
		t.Fatal(err)
	}
	if session.Route.Backend != config.BackendCodex {
		t.Fatalf("legacy session backend: %v", session.Route.Backend)
	}

	data, err := json.Marshal(store.NewAdmission("cycle", nil, "repair", cfg.RepairRoute))
	if err != nil {
		t.Fatal(err)
	}
	var admission map[string]json.RawMessage
	if err := json.Unmarshal(data, &admission); err != nil {
		t.Fatal(err)
	}
	admission["route"] = oldRoute
	if data, err = json.Marshal(admission); err != nil {
		t.Fatal(err)
	}
	var loaded store.Admission
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Route.Backend != config.BackendCodex {
		t.Fatalf("legacy admission backend: %v", loaded.Route.Backend)
	}

	provider, variant := "fixture", "high"
	invalid := config.Route{Backend: config.BackendOpencode, Model: "fixture-model", Provider: &provider, Variant: &variant}
	invalid.Effort = "high"
	if invalid.Validate(false) == nil {
		t.Fatal("OpenCode route accepted a Codex effort")
	}
	codex := config.NewRoute("model", "high")
	codexProvider := "provider"
	codex.Provider = &codexProvider
	if codex.Validate(false) == nil {
		t.Fatal("Codex route accepted an OpenCode provider")
	}
	var unknown config.Route
	if err := json.Unmarshal([]byte(`{"backend":"unknown","model":"model"}`), &unknown); err == nil {
		t.Fatal("unknown backend accepted")
	}
}
