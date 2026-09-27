package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/evidence"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestCurrentCLIContract(t *testing.T) {
	env := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--help"}, "--data-dir"},
		{[]string{"--version"}, "octomus-agent"},
		{[]string{"--print-config"}, "\"verification_commands\""},
	} {
		var out, err bytes.Buffer
		if code := run(tc.args, env, &out, &err); code != 0 || err.Len() != 0 || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", tc.args, code, out.String(), err.String())
		}
	}
	var out, err bytes.Buffer
	if code := run([]string{"--unknown"}, env, &out, &err); code == 0 || out.Len() != 0 || err.Len() == 0 {
		t.Fatalf("unknown flag: code=%d stdout=%q stderr=%q", code, out.String(), err.String())
	}
}

// The listen address is parsed once, from the flag or the environment, and
// startup reuses that address to decide whether to warn about exposure.
func TestListenAddressIsParsedOnceFromFlagOrEnvironment(t *testing.T) {
	envWith := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		}
	}
	for _, tc := range []struct {
		name     string
		args     []string
		env      map[string]string
		listen   string
		loopback bool
		port     uint16
	}{
		{"default", nil, nil, "127.0.0.1:4200", true, 4200},
		{"flag", []string{"--listen", "127.0.0.1:0"}, nil, "127.0.0.1:0", true, 0},
		{"flag with numeric scope", []string{"--listen=[::1%1]:9"}, nil, "[::1%1]:9", true, 9},
		{"environment", nil, map[string]string{"OCTOMUS_LISTEN": "0.0.0.0:4300"}, "0.0.0.0:4300", false, 4300},
		// Only the final value is validated, so a flag replaces a bad environment value.
		{"flag over environment", []string{"--listen", "[::1]:4400"}, map[string]string{"OCTOMUS_LISTEN": "not an address"}, "[::1]:4400", true, 4400},
	} {
		parsed, display, err := parse(tc.args, envWith(tc.env))
		if err != nil || display != "" {
			t.Fatalf("%s: display=%q err=%v", tc.name, display, err)
		}
		if parsed.listen != tc.listen || !parsed.listenAddr.IsValid() ||
			parsed.listenAddr.Addr().IsLoopback() != tc.loopback || parsed.listenAddr.Port() != tc.port {
			t.Fatalf("%s: listen=%q address=%v", tc.name, parsed.listen, parsed.listenAddr)
		}
	}
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"interface scope", []string{"--listen", "[::1%eth0]:1"}, nil,
			`invalid value "[::1%eth0]:1" for '--listen': invalid socket address syntax`},
		{"missing port", []string{"--listen", "127.0.0.1"}, nil,
			`invalid value "127.0.0.1" for '--listen': invalid socket address syntax`},
		{"environment", nil, map[string]string{"OCTOMUS_LISTEN": "localhost:4200"},
			`invalid value "localhost:4200" for '--listen': invalid socket address syntax`},
		// A bad flag is reported where it appears, before later arguments.
		{"flag before unknown argument", []string{"--listen", "", "--unknown"}, nil,
			`invalid value "" for '--listen': invalid socket address syntax`},
	} {
		if _, _, err := parse(tc.args, envWith(tc.env)); err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
	}
}

