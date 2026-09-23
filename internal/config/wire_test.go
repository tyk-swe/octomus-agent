package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentConfigJSONContract(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"github_repo":"fixture/project"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubRepo != "fixture/project" || cfg.DiscoveryAgents != Default().DiscoveryAgents {
		t.Fatalf("defaults lost: %+v", cfg)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"github_repo"`, `"roles"`, `"tiers"`, `"verification_commands"`} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("missing %s in %s", field, data)
		}
	}
	for _, raw := range []string{`{"unknown":1}`, `{"github_repo":"a","github_repo":"b"}`, `{"roles":null}`} {
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestRouteJSONRequiresExactFields(t *testing.T) {
	for _, raw := range []string{`{"backend":"codex","model":"x","effort":"low","other":1}`, `{"model":null}`, `{"model":"a","model":"b"}`} {
		var route Route
		if err := json.Unmarshal([]byte(raw), &route); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
