package broker

import (
	"bytes"
	"context"
	"errors"
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

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
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
			b := &Broker{cfg: testConfig(t), engine: engineapi.New(serveTestEngine(t, mux)), live: map[string]string{}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			controls := make(chan control)
			done := make(chan error, 1)
			limit := time.Minute
			if action == "timeout" {
				limit = time.Second
			}
			go func() {
				_, err := b.runSandbox(ctx, plan{kind: sandbox.KindProbe, stdin: true, timeout: limit},
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
				case controls <- control{signal: signalKill}:
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

func TestShutdownCancelsPreparationAndSweepsSandboxes(t *testing.T) {
	creating, cancelled := make(chan struct{}), make(chan struct{})
	var removed atomic.Bool
	prefix := "/v" + engineapi.APIVersion + "/containers/"
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+prefix+"create", func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(creating)
		<-r.Context().Done()
		close(cancelled)
	})
	mux.HandleFunc("GET "+prefix+"json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"Id":"leftover","Labels":{"octomus.sandbox.instance":"octomus"}}]`)
	})
	mux.HandleFunc("DELETE "+prefix+"leftover", func(w http.ResponseWriter, _ *http.Request) {
		removed.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v"+engineapi.APIVersion+"/images/{ref}/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Id":"sha256:test"}`)
	})
	b := &Broker{cfg: testConfig(t), engine: engineapi.New(serveTestEngine(t, mux)), live: map[string]string{}, slots: make(chan struct{}, 1)}
	b.info.ImageID = "sha256:test"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Serve(ctx, listener) }()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		req, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v1/sandboxes",
			bytes.NewBufferString(`{"kind":"probe","mode":"versions"}`))
		req.Header.Set("Upgrade", sandbox.UpgradeProtocol)
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-creating:
	case <-time.After(2 * time.Second):
		t.Fatal("Docker create did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel preparation")
	}
	<-requestDone
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Docker create was not cancelled")
	}
	if !removed.Load() || len(b.slots) != 0 {
		t.Fatal("shutdown left a sandbox or admission slot behind")
	}
}

func TestShutdownReportsSweepFailure(t *testing.T) {
	socket := serveTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fixture sweep failure", http.StatusServiceUnavailable)
	}))
	b := &Broker{cfg: testConfig(t), engine: engineapi.New(socket)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Serve(ctx, listener); err == nil || !strings.Contains(err.Error(), "fixture sweep failure") || errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown cleanup error = %v", err)
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
	b := &Broker{cfg: testConfig(t), engine: engineapi.New(socket), live: map[string]string{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := plan{kind: sandbox.KindProbe, timeout: time.Second}
	report, err := b.runSandbox(ctx, p, func([]byte) error { return nil }, func([]byte) error { return nil }, nil)
	if err != nil || report.Code != 7 || report.Killed {
		t.Fatalf("quick exit = %+v, %v; want exit code 7", report, err)
	}
	if !removed.Load() {
		t.Fatal("finished sandbox was not removed")
	}
}
