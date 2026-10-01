package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestSandboxUpgradeAllowsTheWholeStartupLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, broker := net.Pipe()
		defer broker.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		finished := make(chan struct{})
		defer close(finished)
		go func() {
			defer broker.Close()
			req, err := http.ReadRequest(bufio.NewReader(broker))
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			// Waiting for a slot, probing a rebuilt image, creating and attaching can exceed two minutes together.
			select {
			case <-time.After(4 * time.Minute):
			case <-finished:
				return
			}
			if _, err := io.WriteString(broker, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+wire.UpgradeProtocol+"\r\n\r\n"); err != nil {
				return
			}
			select {
			case <-time.After(time.Minute):
			case <-finished:
				return
			}
			_, _ = io.WriteString(broker, "stream")
		}()
		conn, reader, err := upgradeSandbox(ctx, client, []byte(`{"kind":"probe","mode":"versions"}`))
		if err != nil {
			t.Fatalf("upgrade after lengthy broker startup = %v; want it to follow the caller's context", err)
		}
		defer conn.Close()
		// The startup context no longer owns the successfully upgraded stream.
		cancel()
		synctest.Wait()
		data := make([]byte, len("stream"))
		if _, err := io.ReadFull(reader, data); err != nil || string(data) != "stream" {
			t.Fatalf("stream after startup context cancellation = %q, %v", data, err)
		}
	})
}

func TestSandboxUpgradeStopsWithTheCallerContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancelled"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, broker := net.Pipe()
				defer broker.Close()
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 3*time.Minute)
				} else {
					go func() {
						select {
						case <-time.After(3 * time.Minute):
							cancel()
						case <-ctx.Done():
						}
					}()
				}
				defer cancel()
				go func() {
					// Read the request but leave its startup pending until the client closes the connection.
					_, _ = io.Copy(io.Discard, broker)
				}()
				start := time.Now()
				_, _, err := upgradeSandbox(ctx, client, []byte(`{"kind":"probe","mode":"versions"}`))
				if !errors.Is(err, process.ErrSessionCancelled) || time.Since(start) != 3*time.Minute {
					t.Fatalf("upgrade stopped after %s with %v; want caller cancellation after 3m", time.Since(start), err)
				}
			})
		})
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
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wire.BrokerInfo{Image: "sandbox:test", Limits: wire.BrokerLimits{Max: max},
			Runners: map[string]string{"codex": "codex-cli 0.153.4"}})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
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
		exitFrame(out, wire.ExitReport{Code: 137, Killed: true, Error: TimeLimitReason})
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

