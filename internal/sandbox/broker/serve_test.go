package broker

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

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
