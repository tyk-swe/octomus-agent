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
	_ "modernc.org/sqlite"
)

const dest = "destination-a"

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

func enabled(t *testing.T, state *store.Store) {
	t.Helper()
	if err := state.ConfigureNotifications(strPtr(dest), "enabled", nil); err != nil {
		t.Fatal(err)
	}
}

func strPtr(s string) *string { return &s }

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

// receiver hands every request body to the channel and delays the first
// response like the scripted peer does.
type receiver struct {
	url      string
	requests chan []byte
	server   *httptest.Server
	mu       sync.Mutex
	delay    time.Duration // the next response's delay; only the first is held
}

// takeDelay returns the pending first-response delay and clears it. Handlers
// run concurrently, so the delay is read and cleared under the lock.
func (r *receiver) takeDelay() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	delay := r.delay
	r.delay = 0
	return delay
}

func newReceiver(t *testing.T, status int, firstDelay time.Duration) *receiver {
	t.Helper()
	r := &receiver{requests: make(chan []byte, 32), delay: firstDelay}
	// closed ends every held response when the test finishes: the delay is the
	// most a response is held, not what cleanup must wait out, because
	// httptest.Server.Close waits for running handlers.
	closed := make(chan struct{})
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			select {
			case r.requests <- body:
			case <-closed:
				return
			}
		}
		if delay := r.takeDelay(); delay > 0 {
			hold := time.NewTimer(delay)
			defer hold.Stop()
			// A client that gives up (its timeout or a stopped worker) closes
			// the connection, which cancels the request context.
			select {
			case <-hold.C:
			case <-req.Context().Done():
			case <-closed:
			}
		}
		w.WriteHeader(status)
	}))
	r.url = r.server.URL + "/hook"
	// Cleanups run last-registered first: release held handlers, then close.
	t.Cleanup(r.server.Close)
	t.Cleanup(func() { close(closed) })
	return r
}

func (r *receiver) next(t *testing.T) []byte {
	t.Helper()
	select {
	case body := <-r.requests:
		return body
	case <-time.After(15 * time.Second):
		t.Fatal("no request received")
		return nil
	}
}

