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
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
)

// fakeBroker speaks the broker protocol with scripted sandboxes chosen by the verification command.
type fakeBroker struct {
	socket   string
	requests chan Request
}

func startFakeBroker(t *testing.T, max int) *fakeBroker {
	t.Helper()
	dir, err := os.MkdirTemp("", "octomus-broker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeBroker{socket: filepath.Join(dir, "sandboxd.sock"), requests: make(chan Request, 64)}
	listener, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(BrokerInfo{Image: "sandbox:test", Limits: BrokerLimits{Max: max},
			Runners: map[string]string{"codex": "codex-cli 0.153.4"}})
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.requests <- req
		if req.Command == "refuse" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"Sandbox directory is not an owned root"}`)
			return
		}
		conn, stream, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		_, _ = stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + UpgradeProtocol + "\r\n\r\n")
		_ = stream.Flush()
		f.serve(req, conn, stream.Reader)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	return f
}

func exitFrame(out *FrameWriter, report ExitReport) {
	payload, _ := json.Marshal(report)
	_ = out.Frame(FrameExit, payload)
}

func (f *fakeBroker) serve(req Request, conn net.Conn, reader *bufio.Reader) {
	out := NewFrameWriter(conn)
	switch req.Command {
	case "streams":
		_, _ = out.Data(FrameStdout, []byte("out\n"))
		_, _ = out.Data(FrameStderr, []byte("err\n"))
		exitFrame(out, ExitReport{Code: 3})
	case "drop":
		_, _ = out.Data(FrameStdout, []byte("partial"))
	case "oom":
		exitFrame(out, ExitReport{Code: 137, OOM: true})
	case "limit":
		exitFrame(out, ExitReport{Code: 137, Killed: true, Error: "Sandbox time limit reached"})
	case "hang", "":
		var echoed bytes.Buffer
		for {
			kind, payload, err := ReadFrame(reader)
			if err != nil {
				return
			}
			switch kind {
			case FrameStdin:
				echoed.Write(payload)
				_, _ = out.Data(FrameStdout, payload)
			case FrameStdinEOF:
				exitFrame(out, ExitReport{Code: 0})
				return
			case FrameSignal:
				if string(payload) == SignalTerminate {
					exitFrame(out, ExitReport{Code: 143})
					return
				}
				exitFrame(out, ExitReport{Code: 137, Killed: true})
				return
			}
		}
	}
}

func TestRemoteVerifyStreamsAndExit(t *testing.T) {
	f := startFakeBroker(t, 2)
	remote := NewRemote(f.socket)
	dir := ownedWorkspace(t)
	out, err := Verify(context.Background(), remote, dir, "streams", 30, true)
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
	if info, err := os.Lstat(filepath.Join(filepath.Dir(dir), VerifyHome)); err != nil || !info.IsDir() {
		t.Fatalf("verification home was not prepared: %v", err)
	}
}

func TestRemoteTimeoutTerminatesThroughTheBroker(t *testing.T) {
	f := startFakeBroker(t, 2)
	start := time.Now()
	_, err := Verify(context.Background(), NewRemote(f.socket), ownedWorkspace(t), "hang", 1, true)
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
	out, err := Verify(context.Background(), remote, ownedWorkspace(t), "oom", 30, true)
	if err != nil || !out.Status.OOM() || out.Status.Success() || !strings.Contains(out.Status.String(), "memory limit") {
		t.Fatalf("oom = %v, %v", out, err)
	}
	out, err = Verify(context.Background(), remote, ownedWorkspace(t), "limit", 30, true)
	if err != nil || out.Status.Success() || out.Status.String() != "Sandbox time limit reached" {
		t.Fatalf("time limit = %v, %v", out, err)
	}
	if _, err := Verify(context.Background(), remote, ownedWorkspace(t), "drop", 30, true); err == nil || !strings.Contains(err.Error(), "Sandbox stream was lost") {
		t.Fatalf("dropped stream = %v; want an error, never a clean exit", err)
	}
	if _, err := Verify(context.Background(), remote, ownedWorkspace(t), "refuse", 30, true); err == nil || !strings.Contains(err.Error(), "not an owned root") {
		t.Fatalf("refusal = %v; want the broker's reason", err)
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
	if _, err := Verify(context.Background(), remote, ownedWorkspace(t), "streams", 30, true); err != nil {
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
	if _, err := Verify(context.Background(), remote, ownedWorkspace(t), "true", 30, true); err == nil {
		t.Fatal("verification ran without a broker")
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
	if err := os.MkdirAll(filepath.Join(root, RunnerHome), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, RunnerHome, ".codex")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, RunnerHome, ".local"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRoot(Spec{Kind: KindRunner, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, mount := range RunnerHomeDirs {
		if info, err := os.Lstat(filepath.Join(root, RunnerHome, mount.Home)); err != nil || !info.IsDir() {
			t.Fatalf("%s = %v, %v; want a plain directory", mount.Home, info, err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("preparation wrote through a planted link: %v", entries)
	}
	if err := os.MkdirAll(filepath.Join(root, VerifyHome, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRoot(Spec{Kind: KindVerify, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, VerifyHome, "cache")); err != nil {
		t.Fatal("a later command of the same run lost its home")
	}
	if err := PrepareRoot(Spec{Kind: KindVerify, Dir: dir, FreshHome: true}); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(root, VerifyHome)); err != nil || len(entries) != 0 {
		t.Fatalf("fresh verification home = %v, %v", entries, err)
	}
}

func FuzzReadFrame(f *testing.F) {
	var seed bytes.Buffer
	_ = NewFrameWriter(&seed).Frame(FrameStdout, []byte("hello"))
	f.Add(seed.Bytes())
	f.Add([]byte{FrameExit, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		kind, payload, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			return
		}
		if len(payload) > maxFrame || len(data) < 5+len(payload) {
			t.Fatalf("frame %d with %d bytes from %d input bytes", kind, len(payload), len(data))
		}
		var again bytes.Buffer
		if err := NewFrameWriter(&again).Frame(kind, payload); err != nil || !bytes.Equal(again.Bytes(), data[:5+len(payload)]) {
			t.Fatal("frame does not round-trip")
		}
	})
}
