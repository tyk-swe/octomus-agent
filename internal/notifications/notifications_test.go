// Opt-in webhook delivery: URL policy, the minimal payload, delivery health and status classification.

package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
	_ "modernc.org/sqlite"
)

func testStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	state, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state, path
}

func putTask(t *testing.T, state *store.Store, id, status string, reason *model.BlockedReason) {
	t.Helper()
	task := map[string]any{
		"id": id, "cycle_id": "cycle-1", "run_id": "run-1",
		"status": status, "config": map[string]any{"github_repo": "fixture/project"},
	}
	if reason != nil {
		task["blocked_reason"] = reason.String()
	}
	if err := state.Put("task", id, task); err != nil {
		t.Fatal(err)
	}
}

type receivedRequest struct {
	body []byte
	at   time.Time
}

type receiver struct {
	url      string
	requests chan receivedRequest
	server   *httptest.Server
	mu       sync.Mutex
	delay    time.Duration
}

func (r *receiver) takeDelay() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	delay := r.delay
	r.delay = 0
	return delay
}

func newReceiver(t *testing.T, status int, firstDelay time.Duration) *receiver {
	t.Helper()
	r := &receiver{requests: make(chan receivedRequest, 32), delay: firstDelay}
	closed := make(chan struct{})
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			select {
			case r.requests <- receivedRequest{body: body, at: time.Now()}:
			case <-closed:
				return
			}
		}
		if delay := r.takeDelay(); delay > 0 {
			hold := time.NewTimer(delay)
			defer hold.Stop()
			select {
			case <-hold.C:
			case <-req.Context().Done():
			case <-closed:
			}
		}
		w.WriteHeader(status)
	}))
	r.url = r.server.URL + "/hook"
	t.Cleanup(r.server.Close)
	t.Cleanup(func() { close(closed) })
	return r
}

func (r *receiver) next(t *testing.T) []byte {
	t.Helper()
	return r.nextRequest(t).body
}

func (r *receiver) nextRequest(t *testing.T) receivedRequest {
	t.Helper()
	select {
	case request := <-r.requests:
		return request
	case <-time.After(15 * time.Second):
		t.Fatal("no request received")
		return receivedRequest{}
	}
}

