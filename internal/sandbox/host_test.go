package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// script writes an executable runner stand-in. Tests that write one run sequentially: a parallel fork can hold the
// write descriptor open long enough to make exec fail with "text file busy".
func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runner")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHostVerifyRunsBashPipefailInTheWorkspace(t *testing.T) {
	dir := t.TempDir()
	out, err := sandbox.Verify(context.Background(), sandbox.Host{}, dir, "pwd; false | true", 30, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out.Stdout.Bytes)); got != dir {
		t.Fatalf("verification cwd = %q; want %q", got, dir)
	}
	if out.Status.Success() {
		t.Fatal("pipefail must fail a pipeline whose first command fails")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sandbox.Verify(ctx, sandbox.Host{}, dir, "true", 30, true); !errors.Is(err, process.ErrCancelled) {
		t.Fatalf("cancelled verification = %v; want ErrCancelled", err)
	}
}

func TestHostRunnerStreamsAndStderrSink(t *testing.T) {
	binary := script(t, "echo \"$1 $2 $3\"; echo diagnostic >&2; cat")
	sink := &syncBuffer{}
	child, err := sandbox.Host{}.Start(context.Background(), sandbox.Spec{
		Kind: sandbox.KindRunner, Runner: config.BackendCodex, Binary: binary, Dir: t.TempDir(), Stdin: true, Stderr: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Stdin().Write([]byte("echoed\n")); err != nil {
		t.Fatal(err)
	}
	child.Stdin().Close()
	var stdout bytes.Buffer
	if _, err := stdout.ReadFrom(child.Stdout()); err != nil {
		t.Fatal(err)
	}
	status, err := child.Wait()
	if err != nil || !status.Success() {
		t.Fatalf("wait = %v, %v", status, err)
	}
	if stdout.String() != "app-server --listen stdio://\nechoed\n" {
		t.Fatalf("stdout = %q; want the fixed Codex arguments and the echoed stdin", stdout.String())
	}
	if sink.String() != "diagnostic\n" {
		t.Fatalf("stderr sink = %q", sink.String())
	}
}

func TestHostRunnerStartFailureIsDistinguishable(t *testing.T) {
	_, err := sandbox.Host{}.Start(context.Background(), sandbox.Spec{
		Kind: sandbox.KindRunner, Runner: config.BackendCodex, Binary: filepath.Join(t.TempDir(), "missing"), Dir: t.TempDir(),
	})
	var notStarted *sandbox.StartError
	if !errors.As(err, &notStarted) {
		t.Fatalf("missing runner = %v; want a StartError", err)
	}
	if _, err := (sandbox.Host{}).Start(context.Background(), sandbox.Spec{Kind: sandbox.KindProbe, Dir: t.TempDir()}); err == nil {
		t.Fatal("the host backend has no probe sandbox and must refuse one")
	}
}

func TestHostOpenCodeReadiness(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"exits early":      {body: "echo starting; exit 1", want: "OpenCode exited before server readiness"},
		"foreign address":  {body: "echo 'opencode server listening on http://0.0.0.0:4096'; sleep 5", want: "OpenCode did not bind to a local server address"},
		"never ready":      {body: "sleep 5", want: "OpenCode startup timed out"},
		"too much output":  {body: "i=0; while [ $i -lt 1001 ]; do echo noise; i=$((i+1)); done; sleep 5", want: "OpenCode exceeded the startup output limit"},
		"line over bound":  {body: "head -c 20000 /dev/zero | tr '\\0' x; echo; sleep 5", want: "line exceeds the 16384 byte protocol limit"},
		"listening server": {body: "echo 'opencode server listening on http://127.0.0.1:4096'; sleep 5"},
	} {
		t.Run(name, func(t *testing.T) {
			server, err := sandbox.Host{}.StartOpenCode(context.Background(), sandbox.Spec{
				Kind: sandbox.KindRunner, Runner: config.BackendOpencode, Binary: script(t, tc.body), Dir: t.TempDir(),
			}, 1)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				defer server.Child.Kill()
				if server.Base != "http://127.0.0.1:4096" {
					t.Fatalf("base = %q", server.Base)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readiness = %v; want %q", err, tc.want)
			}
		})
	}
}
