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

func TestFrozenCLIContracts(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/compatibility/cli.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name     string
			Args     []string
			Env      map[string]string
			Expected struct {
				Code           int
				Stdout, Stderr string
			}
		}
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(c.Args, func(k string) (string, bool) { v, ok := c.Env[k]; return v, ok }, &stdout, &stderr)
			if code != c.Expected.Code {
				t.Fatalf("exit %d != %d: %s", code, c.Expected.Code, stderr.String())
			}
			if code != 0 {
				if stdout.Len() != 0 || stderr.Len() == 0 {
					t.Fatal("error stream boundaries")
				}
				return
			}
			if stderr.Len() != 0 {
				t.Fatal(stderr.String())
			}
			if strings.Contains(c.Expected.Stdout, "Usage: octomus-agent [OPTIONS]") {
				for _, flag := range []string{"data-dir", "listen", "assets", "print-config", "doctor", "audit", "usage-report", "export-run", "help", "version"} {
					if !strings.Contains(stdout.String(), "--"+flag) {
						t.Errorf("help omitted --%s", flag)
					}
				}
			} else if stdout.String() != c.Expected.Stdout {
				t.Errorf("stdout differs: %s", stdout.String())
			}
		})
	}
}
func TestServiceStartupRequiresOperatorToken(t *testing.T) {
	directory := t.TempDir() + "/service"
	// The reference creates the data directory, takes the lock and opens the
	// state database before checking the operator token: a missing token exits
	// with guidance but leaves the prepared directory behind.
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

// Port of tests/evidence.rs export_run_flag_returns_before_touching_application_state
// and the usage-report half of tests/usage.rs: the read-only flags run before any
// data directory, service lock or worker exists, and print only JSON on stdout.
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