func waitUntil(t *testing.T, seconds float64, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(seconds * float64(time.Second)))
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWebhookURLPolicyAcceptsHTTPSAndLoopbackHTTPOnly(t *testing.T) {
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

func TestLocalReceiverVerifiesMinimalPayloadAndDelivery(t *testing.T) {
	state, _ := testStore(t)
	server := newReceiver(t, 200, 0)
	worker, err := Start(context.Background(), state, server.url)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	reason := model.BlockedReasonStaleBase
	putTask(t, state, "task-1", "blocked", &reason)
	body := r_body(t, server.next(t))
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

func r_body(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return value
}

func TestRetryableAndTerminalStatusesAreClassified(t *testing.T) {
	for _, check := range []struct {
		status    int
		retryable bool
	}{
		{408, true}, {429, true}, {503, true}, {404, false}, {302, false},
	} {
		t.Run(http.StatusText(check.status), func(t *testing.T) {
			state, path := testStore(t)
			server := newReceiver(t, check.status, 0)
			worker, err := Start(context.Background(), state, server.url)
			if err != nil {
				t.Fatal(err)
			}
			defer worker.Stop()
			reason := model.BlockedReasonTimeout
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

func TestSlowDeliveryDoesNotCauseACatchUpBurst(t *testing.T) {
	state, _ := testStore(t)
	server := newReceiver(t, 200, 2200*time.Millisecond)
	worker, err := Start(context.Background(), state, server.url)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	reason := model.BlockedReasonTimeout
	for i := 0; i < 4; i++ {
		putTask(t, state, "task-"+string(rune('0'+i)), "blocked", &reason)
	}
	server.next(t)
	server.next(t)
	previous := time.Now()
	for i := 0; i < 2; i++ {
		server.next(t)
		if elapsed := time.Since(previous); elapsed < 850*time.Millisecond {
			t.Fatalf("catch-up burst: %v between deliveries", elapsed)
		}
		previous = time.Now()
	}
}

func TestDeliveryTimeoutIsBoundedAndVisible(t *testing.T) {
	state, _ := testStore(t)
	server := newReceiver(t, 200, 60*time.Second)
	worker, err := Start(context.Background(), state, server.url)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	reason := model.BlockedReasonTimeout
	putTask(t, state, "task", "blocked", &reason)
	server.next(t)
	waitUntil(t, 15, func() bool {
		health, err := state.NotificationHealth()
		return err == nil && health.LastError != nil && *health.LastError == "timeout"
	}, "the bounded delivery timeout to surface on the destination")
}

func TestHeldHTTPShutdownLeavesTheClaimedRowForRecovery(t *testing.T) {
	state, path := testStore(t)
	server := newReceiver(t, 200, 60*time.Second)
	worker, err := Start(context.Background(), state, server.url)
	if err != nil {
		t.Fatal(err)
	}
	reason := model.BlockedReasonTimeout
	putTask(t, state, "task-1", "blocked", &reason)
	server.next(t)
	// While the delivery is held, ordinary store work is unblocked.
	if health, err := state.NotificationHealth(); err != nil || health.Pending != 1 {
		t.Fatalf("pending during held delivery: %+v %v", health, err)
	}
	worker.Stop()
	// The abandoned in-flight row stays claimed and retries with the same
	// event id once its rescheduled attempt is due.
	destination := queryDestination(t, path)
	before := outboxRows(t, path, "pending")
	if len(before) != 1 {
		t.Fatalf("pending rows: %d", len(before))
	}
	delivery, err := state.ClaimNotification(destination, time.Now().UTC().Add(31*time.Second))
	if err != nil || delivery == nil {
		t.Fatalf("abandoned claim: %v %v", delivery, err)
	}
	if delivery.EventID != before[0].EventID {
		t.Fatal("recovery after an ambiguous send must keep the same event id")
	}
}

func TestOversizedIdentitiesFailAsInvalidPayloadInsteadOfTruncating(t *testing.T) {
	state, _ := testStore(t)
	enabled(t, state)
	long := strings.Repeat("x", 300)
	putTask(t, state, long, "blocked", &[]model.BlockedReason{model.BlockedReasonTimeout}[0])
	delivery, err := state.ClaimNotification(dest, time.Now().UTC())
	if err != nil || delivery == nil {
		t.Fatalf("claim: %v %v", delivery, err)
	}
	if _, err := payload(delivery); err == nil {
		t.Fatal("oversized identity must fail as invalid payload")
	}
	if err := state.FinishNotificationFailure(delivery.Seq, "invalid_payload", nil, false); err != nil {
		t.Fatal(err)
	}
	if health, _ := state.NotificationHealth(); health.Failed != 1 {
		t.Fatalf("health: %+v", health)
	}
}

// rawDB exposes outbox fields that the health view aggregates away.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func outboxRows(t *testing.T, path, status string) []store.NotificationDelivery {
	t.Helper()
	db := rawDB(t, path)
	rows, err := db.Query("SELECT event_id FROM notification_outbox WHERE status=? ORDER BY seq", status)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var deliveries []store.NotificationDelivery
	for rows.Next() {
		var delivery store.NotificationDelivery
		if err := rows.Scan(&delivery.EventID); err != nil {
			t.Fatal(err)
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries
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

func queryDestination(t *testing.T, path string) string {
	t.Helper()
	var destination string
	if err := rawDB(t, path).QueryRow("SELECT destination_id FROM notification_policy WHERE id=1").Scan(&destination); err != nil {
		t.Fatal(err)
	}
	return destination
}

// syncBuffer collects the warnings the worker's loop writes.
type syncBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

// Outbox failures the loop cannot record are reported on the warning stream
// once per episode, never every tick, and never name the destination.
func TestStoreFailuresAreReportedOncePerEpisodeWithoutTheURL(t *testing.T) {
	state, path := testStore(t)
	server := newReceiver(t, 200, 0)
	var warnings syncBuffer
	worker, err := start(context.Background(), state, server.url, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	count := func() int { return strings.Count(warnings.String(), "WARN notifications: ") }
	db := rawDB(t, path)
	destination := queryDestination(t, path)
	// An attempt count that is not an integer makes every claim fail to read
	// the row.
	corrupt := func(eventID string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO notification_outbox
			(event_id,destination_id,created_at,repository,category,action,attempts,next_attempt_at)
			VALUES (?1,?2,strftime('%Y-%m-%dT%H:%M:%fZ','now'),'fixture/project','stale_base','inspect_task',1.5,0)`,
			eventID, destination); err != nil {
			t.Fatal(err)
		}
	}
	repair := func(eventID string) {
		t.Helper()
		if _, err := db.Exec("UPDATE notification_outbox SET attempts=0 WHERE event_id=?1", eventID); err != nil {
			t.Fatal(err)
		}
	}
	corrupt("first")
	waitUntil(t, 5, func() bool { return count() > 0 }, "the failing claim to be reported")
	// The loop retries every second; repeated failed ticks still make one line.
	time.Sleep(2200 * time.Millisecond)
	if count() != 1 || !strings.HasSuffix(warnings.String(), "\n") {
		t.Fatalf("repeated claim failure warnings: %q", warnings.String())
	}
	// A working claim ends the episode, so the same failure later is reported
	// again.
	repair("first")
	server.next(t)
	waitUntil(t, 5, func() bool {
		var delivered int
		err := db.QueryRow("SELECT count(*) FROM notification_outbox WHERE status='delivered'").Scan(&delivered)
		return err == nil && delivered == 1
	}, "the repaired row to be delivered")
	corrupt("second")
	waitUntil(t, 5, func() bool { return count() == 2 }, "the recurring claim failure to be reported")
	if lines := strings.Split(strings.TrimSuffix(warnings.String(), "\n"), "\n"); lines[0] != lines[1] {
		t.Fatalf("recurring failure: %q", lines)
	}
	// A delivery that cannot be recorded is reported too.
	if _, err := db.Exec(`CREATE TRIGGER refuse_delivered BEFORE UPDATE OF status ON notification_outbox
		WHEN NEW.status='delivered' BEGIN SELECT RAISE(ABORT,'synthetic finish failure'); END`); err != nil {
		t.Fatal(err)
	}
	repair("second")
	server.next(t)
	waitUntil(t, 5, func() bool { return count() == 3 }, "the failed delivery record to be reported")
	text := warnings.String()
	if !strings.Contains(text, "synthetic finish failure") {
		t.Fatalf("finish failure warning: %q", text)
	}
	if strings.Contains(text, server.url) || strings.Contains(text, server.server.Listener.Addr().String()) {
		t.Fatalf("warnings name the destination: %q", text)
	}
}
