package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type testServiceScheduler struct {
	recover  func() error
	run      func(context.Context) error
	shutdown func()
}

func (s testServiceScheduler) Recover() error {
	if s.recover != nil {
		return s.recover()
	}
	return nil
}
func (s testServiceScheduler) Run(ctx context.Context) error {
	if s.run != nil {
		return s.run(ctx)
	}
	<-ctx.Done()
	return nil
}
func (s testServiceScheduler) Shutdown() {
	if s.shutdown != nil {
		s.shutdown()
	}
}
func (testServiceScheduler) Drained() bool { return true }

type observedServiceHTTP struct {
	*http.Server
	started chan net.Addr
}

func (s observedServiceHTTP) Serve(listener net.Listener) error {
	s.started <- listener.Addr()
	return s.Server.Serve(listener)
}

func TestSignalShutdownWaitsForSchedulerDrain(t *testing.T) {
	ctx, signal := context.WithCancel(context.Background())
	defer signal()
	runStarted := make(chan struct{})
	shutdownStarted := make(chan struct{})
	drained := make(chan struct{})
	var workerStopped atomic.Int32
	server := observedServiceHTTP{
		Server:  &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })},
		started: make(chan net.Addr, 1),
	}
	components := serviceComponents{
		scheduler: testServiceScheduler{
			run: func(ctx context.Context) error {
				close(runStarted)
				<-ctx.Done()
				<-drained
				return nil
			},
			shutdown: func() { close(shutdownStarted) },
		},
		http: server,
		startWorker: func() (func(), error) {
			return func() { workerStopped.Add(1) }, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- components.run(ctx, "127.0.0.1:0", &bytes.Buffer{}) }()
	select {
	case <-runStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler did not start")
	}
	select {
	case <-server.started:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP server did not start")
	}
	signal()
	select {
	case <-shutdownStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler shutdown did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("service returned before the scheduler drained: %v", err)
	default:
	}
	close(drained)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("normal shutdown returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service did not finish after the scheduler drained")
	}
	if workerStopped.Load() != 1 {
		t.Fatalf("worker stop calls = %d, want one", workerStopped.Load())
	}
}
