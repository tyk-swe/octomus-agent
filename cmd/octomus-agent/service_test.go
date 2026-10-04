// The service's start and stop around a real scheduler and HTTP server.

package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestSignalShutdownWaitsForSchedulerDrain(t *testing.T) {
	data := t.TempDir()
	state, err := store.Open(filepath.Join(data, stateDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	app := engine.New(state, data)
	started := make(chan net.Addr, 1)
	server := newHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	server.BaseContext = func(listener net.Listener) context.Context {
		started <- listener.Addr()
		return context.Background()
	}
	var workerStopped atomic.Int32
	components := serviceComponents{
		app:  app,
		http: server,
		startWorker: func() (func(), error) {
			return func() { workerStopped.Add(1) }, nil
		},
	}
	ctx, signal := context.WithCancel(context.Background())
	defer signal()
	done := make(chan error, 1)
	go func() { done <- components.run(ctx, "127.0.0.1:0", &bytes.Buffer{}) }()
	var address net.Addr
	select {
	case address = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP server did not start")
	}
	response, err := http.Get("http://" + address.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	select {
	case err := <-done:
		t.Fatalf("service returned before the shutdown signal: %v", err)
	default:
	}
	signal()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normal shutdown returned an error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("service did not finish after the shutdown signal")
	}
	if app.Context().Err() == nil || !app.Drained() {
		t.Fatal("service returned before the scheduler shut down and drained")
	}
	if workerStopped.Load() != 1 {
		t.Fatalf("worker stop calls = %d, want one", workerStopped.Load())
	}
	if conn, err := net.DialTimeout("tcp", address.String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("HTTP listener still accepts connections after shutdown")
	}
}
