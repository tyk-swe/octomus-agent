package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// startBroker answers every sandbox request with an upgraded stream that serve scripts, so runner cleanup and
// connect failures can be checked against the remote backend without Docker.
func startBroker(t *testing.T, serve func(out *sandbox.FrameWriter, frames *bufio.Reader)) *sandbox.Remote {
	t.Helper()
	dir, err := os.MkdirTemp("", "runner-broker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "broker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(sandbox.BrokerInfo{Limits: sandbox.BrokerLimits{Max: 2}})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		conn, stream, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + sandbox.UpgradeProtocol + "\r\n\r\n")
		_ = stream.Flush()
		serve(sandbox.NewFrameWriter(conn), stream.Reader)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return sandbox.NewRemote(socket)
}

func reportExit(out *sandbox.FrameWriter, report sandbox.ExitReport) {
	payload, _ := json.Marshal(report)
	_ = out.Frame(sandbox.FrameExit, payload)
}

// awaitKill reads control frames until the client asks for a kill, and reports whether it did.
func awaitKill(frames *bufio.Reader) bool {
	for {
		kind, payload, err := sandbox.ReadFrame(frames)
		if err != nil {
			return false
		}
		if kind == sandbox.FrameSignal && string(payload) == sandbox.SignalKill {
			return true
		}
	}
}

// ownedWorkspace is a workspace inside its own root, as the remote backend prepares one.
func ownedWorkspace(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCodexCloseDrainsRemoteStdoutForKillEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		limit   int
	}{
		{"queued notifications", strings.Repeat("{}\n", 64<<10), MaxMessage},
		{"oversized notification", strings.Repeat("x", 64<<10), 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outputStarted := make(chan struct{})
			remote := startBroker(t, func(out *sandbox.FrameWriter, frames *bufio.Reader) {
				killed := make(chan struct{})
				go func() {
					defer close(killed)
					awaitKill(frames)
				}()
				close(outputStarted)
				if _, err := out.Data(sandbox.FrameStdout, []byte(tc.payload)); err != nil {
					return
				}
				<-killed
				reportExit(out, sandbox.ExitReport{Killed: true, Sandbox: &model.SandboxRecord{ImageID: "fixture-image", Runs: 1}})
			})
			child, err := remote.Start(context.Background(), sandbox.Spec{Kind: sandbox.KindProbe, Stdin: true})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			client := &Codex{child: child, stdin: child.Stdin(), stdout: child.Stdout(), done: done,
				lines: lineReader(child.Stdout(), tc.limit, done), waitCh: make(chan error, 1)}
			go func() { client.waitCh <- exitErr(child.Wait()) }()
			t.Cleanup(func() { _ = child.Stdout().Close(); _ = client.Close() })
			<-outputStarted
			closed := make(chan error, 1)
			go func() { closed <- client.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Codex cleanup blocked before the broker's kill report")
			}
			if evidence := client.SandboxEvidence(); evidence == nil || evidence.ImageID != "fixture-image" {
				t.Fatalf("lost sandbox evidence during cleanup: %+v", evidence)
			}
		})
	}
}

func TestOpenCodeCloseKeepsKillEvidenceBehindUnreadStdout(t *testing.T) {
	t.Parallel()
	flooded := make(chan struct{})
	remote := startBroker(t, func(out *sandbox.FrameWriter, frames *bufio.Reader) {
		_, _ = out.Data(sandbox.FrameStdout, []byte("OCTOMUS-READY\n"))
		// Bytes a sandbox process wrote into the bridge's stdout: the HTTP/2 client has stopped reading.
		_, _ = out.Data(sandbox.FrameStdout, []byte("garbage"))
		close(flooded)
		if awaitKill(frames) {
			reportExit(out, sandbox.ExitReport{Killed: true, Sandbox: &model.SandboxRecord{ImageID: "fixture-image", Runs: 1,
				Egress: model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{"exfil.example.net:443": 40}}}})
		}
	})
	started, err := remote.StartOpenCode(context.Background(), sandbox.Spec{
		Kind: sandbox.KindRunner, Runner: config.BackendOpencode, Dir: ownedWorkspace(t),
	}, 30)
	if err != nil {
		t.Fatal(err)
	}
	server := &OpenCode{child: started.Child, stdout: started.Child.Stdout(), client: newClient(started.Transport),
		waitCh: make(chan error, 1), done: make(chan struct{}), drainDone: started.Drained}
	go func() { server.waitCh <- exitErr(started.Child.Wait()) }()
	<-flooded
	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenCode cleanup blocked behind stdout nobody reads")
	}
	if evidence := server.SandboxEvidence(); evidence == nil || evidence.Egress.Denied["exfil.example.net:443"] != 40 {
		t.Fatalf("lost the refused hosts during cleanup: %+v", evidence)
	}
}

