package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestConfigAPIDuplicateDecodedMapKeysLeaveSavedConfigIntact(t *testing.T) {
	app, state := testApp(t)
	router := Router(app, token, "", "test")
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := string(data)
	const emptyPaths = `"runner_storage_paths":{}`
	if !strings.Contains(base, emptyPaths) {
		t.Fatalf("missing map in config JSON: %s", base)
	}
	distinct := strings.Replace(base, emptyPaths, `"runner_storage_paths":{"codex":"/storage/codex","opencode":"/storage/opencode"}`, 1)
	if response := call(t, router, "PUT", "/api/config", distinct); response.Code != http.StatusOK {
		t.Fatalf("distinct keys: %d %s", response.Code, response.Body.String())
	}
	expected := cfg.Clone()
	expected.RunnerStoragePaths = map[string]string{"codex": "/storage/codex", "opencode": "/storage/opencode"}
	before, found, err := state.GetRaw("settings", "config")
	if err != nil || !found {
		t.Fatalf("saved config: found=%t, err=%v", found, err)
	}

	role, err := json.Marshal(cfg.Roles["discovery"])
	if err != nil {
		t.Fatal(err)
	}
	tier, err := json.Marshal(cfg.Tiers["XS"])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, old, replacement, duplicate string
	}{
		{"repeated storage key", emptyPaths, `"runner_storage_paths":{"codex":"/storage/first","codex":"/storage/second"}`, "codex"},
		{"escaped storage key", emptyPaths, `"runner_storage_paths":{"codex":"/storage/first","co\u0064ex":"/storage/second"}`, "codex"},
		{"repeated role key", `"roles":{`, `"roles":{"discovery":` + string(role) + `,`, "discovery"},
		{"escaped tier key", `"tiers":{`, `"tiers":{"\u0058S":` + string(tier) + `,`, "XS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(base, tc.old) {
				t.Fatalf("missing %q in config JSON", tc.old)
			}
			body := strings.Replace(base, tc.old, tc.replacement, 1)
			response := call(t, router, "PUT", "/api/config", body)
			if response.Code != http.StatusUnprocessableEntity || !strings.HasPrefix(response.Body.String(), "Failed to deserialize the JSON body into the target type: ") || !strings.Contains(response.Body.String(), `duplicate field "`+tc.duplicate+`"`) {
				t.Fatalf("duplicate key: %d %s", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Fatalf("content type: %q", got)
			}
			after, found, err := state.GetRaw("settings", "config")
			if err != nil || !found || string(after) != string(before) {
				t.Fatalf("saved config changed: found=%t, err=%v", found, err)
			}
		})
	}
	response := call(t, router, "GET", "/api/config", "")
	if response.Code != http.StatusOK {
		t.Fatalf("get config: %d %s", response.Code, response.Body.String())
	}
	var fromAPI config.Config
	if err := json.Unmarshal(response.Body.Bytes(), &fromAPI); err != nil || !reflect.DeepEqual(fromAPI, expected) {
		t.Fatalf("API config: %v, err=%v", fromAPI, err)
	}

	// A canonical saved record still loads when an existing version-7 database is reopened.
	reopened, err := store.Open(state.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := store.Get[config.Config](reopened, "settings", "config")
	if err != nil || loaded == nil || !reflect.DeepEqual(*loaded, expected) {
		t.Fatalf("reopened config: %v, err=%v", loaded, err)
	}
}