func TestServiceStartupRequiresOperatorToken(t *testing.T) {
	directory := t.TempDir() + "/service"
	// Startup prepares the data directory and database before token validation.
	var out, err bytes.Buffer
	code := run([]string{"--data-dir", directory}, func(string) (string, bool) { return "", false }, &out, &err)
	if code != 1 || out.Len() != 0 || !bytes.Contains(err.Bytes(), []byte("OCTOMUS_TOKEN")) {
		t.Fatal(code, out.String(), err.String())
	}
	if _, statErr := os.Stat(filepath.Join(directory, stateDBName)); statErr != nil {
		t.Fatal("state database was not created before the token check", statErr)
	}
	// A short token is rejected with its own message.
	code = run([]string{"--data-dir", directory}, func(k string) (string, bool) {
		if k == "OCTOMUS_TOKEN" {
			return "short", true
		}
		return "", false
	}, &out, &err)
	if code != 1 || !bytes.Contains(err.Bytes(), []byte("at least 32 characters")) {
		t.Fatal(code, err.String())
	}
	// --doctor runs before the token check: an unconfigured repository is an
	// explicit diagnostic failure, not a usage error.
	for _, args := range [][]string{{"--doctor"}, {"--doctor", "--audit"}} {
		var derr bytes.Buffer
		code := run(append([]string{"--data-dir", directory}, args...), func(string) (string, bool) { return "", false }, &out, &derr)
		if code != 1 || derr.Len() == 0 {
			t.Fatal(args, code, derr.String())
		}
	}
	// Read-only exports still fail explicitly on missing state without
	// creating anything.
	empty := t.TempDir() + "/must-not-exist"
	for _, args := range [][]string{{"--usage-report"}, {"--export-run", "cycle"}} {
		var out, err bytes.Buffer
		code := run(append([]string{"--data-dir", empty}, args...), func(string) (string, bool) { return "", false }, &out, &err)
		if code != 1 || out.Len() != 0 || !bytes.Contains(err.Bytes(), []byte("state database")) {
			t.Fatal(args, code, out.String(), err.String())
		}
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("read-only export created state", err)
	}
}

// Read-only exports run before state creation, locking or workers start.
func TestReadOnlyExportsReturnBeforeTouchingApplicationState(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "state-dir")
	noEnv := func(string) (string, bool) { return "", false }
	call := func(args ...string) (int, string, string) {
		var out, err bytes.Buffer
		code := run(append([]string{"--data-dir", dataDir}, args...), noEnv, &out, &err)
		return code, out.String(), err.String()
	}

	// No saved state: an explicit failure that never creates the data directory,
	// takes the service lock or starts a worker.
	code, out, errText := call("--export-run", "cycle-a")
	if code == 0 || out != "" || !strings.Contains(errText, "state database") {
		t.Fatal(code, out, errText)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatal("export created the data directory")
	}

	// Incompatible action flags are rejected.
	code, _, errText = call("--export-run", "cycle-a", "--usage-report")
	if code == 0 || !strings.Contains(errText, "cannot be used with") {
		t.Fatal(code, errText)
	}
	for _, extra := range []string{"--doctor", "--print-config"} {
		if code, _, _ := call("--export-run", "cycle-a", extra); code == 0 {
			t.Fatalf("%s was accepted", extra)
		}
	}

	// With synthetic saved state the flags print the representation on stdout only.
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dataDir, stateDBName))
	if err != nil {
		t.Fatal(err)
	}
	completed := "2026-09-12T01:00:00Z"
	c := model.Cycle{
		ID: "cycle-a", Number: 7, Status: model.CycleCompleted,
		StartedAt: "2026-09-12T00:00:00Z", CompletedAt: &completed,
		Proposals: []model.Proposal{}, Assessments: []any{}, Sessions: []model.Session{},
		Repository: "fixture/project",
	}
	if err := s.Put("cycle", c.ID, c); err != nil {
		t.Fatal(err)
	}
	if err := s.ReserveSession(0, store.NewAdmission("cycle-a", nil, "discovery", config.NewRoute("fixture", "low"))); err != nil {
		t.Fatal(err)
	}
	code, out, errText = call("--export-run", "cycle-a")
	if code != 0 || errText != "" {
		t.Fatal(code, errText)
	}
	var exported map[string]any
	if err := json.Unmarshal([]byte(out), &exported); err != nil {
		t.Fatal(err, out)
	}
	if cycle, _ := exported["cycle"].(map[string]any); cycle["id"] != "cycle-a" || exported["schema_version"] != float64(evidence.SchemaVersion) {
		t.Fatal(out)
	}
	code, out, errText = call("--usage-report")
	if code != 0 || errText != "" {
		t.Fatal(code, errText)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err, out)
	}
	if report["has_admission_ledger"] != true || len(report["admissions"].([]any)) != 1 {
		t.Fatal(out)
	}
	if !strings.HasPrefix(out, "{\n  \"admissions\"") || !strings.HasSuffix(out, "}\n") {
		t.Fatalf("usage report is not pretty-printed JSON: %q", out[:40])
	}
	if _, err := os.Stat(filepath.Join(dataDir, "service.lock")); !os.IsNotExist(err) {
		t.Fatal("read-only export took the service lock")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
