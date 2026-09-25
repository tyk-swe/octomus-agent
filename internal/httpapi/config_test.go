package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// settingsView decodes one settings read/update response into its parts.
func settingsView(t *testing.T, body []byte) (map[string]any, string, []map[string]any) {
	t.Helper()
	var view struct {
		Config            map[string]any   `json:"config"`
		Revision          string           `json:"revision"`
		TransformedFields []map[string]any `json:"transformed_fields"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("settings view: %v", err)
	}
	if len(view.Revision) != 64 {
		t.Fatalf("revision: %q", view.Revision)
	}
	return view.Config, view.Revision, view.TransformedFields
}

func transformKinds(t *testing.T, fields []map[string]any, field string) []any {
	t.Helper()
	for _, entry := range fields {
		if entry["field"] == field {
			kinds, _ := entry["kinds"].([]any)
			return kinds
		}
	}
	t.Fatalf("no transform entry for %s: %v", field, fields)
	return nil
}

func TestConfigAPIRevisionGatePreservesCanonicalValues(t *testing.T) {
	app, state := testApp(t)
	router := Router(app, token, "", "test")
	// A harmless command literal that trips secret scrubbing plus an overlong
	// path that trips the display bound exercise both transform kinds.
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"echo ghp_syntheticsecrettoken123", "true"}
	longPath := "/" + strings.Repeat("storage", 2500)
	cfg.RunnerStoragePaths = map[string]string{"codex": longPath}
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	revision, err := cfg.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}

	response := call(t, router, "GET", "/api/config", "")
	if response.Code != http.StatusOK {
		t.Fatalf("get config: %d %s", response.Code, response.Body.String())
	}
	display, gotRevision, fields := settingsView(t, response.Body.Bytes())
	if gotRevision != revision {
		t.Fatalf("revision %q != canonical fingerprint %q", gotRevision, revision)
	}
	commands := display["verification_commands"].([]any)
	if commands[0] != "echo [redacted]" || commands[1] != "true" {
		t.Fatalf("display commands: %v", commands)
	}
	paths := display["runner_storage_paths"].(map[string]any)
	if got := len(paths["codex"].(string)); got != 16384 {
		t.Fatalf("shortened display path: %d", got)
	}
	if kinds := transformKinds(t, fields, "verification_commands"); !reflect.DeepEqual(kinds, []any{"redacted"}) {
		t.Fatalf("command transforms: %v", kinds)
	}
	if kinds := transformKinds(t, fields, "runner_storage_paths"); !reflect.DeepEqual(kinds, []any{"shortened"}) {
		t.Fatalf("path transforms: %v", kinds)
	}
	// Reading again returns the same revision for unchanged canonical state.
	again := call(t, router, "GET", "/api/config", "")
	if _, repeat, _ := settingsView(t, again.Body.Bytes()); repeat != revision {
		t.Fatalf("unstable revision: %q != %q", repeat, revision)
	}

	// An unrelated numeric edit preserves every untouched canonical value.
	response = call(t, router, "PUT", "/api/config",
		`{"expected_revision":"`+revision+`","config":{"max_sessions_per_day":200}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("patch save: %d %s", response.Code, response.Body.String())
	}
	display, revision, fields = settingsView(t, response.Body.Bytes())
	if display["max_sessions_per_day"] != float64(200) {
		t.Fatalf("saved view: %v", display["max_sessions_per_day"])
	}
	if commands := display["verification_commands"].([]any); commands[0] != "echo [redacted]" {
		t.Fatalf("saved view commands: %v", commands)
	}
	saved, err := store.Get[config.Config](state, "settings", "config")
	if err != nil || saved == nil {
		t.Fatal(err)
	}
	if saved.VerificationCommands[0] != "echo ghp_syntheticsecrettoken123" ||
		saved.RunnerStoragePaths["codex"] != longPath ||
		saved.MaxSessionsPerDay != 200 || saved.GitHubRepo != cfg.GitHubRepo {
		t.Fatalf("canonical config: %+v", saved)
	}
	newFingerprint, err := saved.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if revision != newFingerprint {
		t.Fatalf("returned revision %q != saved fingerprint %q", revision, newFingerprint)
	}

	// A stale revision conflicts and leaves the saved record byte-identical.
	after, found, err := state.GetRaw("settings", "config")
	if err != nil || !found {
		t.Fatalf("saved config: found=%t, err=%v", found, err)
	}
	if response := call(t, router, "PUT", "/api/config",
		`{"expected_revision":"`+strings.Repeat("0", 64)+`","config":{"max_sessions_per_day":10}}`); response.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d %s", response.Code, response.Body.String())
	}
	if raw, _, _ := state.GetRaw("settings", "config"); string(raw) != string(after) {
		t.Fatal("stale save changed the saved record")
	}

	// Whole-map and whole-list replacements are exact.
	response = call(t, router, "PUT", "/api/config",
		`{"expected_revision":"`+revision+`","config":{"runner_storage_paths":{"opencode":"/o"},"verification_commands":[]}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("replacement: %d %s", response.Code, response.Body.String())
	}
	saved, _ = store.Get[config.Config](state, "settings", "config")
	if !reflect.DeepEqual(saved.RunnerStoragePaths, map[string]string{"opencode": "/o"}) || len(saved.VerificationCommands) != 0 {
		t.Fatalf("replacement did not apply exactly: %+v", saved)
	}
	_, revision, fields = settingsView(t, response.Body.Bytes())
	if len(fields) != 0 {
		t.Fatalf("transforms after clean save: %v", fields)
	}
	after, found, err = state.GetRaw("settings", "config")
	if err != nil || !found {
		t.Fatalf("saved config: found=%t, err=%v", found, err)
	}

	// Rejections: missing revision, legacy whole-config bodies, unknown fields
	// and duplicate keys all fail without touching the saved record.
	escapedKey := `{"expected_revision":"` + revision + `","config":{"runner_storage_paths":{"codex":"/a","co` + "\\u0064" + `ex":"/b"}}}`
	for name, body := range map[string]struct {
		raw    string
		status int
		want   string
	}{
		"missing revision":    {`{"config":{}}`, http.StatusUnprocessableEntity, `missing field "expected_revision"`},
		"missing config":      {`{"expected_revision":"` + revision + `"}`, http.StatusUnprocessableEntity, `missing field "config"`},
		"legacy body":         {`{"repository":"/repo","github_repo":"fixture/project"}`, http.StatusUnprocessableEntity, `unknown field "repository"`},
		"unknown top-level":   {`{"expected_revision":"` + revision + `","config":{},"bogus":1}`, http.StatusUnprocessableEntity, `unknown field "bogus"`},
		"duplicate body key":  {`{"expected_revision":"a","expected_revision":"b","config":{}}`, http.StatusUnprocessableEntity, `duplicate field "expected_revision"`},
		"unknown patch field": {`{"expected_revision":"` + revision + `","config":{"nonsense":1}}`, http.StatusUnprocessableEntity, `unknown field "nonsense"`},
		"duplicate patch key": {`{"expected_revision":"` + revision + `","config":{"runner_storage_paths":{"codex":"/a","codex":"/b"}}}`, http.StatusUnprocessableEntity, `duplicate field "codex"`},
		"escaped patch key":   {escapedKey, http.StatusUnprocessableEntity, `duplicate field "codex"`},
		"wrong patch type":    {`{"expected_revision":"` + revision + `","config":{"max_retries":"high"}}`, http.StatusUnprocessableEntity, "max_retries"},
		"null patch value":    {`{"expected_revision":"` + revision + `","config":{"roles":null}}`, http.StatusUnprocessableEntity, "null is not allowed"},
	} {
		t.Run(name, func(t *testing.T) {
			response := call(t, router, "PUT", "/api/config", body.raw)
			if response.Code != body.status || !strings.Contains(response.Body.String(), body.want) {
				t.Fatalf("%d %s; want %d containing %q", response.Code, response.Body.String(), body.status, body.want)
			}
			if got := response.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Fatalf("content type: %q", got)
			}
			if raw, _, _ := state.GetRaw("settings", "config"); string(raw) != string(after) {
				t.Fatal("rejected save changed the saved record")
			}
		})
	}

	// A canonical saved record still loads when an existing version-7 database is reopened.
	reopened, err := store.Open(state.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := store.Get[config.Config](reopened, "settings", "config")
	if err != nil || loaded == nil || !reflect.DeepEqual(*loaded, *saved) {
		t.Fatalf("reopened config: %v, err=%v", loaded, err)
	}
}

// TestConfigAPIDisplayIdentityCollision proves two different canonical
// configurations that transform to identical display values still carry
// distinct revisions, so display text can never stand in for identity.
func TestConfigAPIDisplayIdentityCollision(t *testing.T) {
	app, state := testApp(t)
	router := Router(app, token, "", "test")
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"echo ghp_firstsyntheticvalue"}
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	response := call(t, router, "GET", "/api/config", "")
	first, firstRevision, _ := settingsView(t, response.Body.Bytes())

	other := cfg.Clone()
	other.VerificationCommands = []string{"echo ghp_secondsyntheticvalue"}
	if err := state.Put("settings", "config", other); err != nil {
		t.Fatal(err)
	}
	response = call(t, router, "GET", "/api/config", "")
	second, secondRevision, _ := settingsView(t, response.Body.Bytes())

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("displays must collide: %v vs %v", first["verification_commands"], second["verification_commands"])
	}
	if firstRevision == secondRevision {
		t.Fatal("canonical revisions must differ")
	}
	// A baseline start or save pinned to the first revision now conflicts.
	if response := call(t, router, "POST", "/api/baseline-checks",
		fmt.Sprintf(`{"expected_revision":%q}`, firstRevision)); response.Code != http.StatusConflict {
		t.Fatalf("stale baseline revision: %d", response.Code)
	}
	if response := call(t, router, "PUT", "/api/config",
		fmt.Sprintf(`{"expected_revision":%q,"config":{"max_retries":1}}`, firstRevision)); response.Code != http.StatusConflict {
		t.Fatalf("stale save revision: %d", response.Code)
	}
}