func TestRemoteOpenCodeFailedHandshakeClosesUnreadStdout(t *testing.T) {
	for name, tc := range map[string]struct {
		handshake string
		want      string
		cancel    bool
		sandbox   bool
	}{
		"sandbox never started": {handshake: "start-fail", want: startFailure, sandbox: true},
		"oversized failure":     {handshake: handshakeFailed + strings.Repeat("x", 2*handshakeLimit) + "\n", want: "exceeded its size limit"},
		"unexpected line":       {handshake: "unexpected\n" + strings.Repeat("x", handshakeLimit), want: "unexpected handshake"},
		"failure with tail":     {handshake: handshakeFailed + "invalid URL\n" + strings.Repeat("x", handshakeLimit), want: "invalid URL"},
		"cancelled startup":     {handshake: "OCTOMUS-", cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := startFakeBroker(t, 1)
			remote := NewRemote(f.socket)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := ownedWorkspace(t)
			done := make(chan error, 1)
			go func() {
				_, err := remote.StartOpenCode(ctx, Spec{Kind: KindRunner, Runner: config.BackendOpencode,
					Dir: dir, Env: []string{"HANDSHAKE=" + tc.handshake}}, 30)
				done <- err
			}()
			if tc.cancel {
				select {
				case <-f.requests:
					cancel()
				case <-time.After(3 * time.Second):
					t.Fatal("startup never reached the broker")
				}
			}
			select {
			case err := <-done:
				if tc.cancel {
					if !errors.Is(err, process.ErrSessionCancelled) {
						t.Fatalf("cancelled startup = %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("startup = %v; want %q", err, tc.want)
				}
				if Infrastructure(err) != tc.sandbox {
					t.Fatalf("startup = %v; sandbox failure %v, want %v", err, Infrastructure(err), tc.sandbox)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("failed startup hung while cleaning up unread stdout")
			}
			// Cleanup must also release the only slot so another sandbox can start.
			nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
			defer nextCancel()
			if _, _, err := Verify(nextCtx, remote, dir, "streams", 30, true); err != nil {
				t.Fatalf("failed startup retained its sandbox slot: %v", err)
			}
		})
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
	if req.Kind != "verify" || req.Dir != dir || req.Command != "streams" || !req.FreshHome || req.Timeout != 30+verifyGrace {
		t.Fatalf("request = %+v", req)
	}
	if info, err := os.Lstat(filepath.Join(filepath.Dir(dir), wire.VerifyHome)); err != nil || !info.IsDir() {
		t.Fatalf("verification home was not prepared: %v", err)
	}
}

func TestRemoteTimeoutTerminatesThroughTheBroker(t *testing.T) {
	f := startFakeBroker(t, 2)
	start := time.Now()
	_, _, err := Verify(context.Background(), NewRemote(f.socket), ownedWorkspace(t), "hang", 1, true)
	if !process.IsDeadlineElapsed(err) {
		t.Fatalf("hanging command = %v; want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("termination took %v", elapsed)
	}
}

func TestRemoteExitReasons(t *testing.T) {
	f := startFakeBroker(t, 2)
	remote := NewRemote(f.socket)
	out, evidence, err := Verify(context.Background(), remote, ownedWorkspace(t), "oom", 30, true)
	if err != nil || !out.Status.OOM() || out.Status.Success() || !strings.Contains(out.Status.String(), "memory limit") {
		t.Fatalf("oom = %v, %v", out, err)
	}
	if evidence == nil || !evidence.OOM || evidence.Egress.Denied["example.com:443"] != 2 {
		t.Fatalf("evidence = %+v; want the broker's record of the sandbox", evidence)
	}
	out, _, err = Verify(context.Background(), remote, ownedWorkspace(t), "limit", 30, true)
	if err != nil || out.Status.Success() || out.Status.String() != TimeLimitReason {
		t.Fatalf("time limit = %v, %v", out, err)
	}
	// Every way the sandbox itself fails is reported as the sandbox's failure, never as the command's own result.
	for command, want := range map[string]string{
		"drop":       "Sandbox stream was lost",
		"bad-report": "Sandbox stream was lost",
		"refuse":     "not an owned root",
		"start-fail": startFailure,
	} {
		if _, _, err := Verify(context.Background(), remote, ownedWorkspace(t), command, 30, true); err == nil ||
			!strings.Contains(err.Error(), want) || !Infrastructure(err) {
			t.Fatalf("%s = %v; want a sandbox failure naming %q", command, err, want)
		}
	}
}

func TestRemoteLostStreamIsNeverAKill(t *testing.T) {
	f := startFakeBroker(t, 1)
	child, err := NewRemote(f.socket).Start(context.Background(), Spec{Kind: KindVerify, Dir: ownedWorkspace(t), Command: "drop"})
	if err != nil {
		t.Fatal(err)
	}
	// A runner sees its stdout end and cleans up before anyone waits; the lost broker must still be reported.
	if data, _ := io.ReadAll(child.Stdout()); string(data) != "partial" {
		t.Fatalf("stdout = %q", data)
	}
	child.Kill()
	if status, err := child.Wait(); err == nil || !Infrastructure(err) || !strings.Contains(err.Error(), "Sandbox stream was lost") {
		t.Fatalf("lost stream after a kill = %v, %v; want a sandbox failure, never Octomus's own kill", status, err)
	}
}

func TestRemoteKillConfirmsTheEndOrFails(t *testing.T) {
	// The broker may spend up to 45 seconds confirming a removal before it reports.
	if killReportWait < 60*time.Second {
		t.Fatalf("a kill waits only %s for the broker's report", killReportWait)
	}
	// One slot: each child must give it back before Wait returns, so the last check sees a leaked one.
	f := startFakeBroker(t, 1)
	remote := NewRemote(f.socket)
	remote.killWait = 5 * time.Second
	child, err := remote.Start(context.Background(), Spec{Kind: KindVerify, Dir: ownedWorkspace(t), Command: "slow-report"})
	if err != nil {
		t.Fatal(err)
	}
	child.Kill()
	if status, err := child.Wait(); err != nil || !errors.Is(status.Err(), process.ErrKilled) {
		t.Fatalf("reported kill = %v, %v", status, err)
	}
	if evidence := EvidenceOf(child); evidence == nil || evidence.Egress.Denied["exfil.example.net:443"] != 40 {
		t.Fatalf("evidence = %+v; want the report's record", evidence)
	}

	remote.killWait = 300 * time.Millisecond
	child, err = remote.Start(context.Background(), Spec{Kind: KindVerify, Dir: ownedWorkspace(t), Command: "ignore-kill"})
	if err != nil {
		t.Fatal(err)
	}
	child.Kill()
	status, err := child.Wait()
	if err == nil || !Infrastructure(err) || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatalf("unreported kill = %v, %v; want an unconfirmed end, never a clean kill", status, err)
	}
	slot, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := Verify(slot, remote, ownedWorkspace(t), "streams", 30, true); err != nil {
		t.Fatalf("an unconfirmed kill kept its slot: %v", err)
	}
}

func TestRemoteKilledSandboxTheBrokerFailedIsASandboxFailure(t *testing.T) {
	f := startFakeBroker(t, 1)
	child, err := NewRemote(f.socket).Start(context.Background(), Spec{Kind: KindVerify, Dir: ownedWorkspace(t), Command: "remove-fails"})
	if err != nil {
		t.Fatal(err)
	}
	child.Kill()
	if status, err := child.Wait(); err == nil || !Infrastructure(err) || !strings.Contains(err.Error(), removeFailure) {
		t.Fatalf("killed sandbox the broker could not remove = %v, %v; want a sandbox failure, never Octomus's own kill", status, err)
	}
}

// A command Octomus stops at its timeout or on cancellation keeps the sandbox's failure to end cleanly: the timeout
// alone would let verification read the work tree, or start its next command, beside a container that may still run.
func TestRemoteStoppedCommandKeepsAnUnconfirmedEnd(t *testing.T) {
	for _, tc := range []struct {
		command string
		cancel  bool
		want    string
	}{
		{command: "ignore-kill", want: "Sandbox end is unconfirmed"},
		{command: "drop-on-signal", want: "Sandbox stream was lost"},
		{command: "ignore-kill", cancel: true, want: "Sandbox end is unconfirmed"},
	} {
		t.Run(fmt.Sprintf("%s cancel=%v", tc.command, tc.cancel), func(t *testing.T) {
			t.Parallel()
			f := startFakeBroker(t, 1)
			remote := NewRemote(f.socket)
			remote.killWait = 300 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			seconds := uint64(1)
			if tc.cancel {
				seconds = 30
				go func() {
					<-f.requests
					time.Sleep(200 * time.Millisecond)
					cancel()
				}()
			}
			_, _, err := Verify(ctx, remote, ownedWorkspace(t), tc.command, seconds, true)
			stoppedFor := process.IsDeadlineElapsed(err)
			if tc.cancel {
				stoppedFor = errors.Is(err, process.ErrCancelled)
			}
			if !stoppedFor || !Infrastructure(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("stopped command = %v; want why it was stopped and the sandbox failure %q", err, tc.want)
			}
			slot, cancelSlot := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelSlot()
			if _, _, err := Verify(slot, remote, ownedWorkspace(t), "streams", 30, true); err != nil {
				t.Fatalf("the stopped command kept its slot: %v", err)
			}
		})
	}
}

func TestRemoteKillNeverWaitsOnUnreadOutput(t *testing.T) {
	f := startFakeBroker(t, 1)
	remote := NewRemote(f.socket)
	remote.killWait = 5 * time.Second
	child, err := remote.Start(context.Background(), Spec{Kind: KindVerify, Dir: ownedWorkspace(t), Command: "flood"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.requests
	time.Sleep(100 * time.Millisecond) // Let the output reach the client, where nobody reads it.
	start := time.Now()
	child.Kill()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("kill took %v behind unread output", elapsed)
	}
	if status, err := child.Wait(); err != nil || !errors.Is(status.Err(), process.ErrKilled) {
		t.Fatalf("killed = %v, %v", status, err)
	}
	if evidence := EvidenceOf(child); evidence == nil || evidence.Egress.Denied["exfil.example.net:443"] != 40 {
		t.Fatalf("evidence = %+v; the kill's report must reach the client past unread output", evidence)
	}
}

func TestRemoteStdinNeverSplitsAFrame(t *testing.T) {
	f := startFakeBroker(t, 2)
	child, err := NewRemote(f.socket).Start(context.Background(), Spec{Kind: KindRunner, Runner: config.BackendCodex, Dir: ownedWorkspace(t), Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<17)
	got := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(child.Stdout())
		got <- data
	}()
	for rest := payload; len(rest) > 0; {
		_ = child.Stdin().SetWriteDeadline(time.Now().Add(time.Millisecond))
		n, err := child.Stdin().Write(rest[:min(len(rest), 100_000)])
		rest = rest[n:]
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	}
	_ = child.Stdin().SetWriteDeadline(time.Time{})
	if err := child.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	if data := <-got; !bytes.Equal(data, payload) {
		t.Fatalf("echoed %d bytes; want the exact %d written under short deadlines", len(data), len(payload))
	}
	if status, err := child.Wait(); err != nil || !status.Success() {
		t.Fatalf("wait = %v, %v", status, err)
	}
}

func TestRemoteKillAndSlots(t *testing.T) {
	f := startFakeBroker(t, 1)
	remote := NewRemote(f.socket)
	first, err := remote.Start(context.Background(), Spec{Kind: KindRunner, Runner: config.BackendCodex, Dir: ownedWorkspace(t), Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	blocked, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := remote.Start(blocked, Spec{Kind: KindVerify, Dir: ownedWorkspace(t), Command: "streams"}); !errors.Is(err, process.ErrSessionCancelled) {
		t.Fatalf("start beyond capacity = %v; want it to wait for a slot", err)
	}
	first.Kill()
	status, err := first.Wait()
	if err != nil || !errors.Is(status.Err(), process.ErrKilled) {
		t.Fatalf("killed = %v, %v", status, err)
	}
	if _, _, err := Verify(context.Background(), remote, ownedWorkspace(t), "streams", 30, true); err != nil {
		t.Fatalf("slot was not released after the kill: %v", err)
	}
	version, err := remote.RunnerVersion(context.Background(), Spec{Runner: config.BackendCodex}, 10)
	if err != nil || version != "codex-cli 0.153.4" {
		t.Fatalf("version = %q, %v", version, err)
	}
	if _, err := remote.RunnerVersion(context.Background(), Spec{Runner: config.BackendOpencode}, 10); err == nil {
		t.Fatal("a runner missing from the image must be an error")
	}
}

func TestRemoteUnavailableBrokerFailsClosed(t *testing.T) {
	remote := NewRemote(filepath.Join(t.TempDir(), "missing.sock"))
	if err := remote.Healthy(context.Background()); err == nil || !strings.Contains(err.Error(), "Sandbox broker is unavailable") {
		t.Fatalf("health = %v", err)
	}
	if _, _, err := Verify(context.Background(), remote, ownedWorkspace(t), "true", 30, true); err == nil || !Infrastructure(err) {
		t.Fatalf("verification without a broker = %v; want a sandbox failure", err)
	}
}

func ownedWorkspace(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPrepareRootReplacesPlantedLinksAndRefreshesVerificationHome(t *testing.T) {
	dir := ownedWorkspace(t)
	root := filepath.Dir(dir)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, wire.RunnerHome), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, wire.RunnerHome, ".codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, wire.RunnerHome, ".local"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRoot(Spec{Kind: KindRunner, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, mount := range wire.RunnerHomeDirs {
		if info, err := os.Lstat(filepath.Join(root, wire.RunnerHome, mount.Home)); err != nil || !info.IsDir() {
			t.Fatalf("%s = %v, %v; want a plain directory", mount.Home, info, err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("preparation wrote through a planted link: %v", entries)
	}
	if err := os.MkdirAll(filepath.Join(root, wire.VerifyHome, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRoot(Spec{Kind: KindVerify, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, wire.VerifyHome, "cache")); err != nil {
		t.Fatal("a later command of the same run lost its home")
	}
	if err := PrepareRoot(Spec{Kind: KindVerify, Dir: dir, FreshHome: true}); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, wire.VerifyHome)); err != nil || len(entries) != 0 {
		t.Fatalf("fresh verification home = %v, %v", entries, err)
	}
}
