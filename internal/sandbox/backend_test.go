// Both backends every untrusted child starts through: the host for --sandbox off and the broker-backed remote.

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestHostVerify(t *testing.T) {
	dir := t.TempDir()
	out, _, err := Verify(context.Background(), Host{}, dir, "pwd; false | true", 30, true)
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
	if _, _, err := Verify(ctx, Host{}, dir, "true", 30, true); !errors.Is(err, process.ErrCancelled) {
		t.Fatalf("cancelled verification = %v; want ErrCancelled", err)
	}
}

// fakeBroker speaks the broker protocol with scripted sandboxes chosen by the verification command.
type fakeBroker struct {
	socket   string
	requests chan wire.Request
}

func startFakeBroker(t *testing.T, max int) *fakeBroker {
	t.Helper()
	f := &fakeBroker{requests: make(chan wire.Request, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.InfoPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.BrokerInfo{Image: "sandbox:test", Limits: wire.BrokerLimits{Max: max},
			Runners: map[string]string{"codex": "codex-cli 0.153.4"}})
	})
	mux.HandleFunc("POST "+wire.SandboxesPath, func(w http.ResponseWriter, r *http.Request) {
		var req wire.Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.requests <- req
		if req.Command == "refuse" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"Sandbox directory is not an owned root"}`)
			return
		}
		conn, stream := testutil.SwitchProtocols(t, w, wire.UpgradeProtocol)
		if conn == nil {
			return
		}
		defer conn.Close()
		f.serve(req, conn, stream.Reader)
	})
	f.socket = testutil.UnixHTTPServer(t, mux)
	return f
}

func exitFrame(out *wire.FrameWriter, report wire.ExitReport) {
	payload, _ := json.Marshal(report)
	_ = out.Frame(wire.FrameExit, payload)
}

// startFailure is how the broker reports a container it created but could not start, after the upgrade.
const startFailure = "Starting the sandbox: Error response from daemon: unknown or invalid runtime name: runsc"

// removeFailure is how the broker reports a sandbox it could not remove, whatever ended it.
const removeFailure = "Removing the sandbox failed: container is stuck"

// killedEvidence is what the broker hands back about a killed sandbox: refused hosts exist nowhere else.
func killedEvidence() wire.ExitReport {
	return wire.ExitReport{Code: 137, Killed: true, Sandbox: &model.SandboxRecord{ImageID: "sha256:sandbox", Runs: 1,
		Egress: model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{"exfil.example.net:443": 40}}}}
}

// awaitKill reads control frames until the client asks for a kill, and reports whether it did.
func awaitKill(reader *bufio.Reader) bool {
	for {
		kind, payload, err := wire.ReadFrame(reader)
		if err != nil {
			return false
		}
		if kind == wire.FrameSignal && string(payload) == wire.SignalKill {
			return true
		}
	}
}

func (f *fakeBroker) serve(req wire.Request, conn net.Conn, reader *bufio.Reader) {
	out := wire.NewFrameWriter(conn)
	if req.Command == "start-fail" || slices.Contains(req.Env, "HANDSHAKE=start-fail") {
		exitFrame(out, wire.ExitReport{Error: startFailure})
		return
	}
	if req.Mode == wire.RunnerModeOpenCode {
		handshake := strings.TrimPrefix(req.Env[0], "HANDSHAKE=")
		_, _ = out.Data(wire.FrameStdout, []byte(handshake))
		for {
			kind, _, err := wire.ReadFrame(reader)
			if err != nil {
				return
			}
			if kind == wire.FrameSignal {
				exitFrame(out, wire.ExitReport{Code: 137, Killed: true})
				return
			}
		}
	}
	switch req.Command {
	case "streams":
		_, _ = out.Data(wire.FrameStdout, []byte("out\n"))
		_, _ = out.Data(wire.FrameStderr, []byte("err\n"))
		exitFrame(out, wire.ExitReport{Code: 3})
	case "drop":
		_, _ = out.Data(wire.FrameStdout, []byte("partial"))
	case "oom":
		exitFrame(out, wire.ExitReport{Code: 137, OOM: true, Sandbox: &model.SandboxRecord{ImageID: "sha256:sandbox", Runs: 1, OOM: true,
			Egress: model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{"example.com:443": 2}}}})
	case "limit":
		exitFrame(out, wire.ExitReport{Code: 137, Killed: true, Error: wire.TimeLimitReason})
	case "bad-report":
		_ = out.Frame(wire.FrameExit, []byte("{"))
	case "flood":
		// Output nobody reads, as when the OpenCode bridge's HTTP/2 client has stopped, then the kill's report.
		_, _ = out.Data(wire.FrameStdout, []byte("unread stdout"))
		_, _ = out.Data(wire.FrameStderr, []byte("unread stderr"))
		if awaitKill(reader) {
			exitFrame(out, killedEvidence())
		}
	case "slow-report":
		if awaitKill(reader) {
			time.Sleep(200 * time.Millisecond)
			exitFrame(out, killedEvidence())
		}
	case "ignore-kill":
		// A broker that never confirms the kill: hold the stream until the client cuts it.
		awaitKill(reader)
		_, _ = io.Copy(io.Discard, reader)
	case "remove-fails":
		// The kill worked, but the broker could not confirm the container is gone.
		if awaitKill(reader) {
			exitFrame(out, wire.ExitReport{Code: 137, Killed: true, Error: removeFailure})
		}
	case "drop-on-signal":
		// The broker goes away as the client starts to stop the sandbox.
		for {
			kind, _, err := wire.ReadFrame(reader)
			if err != nil || kind == wire.FrameSignal {
				return
			}
		}
	case "hang", "":
		var echoed bytes.Buffer
		for {
			kind, payload, err := wire.ReadFrame(reader)
			if err != nil {
				return
			}
			switch kind {
			case wire.FrameStdin:
				echoed.Write(payload)
				_, _ = out.Data(wire.FrameStdout, payload)
			case wire.FrameStdinEOF:
				exitFrame(out, wire.ExitReport{Code: 0})
				return
			case wire.FrameSignal:
				if string(payload) == wire.SignalTerminate {
					exitFrame(out, wire.ExitReport{Code: 143})
					return
				}
				exitFrame(out, wire.ExitReport{Code: 137, Killed: true})
				return
			}
		}
	}
}

func TestRemoteVerifyStreamsAndExit(t *testing.T) {
	f := startFakeBroker(t, 2)
	remote := NewRemote(f.socket)
	dir := ownedWorkspace(t)
	out, _, err := Verify(context.Background(), remote, dir, "streams", 30, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Stdout.Bytes) != "out\n" || string(out.Stderr.Bytes) != "err\n" {
		t.Fatalf("streams = %q / %q", out.Stdout.Bytes, out.Stderr.Bytes)
	}
	if code, ok := out.Status.Code(); !ok || code != 3 || out.Status.String() != "exit status: 3" {
		t.Fatalf("status = %v", out.Status)
	}
	req := <-f.requests
	if req.Kind != "verify" || req.Dir != dir || req.Command != "streams" || !req.FreshHome || req.Timeout != 30+VerificationGraceSeconds {
		t.Fatalf("request = %+v", req)
	}
	if info, err := os.Lstat(filepath.Join(filepath.Dir(dir), wire.VerifyHome)); err != nil || !info.IsDir() {
		t.Fatalf("verification home was not prepared: %v", err)
	}
}

func TestRemoteTimeout(t *testing.T) {
	f := startFakeBroker(t, 2)
	start := time.Now()
	_, _, err := Verify(context.Background(), NewRemote(f.socket), ownedWorkspace(t), "hang", 1, true)
	if !errors.Is(err, process.ErrDeadlineElapsed) {
		t.Fatalf("hanging command = %v; want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("termination took %v", elapsed)
	}
}

func TestRemoteUnavailableBroker(t *testing.T) {
	remote := NewRemote(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := remote.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "Sandbox broker is unavailable") {
		t.Fatalf("health = %v", err)
	}
	if _, _, err := Verify(context.Background(), remote, ownedWorkspace(t), "true", 30, true); err == nil || !Infrastructure(err) {
		t.Fatalf("verification without a broker = %v; want a sandbox failure", err)
	}
}

func ownedWorkspace(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), wire.WorkspaceDir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
