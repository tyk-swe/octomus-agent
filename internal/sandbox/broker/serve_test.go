package broker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestFullBrokerMakesRequestsWait(t *testing.T) {
	e := newFakeEngine(t)
	finish := make(chan struct{})
	e.run = func(c *fakeContainer) {
		<-finish
		c.End(0)
	}
	cfg := testConfig(t)
	cfg.Max = 1
	b := e.broker(t, cfg)
	first := serve(t, b)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	holder, err := first.Start(ctx, versionsProbe)
	if err != nil {
		t.Fatal(err)
	}
	e.WaitCreated(t, 1)
	// A second control plane, or one that counted its slot free a moment early, asks while the only slot is held.
	type started struct {
		child sandbox.Child
		err   error
	}
	second := make(chan started, 1)
	go func() {
		child, err := sandbox.NewRemote(first.Socket()).Start(ctx, versionsProbe)
		second <- started{child, err}
	}()
	select {
	case got := <-second:
		t.Fatalf("a request for a held slot did not wait: %v", got.err)
	case <-time.After(300 * time.Millisecond):
	}
	close(finish)
	if status, err := holder.Wait(); err != nil || !status.Success() {
		t.Fatalf("first sandbox = %v, %v", status, err)
	}
	got := <-second
	if got.err != nil {
		t.Fatalf("waiting request = %v; want it admitted once the slot came back", got.err)
	}
	if status, err := got.child.Wait(); err != nil || !status.Success() {
		t.Fatalf("second sandbox = %v, %v", status, err)
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
	b := brokerOn(t, testutil.UnixHTTPServer(t, mux), 1)
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
		req.Header.Set("Upgrade", wire.UpgradeProtocol)
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
	socket := testutil.UnixHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fixture sweep failure", http.StatusServiceUnavailable)
	}))
	b := brokerOn(t, socket, 0)
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

func TestShutdownLetsSandboxesRemoveThemselvesBeforeSweeping(t *testing.T) {
	e := newFakeEngine(t)
	// Docker refuses a second removal of a container while the first one runs.
	e.removeDelay = 300 * time.Millisecond
	e.run = func(*fakeContainer) {}
	cfg := testConfig(t)
	cfg.Max = 4
	b := e.broker(t, cfg)
	listener, socket := testutil.ListenUnix(t, "sandboxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- b.Serve(ctx, listener) }()
	remote := sandbox.NewRemote(socket)
	for range 4 {
		child, err := remote.Start(context.Background(), versionsProbe)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
	}
	e.WaitCreated(t, 4)
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("shutdown = %v; want every sandbox removed cleanly", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if remaining := e.Remaining(); len(remaining) != 0 {
		t.Fatalf("shutdown left %v", remaining)
	}
	// Each sandbox's own teardown removed it; the sweep found nothing to race them for.
	if deletes := e.Deletes(); len(deletes) != 4 {
		t.Fatalf("shutdown sent %d removals for 4 sandboxes; the sweep raced their own teardown", len(deletes))
	}
}
