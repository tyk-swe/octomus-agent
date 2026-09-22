package runner

// Port of tests/runners.rs:601 pinned_opencode_protocol_smoke_without_model_calls.
// The pinned CLI drives only the local protocol surface — health, policy,
// catalog, session create and resume — against a provider whose endpoint can
// never answer, so no model call is possible. Skipped unless
// OCTOMUS_OPENCODE_SMOKE_BINARY points at the pinned binary; the heavier
// TestPinnedOpenCodeContract exercises real turns under
// OCTOMUS_CONTRACT_OPENCODE_BINARY instead.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestPinnedOpenCodeProtocolSmokeWithoutModelCalls(t *testing.T) {
	binary := os.Getenv("OCTOMUS_OPENCODE_SMOKE_BINARY")
	if binary == "" {
		t.Skip("OCTOMUS_OPENCODE_SMOKE_BINARY is not set")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// The provider endpoint is unreachable by construction: session lifecycle
	// must never depend on it answering.
	configPath := filepath.Join(root, "smoke.json")
	policy, err := json.Marshal(map[string]any{
		"enabled_providers": []string{"smoke"},
		"provider": map[string]any{
			"smoke": map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "Smoke provider",
				"options": map[string]any{
					"baseURL": "http://127.0.0.1:9/v1",
					"apiKey":  "unused-smoke-placeholder",
				},
				"models": map[string]any{
					"smoke-model": map[string]any{
						"name":      "Smoke model",
						"limit":     map[string]any{"context": 8192, "output": 1024},
						"tool_call": true,
						"variants":  map[string]any{"high": map[string]any{"temperature": 0.1}},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	// The CLI gets only temporary XDG directories, the synthetic provider
	// configuration, and the adapter's own policy environment.
	wrapper := filepath.Join(root, "opencode")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys
root=%s
env={k:v for k,v in os.environ.items() if k in ['PATH','LANG','OPENCODE_SERVER_USERNAME','OPENCODE_SERVER_PASSWORD','OPENCODE_CONFIG_CONTENT','OPENCODE_DISABLE_PROJECT_CONFIG','OPENCODE_DISABLE_AUTOUPDATE','OPENCODE_DISABLE_AUTOCOMPACT','OPENCODE_DISABLE_TERMINAL_TITLE']}
env['HOME']=root
for key in ['XDG_CONFIG_HOME','XDG_DATA_HOME','XDG_STATE_HOME','XDG_CACHE_HOME']:
 env[key]=root+'/'+key
env['OPENCODE_CONFIG']=%s
env['OPENCODE_DISABLE_MODELS_FETCH']='true'
binary=%s
os.execve(binary,[binary]+sys.argv[1:],env)
`, pyString(root), pyString(configPath), pyString(binary))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CodexBinary = "/no-codex-installed"
	cfg.OpencodeBinary = wrapper
	cfg.CommandTimeoutSeconds = 60
	cfg.SessionTimeoutSeconds = 60
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := Connect(owner, config.BackendOpencode, cfg, workspace, state, "smoke")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	server, ok := client.(*OpenCode)
	if !ok || server.Version() != OpenCodeProtocolVersion {
		t.Fatalf("pinned protocol baseline %s, got %v", OpenCodeProtocolVersion, client)
	}
	models, err := client.Models(workspace)
	if err != nil {
		t.Fatalf("model catalog discovery against the dead provider: %v", err)
	}
	selected := config.Route{
		Backend:  config.BackendOpencode,
		Model:    "smoke-model",
		Provider: stringPtr("smoke"),
		Variant:  stringPtr("high"),
	}
	if err := ValidateRoute(selected, models); err != nil {
		t.Fatalf("route validation against the discovered catalog: %v", err)
	}
	session, err := client.Start(selected, workspace, nil)
	if err != nil {
		t.Fatalf("session start: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	client, err = Connect(owner, config.BackendOpencode, cfg, workspace, state, "smoke")
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer client.Close()
	resumed, err := client.Start(selected, workspace, &session)
	if err != nil {
		t.Fatalf("session resume: %v", err)
	}
	if resumed != session {
		t.Fatalf("resume changed session identity: %q", resumed)
	}
	events, err := state.Events(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("smoke check started a turn: %+v", events)
	}
}