func waitUntil(t *testing.T, seconds float64, condition func() bool, what string) {
	t.Helper()
	if !testutil.WaitUntil(time.Duration(seconds*float64(time.Second)), condition) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestWebhookURLPolicy(t *testing.T) {
	t.Parallel()
	state, _ := testStore(t)
	cases := []struct {
		url      string
		expected string
	}{
		{"https://hooks.example.com/notify?token=synthetic", "enabled"},
		{"http://127.0.0.1:8080/hook", "enabled"},
		{"http://[::1]:8080/hook", "enabled"},
		{"http://10.0.0.1:8080/hook", "invalid"},
		{"http://localhost:8080/hook", "invalid"},
		{"http://user:pass@127.0.0.1:8080/hook", "invalid"},
		{"https://example.com/hook#fragment", "invalid"},
		{"ftp://example.com/hook", "invalid"},
		{"file:///etc/passwd", "invalid"},
		{"https://example.com:99999/hook", "invalid"},
	}
	for _, check := range cases {
		worker, err := Start(context.Background(), state, check.url)
		if err != nil {
			t.Fatalf("%s: %v", check.url, err)
		}
		health, err := state.NotificationHealth()
		if err != nil {
			t.Fatal(err)
		}
		if health.State != check.expected {
			t.Fatalf("%s must be %s, got %s", check.url, check.expected, health.State)
		}
		if check.expected == "invalid" {
			if health.LastError == nil || *health.LastError == "" {
				t.Fatalf("%s missing a sanitized error", check.url)
			}
			if strings.Contains(*health.LastError, check.url) || strings.Contains(*health.LastError, "example.com") {
				t.Fatalf("%s error leaks the destination: %q", check.url, *health.LastError)
			}
		}
		if worker != nil {
			worker.Stop()
		}
	}
	worker, err := Start(context.Background(), state, "https://example.com/"+strings.Repeat("x", 9000))
	if err != nil {
		t.Fatal(err)
	}
	if health, _ := state.NotificationHealth(); health.State != "invalid" {
		t.Fatalf("over-limit URL: %s", health.State)
	}
	if worker != nil {
		worker.Stop()
	}
	worker, err = Start(context.Background(), state, "")
	if err != nil {
		t.Fatal(err)
	}
	if health, _ := state.NotificationHealth(); health.State != "disabled" {
		t.Fatalf("empty URL: %s", health.State)
	}
	if worker != nil {
		worker.Stop()
	}
}

func TestWebhookDelivery(t *testing.T) {
	t.Parallel()
	state, _ := testStore(t)
	server := newReceiver(t, 200, 0)
	worker, err := Start(context.Background(), state, server.url)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	reason := model.BlockedStaleBase
	putTask(t, state, "task-1", "blocked", &reason)
	body := rBody(t, server.next(t))
	for key, want := range map[string]any{
		"schema_version": float64(1),
		"category":       "stale_base",
		"action":         "inspect_task",
		"task_id":        "task-1",
		"repository":     "fixture/project",
		"cycle_id":       "cycle-1",
		"run_id":         "run-1",
	} {
		if body[key] != want {
			t.Fatalf("%s: %v want %v", key, body[key], want)
		}
	}
	if _, ok := body["event_id"].(string); !ok {
		t.Fatal("event_id missing")
	}
	if _, ok := body["occurred_at"].(string); !ok {
		t.Fatal("occurred_at missing")
	}
	if len(body) != 9 {
		t.Fatalf("payload keys: %v", body)
	}
	waitUntil(t, 5, func() bool {
		health, err := state.NotificationHealth()
		return err == nil && health.LastDeliveredAt != nil
	}, "delivery to be recorded")
	health, err := state.NotificationHealth()
	if err != nil || health.Pending != 0 || health.LastDeliveredAt == nil {
		t.Fatalf("health: %+v", health)
	}
}

func rBody(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return value
}

func TestStatusClassification(t *testing.T) {
	t.Parallel()
	for _, check := range []struct {
		status    int
		retryable bool
	}{
		{408, true}, {429, true}, {503, true}, {404, false}, {302, false},
	} {
		t.Run(http.StatusText(check.status), func(t *testing.T) {
			t.Parallel()
			state, path := testStore(t)
			server := newReceiver(t, check.status, 0)
			worker, err := Start(context.Background(), state, server.url)
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Stop()
			reason := model.BlockedTimeout
			putTask(t, state, "task-1", "blocked", &reason)
			server.next(t)
			recorded := func() bool {
				return outboxHas(t, path, "pending", check.status) || outboxHas(t, path, "failed", check.status)
			}
			waitUntil(t, 5, recorded, "HTTP status to be recorded")
			pending := outboxCount(t, path, "pending")
			failed := outboxCount(t, path, "failed")
			if check.retryable {
				if pending != 1 {
					t.Fatalf("%d must retry: pending %d", check.status, pending)
				}
			} else {
				if pending != 0 || failed != 1 {
					t.Fatalf("%d must terminate: pending %d failed %d", check.status, pending, failed)
				}
			}
		})
	}
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func outboxCount(t *testing.T, path, status string) int {
	t.Helper()
	var count int
	if err := rawDB(t, path).QueryRow("SELECT count(*) FROM notification_outbox WHERE status=?", status).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func outboxHas(t *testing.T, path, status string, httpStatus int) bool {
	t.Helper()
	var count int
	if err := rawDB(t, path).QueryRow("SELECT count(*) FROM notification_outbox WHERE status=? AND http_status=?", status, httpStatus).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}
