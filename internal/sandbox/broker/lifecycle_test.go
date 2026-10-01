package broker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

func serveTestEngine(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "octomus-engine-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

// brokerOn builds a broker against a test engine without New's deployment checks; limit, when set, caps its
// sandboxes.
func brokerOn(t *testing.T, socket string, limit int) *Broker {
	t.Helper()
	cfg := testConfig(t)
	cfg.DockerSocket, cfg.Log = socket, io.Discard
	if limit > 0 {
		cfg.Max = limit
	}
	return newBroker(cfg)
}

func TestBlockedStdinDoesNotBlockSandboxLifecycle(t *testing.T) {
	for _, action := range []string{"cancel", "disconnect", "kill", "timeout", "backlog"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			inputStarted, exited := make(chan struct{}), make(chan struct{})
			stop := sync.OnceFunc(func() { close(exited) })
			t.Cleanup(stop)
			var removed atomic.Bool
			prefix := "/v" + engineapi.APIVersion + "/containers/"
			mux := http.NewServeMux()
			mux.HandleFunc("POST "+prefix+"create", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"Id":"blocked"}`)
			})
			mux.HandleFunc("POST "+prefix+"blocked/attach", func(w http.ResponseWriter, _ *http.Request) {
				conn, stream, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_, _ = stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
				_ = stream.Flush()
				// Read one byte to confirm the write began, then stop consuming stdin.
				var first [1]byte
				if _, err := io.ReadFull(stream, first[:]); err == nil {
					close(inputStarted)
				}
				<-exited
			})
			mux.HandleFunc("POST "+prefix+"blocked/start", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("POST "+prefix+"blocked/wait", func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-exited:
					_, _ = io.WriteString(w, `{"StatusCode":137}`)
				case <-r.Context().Done():
				}
			})
			mux.HandleFunc("POST "+prefix+"blocked/kill", func(w http.ResponseWriter, _ *http.Request) {
				stop()
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("GET "+prefix+"blocked/json", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"State":{"Status":"exited"}}`)
			})
			mux.HandleFunc("DELETE "+prefix+"blocked", func(w http.ResponseWriter, _ *http.Request) {
				removed.Store(true)
				stop()
				w.WriteHeader(http.StatusNoContent)
			})
			b := brokerOn(t, serveTestEngine(t, mux), 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			controls := make(chan control)
			done := make(chan error, 1)
			limit := time.Minute
			if action == "timeout" {
				limit = time.Second
			}
			go func() {
				_, err := b.runSandbox(ctx, plan{kind: wire.KindProbe, stdin: true, timeout: limit},
					func([]byte) error { return nil }, func([]byte) error { return nil }, controls)
				done <- err
			}()
			select {
			case controls <- control{stdin: make([]byte, 8<<20)}:
			case <-time.After(2 * time.Second):
				t.Fatal("sandbox did not accept stdin")
			}
			select {
			case <-inputStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("stdin forwarding did not start")
			}
			switch action {
			case "cancel":
				cancel()
			case "disconnect":
				close(controls)
			case "kill":
				select {
				case controls <- control{signal: wire.SignalKill}:
				case <-time.After(2 * time.Second):
					t.Fatal("kill was blocked behind stdin")
				}
			case "backlog":
				go func() {
					for range 40 {
						select {
						case controls <- control{stdin: make([]byte, 1<<20)}:
						case <-ctx.Done():
							return
						}
					}
				}()
			}
			select {
			case err := <-done:
				if action == "backlog" {
					if err == nil || !strings.Contains(err.Error(), "stdin backlog exceeded") {
						t.Fatalf("full input queue = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("blocked stdin prevented sandbox cleanup")
			}
			if !removed.Load() || b.Info().Live != 0 {
				t.Fatal("sandbox was not removed")
			}
		})
	}
}

func TestPreparedObservesContainerThatExitsDuringStart(t *testing.T) {
	// Delay next-exit registration until an immediate exit has already happened. A not-running wait issued before
	// start would instead return the created container's initial status.
	var started, removed atomic.Bool
	startedDone := make(chan struct{})
	mux := http.NewServeMux()
	prefix := "/v" + engineapi.APIVersion + "/containers/"
	mux.HandleFunc("POST "+prefix+"create", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Id":"quick"}`)
	})
	mux.HandleFunc("POST "+prefix+"quick/attach", func(w http.ResponseWriter, _ *http.Request) {
		conn, stream, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = stream.Flush()
	})
	mux.HandleFunc("POST "+prefix+"quick/start", func(w http.ResponseWriter, _ *http.Request) {
		started.Store(true)
		close(startedDone)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+prefix+"quick/wait", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("condition") != "not-running" {
			select {
			case <-startedDone:
			case <-r.Context().Done():
				return
			}
			<-r.Context().Done()
			return
		}
		if !started.Load() {
			_, _ = io.WriteString(w, `{"StatusCode":0}`)
			return
		}
		_, _ = io.WriteString(w, `{"StatusCode":7}`)
	})
	mux.HandleFunc("GET "+prefix+"quick/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"State":{"Status":"exited","ExitCode":7}}`)
	})
	mux.HandleFunc("DELETE "+prefix+"quick", func(w http.ResponseWriter, _ *http.Request) {
		removed.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	dir, err := os.MkdirTemp("", "octomus-engine-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	b := brokerOn(t, socket, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := plan{kind: wire.KindProbe, timeout: time.Second}
	report, err := b.runSandbox(ctx, p, func([]byte) error { return nil }, func([]byte) error { return nil }, nil)
	if err != nil || report.Code != 7 || report.Killed {
		t.Fatalf("quick exit = %+v, %v; want exit code 7", report, err)
	}
	if !removed.Load() {
		t.Fatal("finished sandbox was not removed")
	}
}
