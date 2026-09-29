package broker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
)

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
