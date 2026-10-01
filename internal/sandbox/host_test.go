package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

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
	out, _, err := sandbox.Verify(context.Background(), sandbox.Host{}, dir, "pwd; false | true", 30, true)
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
	if _, _, err := sandbox.Verify(ctx, sandbox.Host{}, dir, "true", 30, true); !errors.Is(err, process.ErrCancelled) {
		t.Fatalf("cancelled verification = %v; want ErrCancelled", err)
	}
}

func TestHostRunnerStreamsAndStderrSink(t *testing.T) {
	binary := script(t, "echo \"$1 $2 $3\"; echo diagnostic >&2; cat")
	sink := &testutil.SyncBuffer{}
	child, err := sandbox.Host{}.Start(context.Background(), sandbox.Spec{
		Kind: sandbox.KindRunner, Runner: config.BackendCodex, Binary: binary, Dir: t.TempDir(), Stdin: true, Stderr: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Kill()
		child.Stdout().Close()
		child.Stderr().Close()
	})
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
	var data [1]byte
	if _, err := child.Stderr().Read(data[:]); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("copied stderr remains open after Wait: %v", err)
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

func TestParseLoopbackURL(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"http://127.0.0.1:4096", "http://127.0.0.1:4096/"} {
		got, err := sandbox.ParseLoopbackURL(endpoint)
		if err != nil || got != "http://127.0.0.1:4096" {
			t.Errorf("ParseLoopbackURL(%q) = %q, %v", endpoint, got, err)
		}
	}
	for _, endpoint := range []string{
		"",
		"127.0.0.1:4096",
		"https://127.0.0.1:4096/",
		"ws://127.0.0.1:4096/",
		"http://localhost:4096/",
		"http://10.0.0.1:4096/",
		"http://[::1]:4096/",
		"http://127.0.0.1/",
		"http://127.0.0.1:0/",
		"http://127.0.0.1:65536/",
		"http://127.0.0.1:80/",
		"http://user@127.0.0.1:4096/",
		"http://user:pw@127.0.0.1:4096/",
		"http://@127.0.0.1:4096/",
		"http://127.0.0.1:4096/x",
		"http://127.0.0.1:4096/?q",
		"http://127.0.0.1:4096?q",
		"http://127.0.0.1:4096/#f",
		"http://127.0.0.1:4096#f",
	} {
		if got, err := sandbox.ParseLoopbackURL(endpoint); err == nil || got != "" {
			t.Errorf("ParseLoopbackURL(%q) = %q, %v; want a refusal", endpoint, got, err)
		}
	}
}
