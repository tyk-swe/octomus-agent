package runner

// Pinned clients use a synthetic loopback provider. The tests skip unless a
// pinned binary is provided through OCTOMUS_CONTRACT_*_BINARY.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/schemas"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func contract(t *testing.T, backend config.Backend, binary string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := filepath.Join(repoRoot(t), "tests", "fixtures", "provider.py")
	cmd := process.Command("python3", root)
	cmd.Args = append(cmd.Args, provider, root)
	if err := cmd.Start(); err != nil {
		t.Fatalf("the synthetic provider did not start: %v", err)
	}
	owned := process.NewGroupChild(cmd)
	providerWait := make(chan error, 1)
	go func() { providerWait <- cmd.Wait() }()
	t.Cleanup(func() {
		owned.Close()
		select {
		case err := <-providerWait:
			if err != nil && !strings.Contains(err.Error(), "signal: killed") {
				t.Errorf("the synthetic provider exited abnormally: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the synthetic provider was not reaped")
		}
	})
	if !waitUntil(5*time.Second, func() bool { return fileExists(filepath.Join(root, "provider-port")) }) {
		t.Fatal("the synthetic provider never published its port")
	}
	port, err := os.ReadFile(filepath.Join(root, "provider-port"))
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("http://127.0.0.1:%s/v1", strings.TrimSpace(string(port)))
	providerConfig := filepath.Join(root, "provider.json")
	policy, err := json.Marshal(map[string]any{
		"enabled_providers": []string{"contract"},
		"provider": map[string]any{
			"contract": map[string]any{
				"npm":  "@ai-sdk/openai-compatible",
				"name": "Contract",
				"options": map[string]any{
					"baseURL": base,
					"apiKey":  "unused-contract-placeholder",
				},
				"models": map[string]any{
					"contract-model": map[string]any{
						"name":      "Contract model",
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
	if err := os.WriteFile(providerConfig, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex-home")
	if err := os.Mkdir(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	configToml := fmt.Sprintf("model_provider = \"contract\"\n[model_providers.contract]\nname = \"Contract\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\n", base)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(configToml), 0o600); err != nil {
		t.Fatal(err)
	}
	// Isolate only the child environment. Never read or modify operator
	// account state.
	wrapper := filepath.Join(root, "client")
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys
root=%s
env={k:v for k,v in os.environ.items() if k in ['PATH','LANG','OPENCODE_SERVER_USERNAME','OPENCODE_SERVER_PASSWORD','OPENCODE_CONFIG_CONTENT','OPENCODE_DISABLE_PROJECT_CONFIG','OPENCODE_DISABLE_AUTOUPDATE','OPENCODE_DISABLE_AUTOCOMPACT','OPENCODE_DISABLE_TERMINAL_TITLE']}
env['HOME']=root
env['CODEX_HOME']=root+'/codex-home'
for key in ['XDG_CONFIG_HOME','XDG_DATA_HOME','XDG_STATE_HOME','XDG_CACHE_HOME']:
 env[key]=root+'/'+key
env['OPENCODE_CONFIG']=%s
env['OPENCODE_DISABLE_MODELS_FETCH']='true'
binary=%s
os.execve(binary,[binary]+sys.argv[1:],env)
`, pyString(root), pyString(providerConfig), pyString(binary))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	version, err := process.RunMachine(ctx, wrapper, []string{"--version"}, root, 60)
	if err != nil {
		t.Fatalf("the pinned client did not report a version: %v", err)
	}
	expected := OpenCodeProtocolVersion
	if backend == config.BackendCodex {
		expected = "codex-cli " + CodexTestedVersion
	}
	if strings.TrimSpace(version) != expected {
		t.Fatalf("pinned contract requires %s, got %s", expected, strings.TrimSpace(version))
	}
	if backend == config.BackendCodex {
		generated := filepath.Join(root, "schemas")
		if _, err := process.RunMachine(ctx, wrapper,
			[]string{"app-server", "generate-json-schema", "--out", generated}, root, 60); err != nil {
			t.Fatalf("generate-json-schema failed: %v", err)
		}
		for _, name := range []string{
			"ThreadStartParams", "ThreadResumeParams", "TurnStartParams", "TurnInterruptParams",
		} {
			file := filepath.Join(generated, "v2", name+".json")
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("missing generated contract %s", file)
			}
			var schema map[string]any
			if err := json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			if _, ok := schema["properties"].(map[string]any); !ok {
				t.Fatalf("generated request schema changed for %s", name)
			}
		}
	}
	cfg := config.Default()
	cfg.CodexBinary = wrapper
	cfg.OpencodeBinary = wrapper
	cfg.SessionTimeoutSeconds = 30
	cfg.CommandTimeoutSeconds = 60
	route := config.NewRoute("gpt-6-astra", "low")
	if backend == config.BackendOpencode {
		route = config.Route{
			Backend:  backend,
			Model:    "contract-model",
			Provider: stringPtr("contract"),
			Variant:  stringPtr("high"),
		}
	}
	state, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	owner, cancel := context.WithCancel(ctx)
	defer cancel()
	client, err := Connect(owner, backend, cfg, workspace, state, "contract")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()
	if server, ok := client.(*OpenCode); ok {
		spec, err := server.ProtocolSchema(workspace)
		if err != nil {
			t.Fatalf("protocol schema: %v", err)
		}
		object, _ := asObject(spec)
		openapi, _ := strAt(object, "openapi")
		if !strings.HasPrefix(openapi, "3.") {
			t.Fatalf("missing OpenAPI schema: %v", object["openapi"])
		}
		paths, _ := asObject(object["paths"])
		for _, path := range []string{"/session", "/session/{sessionID}/message", "/session/{sessionID}/abort"} {
			entry, _ := asObject(paths[path])
			if _, ok := entry["post"].(map[string]any); !ok {
				t.Fatalf("pinned OpenCode request contract missing %s", path)
			}
		}
	}
	session, err := client.Start(route, workspace, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if backend == config.BackendCodex {
		if _, err := client.Start(route, workspace, &session); err == nil || !strings.Contains(err.Error(), "no rollout found") {
			t.Fatalf("pinned Codex unexpectedly resumed a thread before its first turn: %v", err)
		}
	}
	answer, err := client.Turn(session, route, workspace, "Return the controlled contract result.", nil)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if !strings.Contains(answer, "Controlled contract result") {
		t.Fatalf("missing completed turn result: %q", answer)
	}
	structured, err := client.Turn(session, route, workspace, "Return the controlled review result.", schemas.ReviewSchema())
	if err != nil {
		t.Fatalf("structured turn: %v", err)
	}
	var value any
	if err := json.Unmarshal([]byte(structured), &value); err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate(value, schemas.ReviewSchema()); err != nil {
		t.Fatalf("structured result failed validation: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	client, err = Connect(owner, backend, cfg, workspace, state, "contract")
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer client.Close()
	resumed, err := client.Start(route, workspace, &session)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed != session {
		t.Fatalf("resume changed session identity: %q", resumed)
	}
	turn := turnIn(client, session, route, workspace, "CANCEL_CONTRACT_TURN", nil)
	if !waitUntil(10*time.Second, func() bool { return fileExists(filepath.Join(root, "turn-entered")) }) {
		t.Fatal("controlled cancellation request never reached the provider")
	}
	cancel()
	result, err := await(turn, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.err == nil {
		t.Fatal("cancelled turn returned successful evidence")
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPinnedCodexContract(t *testing.T) {
	binary := os.Getenv("OCTOMUS_CONTRACT_CODEX_BINARY")
	if binary == "" {
		t.Skip("OCTOMUS_CONTRACT_CODEX_BINARY is not set")
	}
	contract(t, config.BackendCodex, binary)
}

func TestPinnedOpenCodeContract(t *testing.T) {
	binary := os.Getenv("OCTOMUS_CONTRACT_OPENCODE_BINARY")
	if binary == "" {
		t.Skip("OCTOMUS_CONTRACT_OPENCODE_BINARY is not set")
	}
	contract(t, config.BackendOpencode, binary)
}
