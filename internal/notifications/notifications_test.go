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
	if err := putRaw(state, store.Tasks.Key(id).Raw(), task); err != nil {
		t.Fatal(err)
	}
}

func putRaw(state *store.Store, key store.Key[json.RawMessage], record map[string]any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return store.Put(state, key, data)
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

type webhookResponseTransport struct {
	status     int
	retryAfter string
}

func (transport webhookResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: transport.status,
		Header:     http.Header{"Retry-After": []string{transport.retryAfter}},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    request,
	}, nil
}

func TestWebhookRetryAfterDefersDestination(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for _, format := range []string{"seconds", "date"} {
			t.Run(http.StatusText(status)+"/"+format, func(t *testing.T) {
				t.Parallel()
				state, path := testStore(t)
				url, destination, err := configure(state, "https://example.com/hook")
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC().Truncate(time.Second)
				deadline := now.Add(time.Hour)
				header := "3600"
				if format == "date" {
					header = deadline.Format(http.TimeFormat)
				}
				worker := &Worker{
					store: state, url: url, destID: destination, ctx: context.Background(), warnings: io.Discard,
					client: &http.Client{Transport: webhookResponseTransport{status: status, retryAfter: header}},
				}
				putTask(t, state, "limited-task", "failed", nil)
				putTask(t, state, "queued-task", "failed", nil)
				worker.deliverNext()
				if err := state.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = reopened.Close() })
				if delivery, err := reopened.ClaimDelivery(destination, now.Add(time.Minute)); err != nil || delivery != nil {
					t.Fatalf("delivery claimed before Retry-After deadline after restart: %+v, %v", delivery, err)
				}
				delivery, err := reopened.ClaimDelivery(destination, time.Now().Add(time.Hour+time.Second))
				if err != nil || delivery == nil || delivery.TaskID == nil || *delivery.TaskID != "limited-task" || delivery.Attempts != 2 {
					t.Fatalf("expected original delivery to retry after cooldown: %+v, %v", delivery, err)
				}
			})
		}
	}
}

func TestWebhookRetryAfterFallback(t *testing.T) {
	t.Parallel()
	for _, check := range []struct {
		name       string
		status     int
		retryAfter string
	}{
		{"missing", http.StatusTooManyRequests, ""},
		{"invalid", http.StatusTooManyRequests, "tomorrow"},
		{"negative", http.StatusServiceUnavailable, "-1"},
		{"overflow", http.StatusServiceUnavailable, "18446744073709551615"},
		{"past", http.StatusServiceUnavailable, time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)},
		{"success", http.StatusOK, "3600"},
		{"terminal", http.StatusBadRequest, "3600"},
	} {
		t.Run(check.name, func(t *testing.T) {
			t.Parallel()
			state, _ := testStore(t)
			url, destination, err := configure(state, "https://example.com/hook")
			if err != nil {
				t.Fatal(err)
			}
			worker := &Worker{
				store: state, url: url, destID: destination, ctx: context.Background(), warnings: io.Discard,
				client: &http.Client{Transport: webhookResponseTransport{status: check.status, retryAfter: check.retryAfter}},
			}
			putTask(t, state, "first-task", "failed", nil)
			putTask(t, state, "queued-task", "failed", nil)
			worker.deliverNext()
			delivery, err := state.ClaimDelivery(destination, time.Now())
			if err != nil || delivery == nil || delivery.TaskID == nil || *delivery.TaskID != "queued-task" || delivery.Attempts != 1 {
				t.Fatalf("expected queued delivery without a cooldown: %+v, %v", delivery, err)
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