// reportBehindStdout is a sandbox whose end is confirmed only after its pending output is consumed, as on a broker
// stream; its kill waits a moment for that confirmation.
type reportBehindStdout struct {
	stdout    *io.PipeReader
	consumed  chan struct{}
	ended     chan struct{}
	once      sync.Once
	confirmed bool
}

func newReportBehindStdout() *reportBehindStdout {
	r, w := io.Pipe()
	c := &reportBehindStdout{stdout: r, consumed: make(chan struct{}), ended: make(chan struct{})}
	go func() {
		_, _ = w.Write([]byte("output the HTTP/2 client stopped reading"))
		close(c.consumed)
	}()
	return c
}

func (c *reportBehindStdout) Kill() {
	c.once.Do(func() {
		select {
		case <-c.consumed:
			c.confirmed = true
		case <-time.After(time.Second):
		}
		close(c.ended)
	})
}

func (c *reportBehindStdout) Wait() (process.Status, error) {
	<-c.ended
	if !c.confirmed {
		return process.Status{}, &sandbox.SandboxError{Err: errors.New("Sandbox end is unconfirmed")}
	}
	return process.ExitStatus(process.Exit{Code: 137, Killed: true}), nil
}

func (c *reportBehindStdout) Terminate()                    {}
func (c *reportBehindStdout) Stdin() sandbox.DeadlineWriter { return nil }
func (c *reportBehindStdout) Stdout() io.ReadCloser         { return c.stdout }
func (c *reportBehindStdout) Stderr() io.ReadCloser         { return io.NopCloser(strings.NewReader("")) }

func TestOpenCodeCloseDrainsStdoutBeforeTheKill(t *testing.T) {
	t.Parallel()
	child := newReportBehindStdout()
	drained := make(chan struct{})
	close(drained)
	server := &OpenCode{child: child, stdout: child.Stdout(), client: newClient(&http.Transport{}),
		waitCh: make(chan error, 1), done: make(chan struct{}), drainDone: drained}
	go func() { server.waitCh <- exitErr(child.Wait()) }()
	if err := server.Close(); err != nil {
		t.Fatalf("close = %v; the kill must not wait behind stdout nobody reads", err)
	}
}

func TestRunnerConnectFailuresKeepWhyTheSandboxEnded(t *testing.T) {
	t.Parallel()
	const failure = "Starting the sandbox: Error response from daemon: unknown or invalid runtime name: runsc"
	cfg := config.Default()
	cfg.SessionTimeoutSeconds, cfg.CommandTimeoutSeconds = 5, 5
	// The broker created the container and answered the upgrade, then could not start it.
	neverStarted := startBroker(t, func(out *sandbox.FrameWriter, _ *bufio.Reader) {
		reportExit(out, sandbox.ExitReport{Error: failure})
	})
	codex, err := ConnectCodex(context.Background(), cfg, ownedWorkspace(t), nil, "fixture", neverStarted)
	if codex != nil {
		_ = codex.Close()
		t.Fatal("Codex connected to a sandbox that never started")
	}
	if !sandbox.Infrastructure(err) || !strings.Contains(err.Error(), failure) {
		t.Fatalf("Codex connect = %v; want the sandbox's own failure", err)
	}
	opencode, err := ConnectOpenCode(context.Background(), cfg, ownedWorkspace(t), nil, "fixture", neverStarted)
	if opencode != nil {
		_ = opencode.Close()
		t.Fatal("OpenCode connected to a sandbox that never started")
	}
	if !sandbox.Infrastructure(err) || !strings.Contains(err.Error(), failure) {
		t.Fatalf("OpenCode connect = %v; want the sandbox's own failure", err)
	}
	// The stream is lost after readiness: the health check only sees its request fail.
	lost := startBroker(t, func(out *sandbox.FrameWriter, _ *bufio.Reader) {
		_, _ = out.Data(sandbox.FrameStdout, []byte("OCTOMUS-READY\n"))
	})
	opencode, err = ConnectOpenCode(context.Background(), cfg, ownedWorkspace(t), nil, "fixture", lost)
	if opencode != nil {
		_ = opencode.Close()
		t.Fatal("OpenCode connected over a lost stream")
	}
	if !sandbox.Infrastructure(err) || !strings.Contains(err.Error(), "Sandbox stream was lost") {
		t.Fatalf("OpenCode connect = %v; want the lost stream named", err)
	}
}
