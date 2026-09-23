package notifications

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/httpapi"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

const heldToken = "operator-fixture-token-with-at-least-32-characters"

// heldReceiver accepts one webhook request and holds its response until the
// test ends after a long first delay without making the
// test wait out the hold on cleanup.
func heldReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{requests: make(chan []byte, 32)}
	release := make(chan struct{})
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			r.requests <- body
		}
		select {
		case <-release:
		case <-time.After(60 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	r.url = r.server.URL + "/hook"
	t.Cleanup(r.server.Close)
	t.Cleanup(func() { close(release) })
	return r
}

// the control API answers while a delivery is held, and application shutdown
// stops the worker and leaves the claimed row to retry with its event id.
func TestHeldHTTPDoesNotBlockSchedulingAndShutdownRecoversTheRow(t *testing.T) {
	server := heldReceiver(t)
	state, path := testStore(t)
	app := engine.New(state, filepath.Dir(path))
	worker, err := Start(app.Context(), state, server.url)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	reason := model.BlockedReasonTimeout
	putTask(t, state, "task-1", "blocked", &reason)
	server.next(t)

	router := httpapi.Router(app, heldToken, "", "test")
	for _, call := range []struct{ method, path string }{{"GET", "/api/state"}, {"POST", "/api/control/pause"}} {
		req := httptest.NewRequest(call.method, call.path, nil)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", "Bearer "+heldToken)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			done <- recorder
		}()
		select {
		case response := <-done:
			if response.Code != http.StatusOK {
				t.Fatalf("%s %s: %d %s", call.method, call.path, response.Code, response.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s %s blocked behind the held delivery", call.method, call.path)
		}
	}
	if health, err := state.NotificationHealth(); err != nil || health.Pending != 1 {
		t.Fatalf("pending during held delivery: %+v %v", health, err)
	}

	app.Shutdown()
	select {
	case <-worker.done:
	case <-time.After(15 * time.Second):
		t.Fatal("application shutdown did not stop the delivery worker")
	}
	destination := queryDestination(t, path)
	before := outboxRows(t, path, "pending")
	if len(before) != 1 {
		t.Fatalf("pending rows: %d", len(before))
	}
	delivery, err := state.ClaimNotification(destination, time.Now().UTC().Add(31*time.Second))
	if err != nil || delivery == nil {
		t.Fatalf("an abandoned in-flight row retries: %v %v", delivery, err)
	}
	if delivery.EventID != before[0].EventID {
		t.Fatal("recovery after an ambiguous send keeps the same event id")
	}
}
