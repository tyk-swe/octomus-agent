package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/export"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
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

	// Display commands need neither valid deployment defaults nor helper programs; flags override
	// a mistyped environment; a mistyped flag wins over help; nothing writes state.
	dataDir := filepath.Join(t.TempDir(), "state")
	mistyped := func(listen bool) func(string) (string, bool) {
		return func(key string) (string, bool) {
			switch key {
			case "OCTOMUS_SANDBOX":
				return "mistyped", true
			case "OCTOMUS_LISTEN":
				return "mistyped", listen
			case "OCTOMUS_DATA_DIR":
				return dataDir, true
			}
			return "", false
		}
	}
	for _, tc := range []struct {
		args   []string
		listen bool
		want   string
	}{
		{[]string{"--help"}, true, "--sandbox"},
		{[]string{"-h"}, true, "--sandbox"},
		{[]string{"--version"}, true, "octomus-agent"},
		{[]string{"-V"}, true, "octomus-agent"},
		{[]string{"--sandbox", "docker", "--print-config"}, false, "\"verification_commands\""},
		{[]string{"--sandbox=off", "--print-config"}, false, "\"verification_commands\""},
	} {
		var out, err bytes.Buffer
		if code := run(tc.args, mistyped(tc.listen), &out, &err); code != 0 || err.Len() != 0 || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%v with a mistyped environment: code=%d stdout=%q stderr=%q", tc.args, code, out.String(), err.String())
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "for OCTOMUS_SANDBOX"},
		{[]string{"--print-config"}, "for OCTOMUS_SANDBOX"},
		{[]string{"--sandbox=mistyped", "--help"}, "for '--sandbox'"},
		{[]string{"--sandbox=mistyped", "--version"}, "for '--sandbox'"},
	} {
		var out, err bytes.Buffer
		if code := run(tc.args, mistyped(false), &out, &err); code != 2 || out.Len() != 0 || !strings.Contains(err.String(), tc.want) {
			t.Fatalf("%v with a mistyped environment: code=%d stdout=%q stderr=%q", tc.args, code, out.String(), err.String())
		}
	}
	if _, statErr := os.Stat(dataDir); !os.IsNotExist(statErr) {
		t.Fatal("CLI display or validation created state", statErr)
	}
}

func TestServiceStartupRequiresOperatorToken(t *testing.T) {
	directory := t.TempDir() + "/service"
	var out, err bytes.Buffer
	code := run([]string{"--data-dir", directory}, func(string) (string, bool) { return "", false }, &out, &err)
	if code != 1 || out.Len() != 0 || !bytes.Contains(err.Bytes(), []byte("OCTOMUS_TOKEN")) {
		t.Fatal(code, out.String(), err.String())
	}
	if _, statErr := os.Stat(filepath.Join(directory, stateDBName)); statErr != nil {
		t.Fatal("state database was not created before the token check", statErr)
	}
	code = run([]string{"--data-dir", directory}, func(k string) (string, bool) {
		if k == "OCTOMUS_TOKEN" {
			return "short", true
		}
		return "", false
	}, &out, &err)
	if code != 1 || !bytes.Contains(err.Bytes(), []byte("at least 32 characters")) {
		t.Fatal(code, err.String())
	}
	for _, args := range [][]string{{"--doctor"}, {"--doctor", "--audit"}} {
		var derr bytes.Buffer
		code := run(append([]string{"--data-dir", directory}, args...), func(string) (string, bool) { return "", false }, &out, &derr)
		if code != 1 || derr.Len() == 0 {
			t.Fatal(args, code, derr.String())
		}
	}
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

func TestReadOnlyExportsReturnBeforeTouchingApplicationState(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "state-dir")
	noEnv := func(string) (string, bool) { return "", false }
	call := func(args ...string) (int, string, string) {
		var out, err bytes.Buffer
		code := run(append([]string{"--data-dir", dataDir}, args...), noEnv, &out, &err)
		return code, out.String(), err.String()
	}

	code, out, errText := call("--export-run", "cycle-a")
	if code == 0 || out != "" || !strings.Contains(errText, "state database") {
		t.Fatal(code, out, errText)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatal("export created the data directory")
	}

	code, _, errText = call("--export-run", "cycle-a", "--usage-report")
	if code == 0 || !strings.Contains(errText, "cannot be used with") {
		t.Fatal(code, errText)
	}
	for _, extra := range []string{"--doctor", "--print-config"} {
		if code, _, _ := call("--export-run", "cycle-a", extra); code == 0 {
			t.Fatalf("%s was accepted", extra)
		}
	}

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
	if cycle, _ := exported["cycle"].(map[string]any); cycle["id"] != "cycle-a" || exported["schema_version"] != float64(export.SchemaVersion) {
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

func TestGoldenExportsReadBack(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), ".octomus")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	testutil.GoldenState(t, "0.1.0", filepath.Join(dataDir, stateDBName))
	noEnv := func(string) (string, bool) { return "", false }
	call := func(args ...string) (int, map[string]any, string) {
		var out, err bytes.Buffer
		code := run(append([]string{"--data-dir", dataDir}, args...), noEnv, &out, &err)
		var value map[string]any
		if code == 0 {
			if err := json.Unmarshal(out.Bytes(), &value); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
		}
		return code, value, err.String()
	}
	for _, args := range [][]string{{"--usage-report"}, {"--export-run", "4306e9ae-0833-47e5-b569-14b7b1d7d4f8"}} {
		if code, _, errText := call(args...); code != 1 || !strings.Contains(errText, "must be upgraded") {
			t.Fatalf("old read-only export: code=%d error=%s", code, errText)
		}
	}
	// The service-owned upgrade precedes read-only exports of historical state.
	s, err := store.Open(filepath.Join(dataDir, stateDBName))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cycle  string
		number float64
		status string
	}{
		{"4306e9ae-0833-47e5-b569-14b7b1d7d4f8", 1, "completed"},
		{"0346c423-2759-4071-bd68-a87ed546ebcb", 2, "failed"},
	} {
		code, value, errText := call("--export-run", tc.cycle)
		if code != 0 {
			t.Fatalf("--export-run %s: code=%d stderr=%q", tc.cycle, code, errText)
		}
		if value["schema_version"] != float64(1) || value["kind"] != "recorded_review_check_evidence" {
			t.Fatalf("%s: %v", tc.cycle, value)
		}
		cycle, ok := value["cycle"].(map[string]any)
		if !ok || cycle["id"] != tc.cycle || cycle["number"] != tc.number || cycle["status"] != tc.status {
			t.Fatalf("%s: %v", tc.cycle, cycle)
		}
		planning, ok := cycle["planning"].(map[string]any)
		if !ok || planning["proposal_count"] != float64(3) {
			t.Fatalf("%s planning: %v", tc.cycle, planning)
		}
	}
	code, value, errText := call("--usage-report")
	if code != 0 {
		t.Fatalf("--usage-report: code=%d stderr=%q", code, errText)
	}
	if len(value["admissions"].([]any)) != 44 || len(value["cycles"].([]any)) != 2 || len(value["tasks"].([]any)) != 3 {
		t.Fatalf("usage report: %v %v %v", len(value["admissions"].([]any)), len(value["cycles"].([]any)), len(value["tasks"].([]any)))
	}
	if value["has_admission_ledger"] != true {
		t.Fatalf("usage report: %v", value["has_admission_ledger"])
	}
	if _, err := os.Stat(filepath.Join(dataDir, "service.lock")); !os.IsNotExist(err) {
		t.Fatal("read-only export created service.lock", err)
	}
}
