package main

import (
	"bytes"
	"context"
	"errors"
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

type failingServiceHTTP struct {
	serveCount    *atomic.Int32
	shutdownCount *atomic.Int32
	failure       error
	address       chan net.Addr
}

func (s failingServiceHTTP) Serve(listener net.Listener) error {
	s.address <- listener.Addr()
	s.serveCount.Add(1)
	return s.failure
}
func (s failingServiceHTTP) Shutdown(context.Context) error {
	s.shutdownCount.Add(1)
	return nil
}
func (failingServiceHTTP) Close() error { return nil }

func TestSchedulerRecoveryFailureNeverOpensHealthListener(t *testing.T) {
	startupErr := errors.New("cannot recover scheduler state")
	var recovered, workerStarted, listenerOpened atomic.Int32
	components := serviceComponents{
		scheduler: testServiceScheduler{
			recover: func() error {
				recovered.Add(1)
				return startupErr
			},
			run: func(context.Context) error {
				t.Fatal("scheduler ran after failed recovery")
				return nil
			},
		},
		http: &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("health was served after failed recovery")
		})},
		startWorker: func() (func(), error) {
			workerStarted.Add(1)
			return nil, nil
		},
		listen: func(network, address string) (net.Listener, error) {
			listenerOpened.Add(1)
			return net.Listen(network, address)
		},
	}
	var output bytes.Buffer
	err := components.run(context.Background(), "127.0.0.1:0", &output)
	if !errors.Is(err, startupErr) {
		t.Fatalf("startup error = %v, want %v", err, startupErr)
	}
	if recovered.Load() != 1 || workerStarted.Load() != 0 || listenerOpened.Load() != 0 || output.Len() != 0 {
		t.Fatalf("failed recovery exposed service: recoveries=%d workers=%d listeners=%d output=%q",
			recovered.Load(), workerStarted.Load(), listenerOpened.Load(), output.String())
	}
}

func TestSchedulerExitStopsHealthAndPropagatesError(t *testing.T) {
	fatal := errors.New("scheduler exited")
	fail := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var recovered, workerStopped, schedulerStopped atomic.Int32
	server := observedServiceHTTP{
		Server: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				w.WriteHeader(http.StatusOK)
			}
		})},
		started: make(chan net.Addr, 1),
	}
	components := serviceComponents{
		scheduler: testServiceScheduler{
			recover: func() error { recovered.Add(1); return nil },
			run: func(ctx context.Context) error {
				select {
				case <-fail:
					return fatal
				case <-ctx.Done():
					return nil
				}
			},
			shutdown: func() { schedulerStopped.Add(1) },
		},
		http: server,
		startWorker: func() (func(), error) {
			return func() { workerStopped.Add(1) }, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- components.run(ctx, "127.0.0.1:0", &bytes.Buffer{}) }()
	var address string
	select {
	case addr := <-server.started:
		address = addr.String()
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP server never started")
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://" + address + "/healthz")
	if err != nil {
		t.Fatal("health was unavailable before scheduler exit:", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status before failure = %d", response.StatusCode)
	}
	close(fail)
	select {
	case err := <-done:
		if !errors.Is(err, fatal) {
			t.Fatalf("service error = %v, want scheduler error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service did not stop after scheduler exit")
	}
	if recovered.Load() != 1 || workerStopped.Load() != 1 || schedulerStopped.Load() != 1 {
		t.Fatalf("unexpected lifecycle counts: recover=%d worker stop=%d scheduler stop=%d",
			recovered.Load(), workerStopped.Load(), schedulerStopped.Load())
	}
	connection, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("health listener remains open after scheduler exit")
	}
}

func TestImmediateSchedulerExitClosesHealthListenerAndPropagatesError(t *testing.T) {
	fatal := errors.New("scheduler failed at startup")
	address := make(chan string, 1)
	var schedulerStopped, workerStopped atomic.Int32
	components := serviceComponents{
		scheduler: testServiceScheduler{
			run:      func(context.Context) error { return fatal },
			shutdown: func() { schedulerStopped.Add(1) },
		},
		http: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				w.WriteHeader(http.StatusOK)
			}
		})},
		startWorker: func() (func(), error) {
			return func() { workerStopped.Add(1) }, nil
		},
		listen: func(network, host string) (net.Listener, error) {
			listener, err := net.Listen(network, host)
			if err == nil {
				address <- listener.Addr().String()
			}
			return listener, err
		},
	}
	done := make(chan error, 1)
	go func() { done <- components.run(context.Background(), "127.0.0.1:0", &bytes.Buffer{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, fatal) {
			t.Fatalf("service error = %v, want original scheduler error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service did not stop after immediate scheduler exit")
	}
	if schedulerStopped.Load() != 1 || workerStopped.Load() != 1 {
		t.Fatalf("shutdown counts: scheduler=%d worker=%d", schedulerStopped.Load(), workerStopped.Load())
	}
	select {
	case host := <-address:
		connection, err := net.DialTimeout("tcp", host, 200*time.Millisecond)
		if err == nil {
			connection.Close()
			t.Fatal("health listener remains open after immediate scheduler exit")
		}
	default:
		t.Fatal("service never bound its listener")
	}
}

func TestEarlyHTTPExitCancelsSchedulerAndPropagatesError(t *testing.T) {
	fatal := errors.New("HTTP listener failed")
	var runCount, serveCount, schedulerStopped, serverStopped, workerStopped atomic.Int32
	server := failingServiceHTTP{
		serveCount: &serveCount, shutdownCount: &serverStopped,
		failure: fatal, address: make(chan net.Addr, 1),
	}
	components := serviceComponents{
		scheduler: testServiceScheduler{
			run: func(ctx context.Context) error {
				runCount.Add(1)
				<-ctx.Done()
				return nil
			},
			shutdown: func() { schedulerStopped.Add(1) },
		},
		http: server,
		startWorker: func() (func(), error) {
			return func() { workerStopped.Add(1) }, nil
		},
	}
	done := make(chan error, 1)
	go func() { done <- components.run(context.Background(), "127.0.0.1:0", &bytes.Buffer{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, fatal) {
			t.Fatalf("service error = %v, want original HTTP error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service hung after HTTP exit")
	}
	if runCount.Load() != 1 || serveCount.Load() != 1 || schedulerStopped.Load() != 1 || serverStopped.Load() != 1 || workerStopped.Load() != 1 {
		t.Fatalf("unexpected lifecycle counts: run=%d serve=%d scheduler stop=%d HTTP stop=%d worker stop=%d",
			runCount.Load(), serveCount.Load(), schedulerStopped.Load(), serverStopped.Load(), workerStopped.Load())
	}
	address := <-server.address
	connection, err := net.DialTimeout("tcp", address.String(), 200*time.Millisecond)
	if err == nil {
		connection.Close()
		t.Fatal("HTTP listener remains open after server exit")
	}
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

// The service bounds how long a connection may take to send headers or sit
// idle, but never the request or response itself: doctor and model catalog
// requests legitimately take about a minute.
func TestServiceHTTPServerBoundsHeadersAndIdleOnly(t *testing.T) {
	server := newHTTPServer(http.NotFoundHandler())
	if server.ReadHeaderTimeout != 10*time.Second || server.IdleTimeout != 2*time.Minute {
		t.Fatalf("header %v idle %v", server.ReadHeaderTimeout, server.IdleTimeout)
	}
	if server.ReadTimeout != 0 || server.WriteTimeout != 0 {
		t.Fatalf("read %v write %v", server.ReadTimeout, server.WriteTimeout)
	}
}
