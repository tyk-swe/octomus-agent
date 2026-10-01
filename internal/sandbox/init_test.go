package sandbox

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installRunners puts runner stand-ins on PATH, which then holds nothing else.
func installRunners(t *testing.T, scripts map[string]string) {
	t.Helper()
	bin := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
}

func versions(t *testing.T) (map[string]string, string, time.Duration) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	started := time.Now()
	if code := printVersions(&stdout, &stderr); code != 0 {
		t.Fatalf("printVersions = %d", code)
	}
	elapsed := time.Since(started)
	found := map[string]string{}
	if err := json.Unmarshal(stdout.Bytes(), &found); err != nil {
		t.Fatalf("version output %q: %v", stdout.String(), err)
	}
	return found, stderr.String(), elapsed
}

// The runners are launchers whose native child shares their stdout. A hung child must not hold the version probe past
// its own limit: the broker kills the whole probe at 120 seconds and refuses to start.
func TestPrintVersionsBoundsAHungRunnerAndItsChildren(t *testing.T) {
	versionTimeout = 500 * time.Millisecond
	t.Cleanup(func() { versionTimeout = 30 * time.Second })
	marker := filepath.Join(t.TempDir(), "survivor")
	installRunners(t, map[string]string{
		"codex":    "(sleep 3; touch " + marker + ") &\nsleep 30\n",
		"opencode": "echo 'opencode 1.18.30'\n",
	})
	found, stderr, elapsed := versions(t)
	if elapsed > 5*time.Second {
		t.Fatalf("the version probe took %s with a 500ms limit per runner", elapsed)
	}
	if _, ok := found["codex"]; ok || found["opencode"] != "opencode 1.18.30" {
		t.Fatalf("versions = %v", found)
	}
	if !strings.Contains(stderr, "codex --version failed: timed out") {
		t.Fatalf("stderr = %q; want the timeout named", stderr)
	}
	time.Sleep(3500*time.Millisecond - elapsed)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a runner's child outlived the version probe")
	}
}

// A runner that is installed but fails is reported as failing, not silently dropped as if it were missing.
func TestPrintVersionsReportsAFailingRunnerApartFromAMissingOne(t *testing.T) {
	installRunners(t, map[string]string{"codex": "echo 'state lock held' >&2\nexit 3\n"})
	found, stderr, _ := versions(t)
	if len(found) != 0 {
		t.Fatalf("versions = %v; want none", found)
	}
	if !strings.Contains(stderr, "codex --version failed: exit status 3: state lock held") {
		t.Fatalf("stderr = %q; want the failure and its stderr", stderr)
	}
	if strings.Contains(stderr, "opencode") {
		t.Fatalf("stderr = %q; a runner that is not installed is not a failure", stderr)
	}
}
