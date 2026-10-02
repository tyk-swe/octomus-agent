package main

import (
	"bytes"
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

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/httpapi"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Expire only the HTTP grace period, leaving the real server, API, app, and
// service teardown in place. Runner cleanup has its own, longer deadline.
type expiredDrainHTTP struct {
	observedServiceHTTP
	closed chan struct{}
}

func (s expiredDrainHTTP) Shutdown(ctx context.Context) error {
	expired, cancel := context.WithCancel(ctx)
	cancel()
	return s.Server.Shutdown(expired)
}

func (s expiredDrainHTTP) Close() error {
	err := s.Server.Close()
	close(s.closed)
	return err
}

type serviceDiagnosticAdapter struct {
	runner.Adapter
	ctx     context.Context
	started chan struct{}
	closing chan struct{}
	release chan struct{}
}

func (a serviceDiagnosticAdapter) Models(string) ([]runner.Model, error) {
	close(a.started)
	<-a.ctx.Done()
	return nil, a.ctx.Err()
}

func (a serviceDiagnosticAdapter) Close() error {
	close(a.closing)
	<-a.release
	return a.Adapter.Close()
}

func TestServiceShutdownJoinsDiagnosticAfterHTTPDrainExpires(t *testing.T) {
	dir := t.TempDir()
	state, err := store.Open(filepath.Join(dir, stateDBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	started, closing, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	script := runnertest.New()
	connect := script.Connector()
	var scratch string
	app := engine.New(state, dir, engine.WithRunnerConnector(func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
		client, err := connect(ctx, backend, cfg, cwd)
		if err != nil {
			return nil, err
		}
		scratch = cwd
		return serviceDiagnosticAdapter{client, ctx, started, closing, release}, nil
	}))
	t.Cleanup(app.Shutdown)
	const token = "synthetic-operator-token-for-shutdown-test"
	requestDone := make(chan struct{})
	router := httpapi.Router(app, token, "", "test")
	server := expiredDrainHTTP{
		observedServiceHTTP: observedServiceHTTP{
			Server: newHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(requestDone)
				router.ServeHTTP(w, r)
			})),
			started: make(chan net.Addr, 1),
		},
		closed: make(chan struct{}),
	}
	var workerStopped atomic.Bool
	components := serviceComponents{
		scheduler: app,
		http:      server,
		startWorker: func() (func(), error) {
			return func() { workerStopped.Store(true) }, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- components.run(ctx, "127.0.0.1:0", &bytes.Buffer{}) }()
	var address net.Addr
	select {
	case address = <-server.started:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP server never started")
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+address.String()+"/api/model-catalog", strings.NewReader(`{"backend":"codex","binary":"codex"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached model listing")
	}
	cancel()
	for _, stage := range []struct {
		name string
		done <-chan struct{}
	}{{"runner cleanup", closing}, {"forced HTTP close", server.closed}, {"client disconnect", clientDone}} {
		select {
		case <-stage.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("shutdown never reached %s", stage.name)
		}
	}
	serviceReturned := false
	select {
	case err := <-serviceDone:
		serviceReturned = true
		t.Errorf("service returned while diagnostic cleanup was still active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if workerStopped.Load() {
		t.Error("notification worker stopped before diagnostic cleanup completed")
	}
	unblock()
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("request never completed after cleanup")
	}
	// The failing implementation may already have returned above.
	if !serviceReturned {
		select {
		case err := <-serviceDone:
			if err != nil {
				t.Errorf("normal service shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("service did not finish after diagnostic cleanup")
		}
	}
	if script.OpenClients() != 0 {
		t.Error("diagnostic client remains open after shutdown")
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("diagnostic scratch root remains after shutdown: %v", err)
	}
}
