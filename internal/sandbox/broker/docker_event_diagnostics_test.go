package broker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

func (h *dockerBroker) memoryFailureEvents(t *testing.T, started time.Time) {
	t.Helper()
	observed := time.Now().UTC()
	until := observed.Add(2 * time.Second)
	// The assertion has already failed. Observe a short diagnostic window for
	// late events without reclassifying the result or retrying the workload.
	events, complete, err := collectMemoryEvents(context.Background(), h.instance, started, until)
	t.Logf("Memory-report failure observed at %s; fixture Docker events through %s (capture_complete=%t; daemon history is limited to its last 256 events):\n%s",
		observed.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano), complete, redact.Text(events))
	if err != nil {
		t.Logf("Fixture Docker event diagnostics unavailable: %s", redact.Error(err))
	}
}

func TestDockerMemoryEventDiagnosticsUseOnlyFixtureEvents(t *testing.T) {
	h := startDockerBroker(t, nil)
	ws := h.taskRoot(t)
	started := time.Now()
	out, _, err := sandbox.Verify(context.Background(), h.remote, ws, "true", 10, true)
	if err != nil || !out.Status.Success() {
		t.Fatalf("diagnostic fixture command = %v, %v", out, err)
	}
	until := time.Now()
	events, complete, err := collectMemoryEvents(context.Background(), h.instance, started, until)
	if err != nil || !complete || !strings.Contains(events, " die ") {
		t.Fatalf("fixture event capture = %q, complete=%t, error=%v", events, complete, err)
	}
	events, complete, err = collectMemoryEvents(context.Background(), "other-"+h.instance, started, until)
	if err != nil || !complete || events != "" {
		t.Fatalf("another fixture observed events = %q, complete=%t, error=%v", events, complete, err)
	}
}

func collectMemoryEvents(ctx context.Context, instance string, since, until time.Time) (string, bool, error) {
	// Restrict observations to this synthetic fixture, and print only event
	// timing, action, container ID and exit code, never arbitrary labels.
	return process.RunTextEnv(ctx, "docker", []string{
		"events", "--since", since.UTC().Format(time.RFC3339Nano), "--until", until.UTC().Format(time.RFC3339Nano),
		"--filter", "type=container", "--filter", "label=octomus.sandbox.instance=" + instance,
		"--filter", "event=oom", "--filter", "event=die",
		"--format", `{{.TimeNano}} {{.Action}} {{.Actor.ID}} {{index .Actor.Attributes "exitCode"}}`,
	}, "", 5, nil, 16*1024)
}

func TestDockerMemoryEventDiagnosticsScopeAndCancellation(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "arguments")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OCTOMUS_TEST_DOCKER_ARGS", argsFile)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OCTOMUS_TEST_DOCKER_ARGS\"\nprintf '123 oom fixture-container\\n124 die fixture-container 137\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 1, 2, 3, 4, 5, 123, time.FixedZone("fixture", 3600))
	until := since.Add(2 * time.Second)
	events, complete, err := collectMemoryEvents(context.Background(), "test-only", since, until)
	if err != nil || !complete || !strings.Contains(events, "oom fixture-container") || !strings.Contains(events, "die fixture-container 137") {
		t.Fatalf("event diagnostics = %q, complete=%t, error=%v", events, complete, err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, pair := range [][2]string{{"--since", since.UTC().Format(time.RFC3339Nano)}, {"--until", until.UTC().Format(time.RFC3339Nano)}, {"--filter", "type=container"}, {"--filter", "label=octomus.sandbox.instance=test-only"}, {"--filter", "event=oom"}, {"--filter", "event=die"}} {
		i := slices.Index(args, pair[1])
		if i < 1 || args[i-1] != pair[0] {
			t.Fatalf("diagnostics omitted scope/window %v: %v", pair, args)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := collectMemoryEvents(ctx, "test-only", since, until); !errors.Is(err, process.ErrCancelled) {
		t.Fatalf("cancelled diagnostics = %v; want cancellation", err)
	}
}
