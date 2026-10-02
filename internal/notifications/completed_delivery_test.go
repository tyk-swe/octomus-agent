package notifications

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

type cancelAfterResponseTransport struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (t cancelAfterResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err == nil {
		response.Body = cancelAfterResponseBody{ReadCloser: response.Body, cancel: t.cancel}
	}
	return response, err
}

type cancelAfterResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancelAfterResponseBody) Close() error {
	err := b.ReadCloser.Close()
	// Model shutdown after the worker has a definitive HTTP status but before
	// it records the result. No transport error or ambiguous response is involved.
	b.cancel()
	return err
}

func TestCompletedDeliverySurvivesShutdownBeforeRecording(t *testing.T) {
	t.Parallel()
	for _, check := range []struct {
		name     string
		status   int
		attempts int
		want     string
	}{
		{"success", http.StatusNoContent, 0, "delivered"},
		{"final_success", http.StatusNoContent, 4, "delivered"},
		{"terminal_failure", http.StatusBadRequest, 0, "failed"},
		{"retryable_failure", http.StatusServiceUnavailable, 0, "pending"},
		{"final_failure", http.StatusServiceUnavailable, 4, "failed"},
	} {
		t.Run(check.name, func(t *testing.T) {
			state, path := testStore(t)
			enabled(t, state)
			server := newReceiver(t, check.status, 0)
			reason := model.BlockedReasonTimeout
			putTask(t, state, "task-1", "blocked", &reason)
			if _, err := rawDB(t, path).Exec("UPDATE notification_outbox SET attempts=?", check.attempts); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := webhookClient()
			defer client.Transport.(*http.Transport).CloseIdleConnections()
			client.Transport = cancelAfterResponseTransport{base: client.Transport, cancel: cancel}
			worker := &Worker{store: state, url: server.url, destID: dest,
				client: client, ctx: ctx, warnings: io.Discard}
			worker.deliverNext()
			server.next(t)
			if ctx.Err() == nil {
				t.Fatal("response cleanup did not simulate shutdown")
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			var status string
			var attempts int
			var httpStatus *int
			var lastError *string
			if err := rawDB(t, path).QueryRow("SELECT status,attempts,http_status,last_error FROM notification_outbox").Scan(&status, &attempts, &httpStatus, &lastError); err != nil {
				t.Fatal(err)
			}
			if status != check.want || attempts != check.attempts+1 {
				t.Errorf("completed HTTP %d lost on shutdown: status=%s attempts=%d, want %s/%d", check.status, status, attempts, check.want, check.attempts+1)
			}
			if check.want == "delivered" {
				if httpStatus != nil || lastError != nil {
					t.Errorf("success retained failure metadata: %v %v", httpStatus, lastError)
				}
			} else if httpStatus == nil || *httpStatus != check.status || lastError == nil || *lastError != httpStatusCategory {
				t.Errorf("completed failure lost its HTTP result: status=%v error=%v", httpStatus, lastError)
			}
			claimed, err := reopened.ClaimNotification(dest, time.Now().UTC().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if (claimed != nil) != (check.want == "pending") {
				t.Fatalf("restarted claim after HTTP %d: %+v", check.status, claimed)
			}
		})
	}
}
