package runner

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

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
			dir, err := os.MkdirTemp("", "codex-cleanup-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			socket := filepath.Join(dir, "broker.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			outputStarted := make(chan struct{})
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(sandbox.BrokerInfo{Limits: sandbox.BrokerLimits{Max: 1}})
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
				killed := make(chan struct{})
				go func() {
					defer close(killed)
					for {
						kind, payload, err := sandbox.ReadFrame(stream.Reader)
						if err != nil || kind == sandbox.FrameSignal && string(payload) == sandbox.SignalKill {
							return
						}
					}
				}()
				out := sandbox.NewFrameWriter(conn)
				close(outputStarted)
				if _, err := out.Data(sandbox.FrameStdout, []byte(tc.payload)); err != nil {
					return
				}
				<-killed
				payload, _ := json.Marshal(sandbox.ExitReport{Killed: true, Sandbox: &model.SandboxRecord{ImageID: "fixture-image", Runs: 1}})
				_ = out.Frame(sandbox.FrameExit, payload)
			})
			server := &http.Server{Handler: mux}
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { _ = server.Close() })
			child, err := sandbox.NewRemote(socket).Start(context.Background(), sandbox.Spec{Kind: sandbox.KindProbe, Stdin: true})
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
