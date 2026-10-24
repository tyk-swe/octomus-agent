package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestNotifyWorkerDeliversAlertOnce(t *testing.T) {
	svc, st := testService(t)
	dest := store.ChatDestination{ID: DestinationID(SlackProvider, "C-ALERT"), Kind: "slack", Target: "C-ALERT"}
	if err := st.SetChatDestinations([]store.ChatDestination{dest}); err != nil {
		t.Fatal(err)
	}
	task := map[string]any{
		"id": "alert-task-1", "cycle_id": "cycle-1", "status": "blocked",
		"blocked_reason": "timeout",
		"config":         map[string]any{"github_repo": "fixture/project"},
	}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(st, store.Tasks.Key("alert-task-1").Raw(), data); err != nil {
		t.Fatal(err)
	}
	var posted []Response
	worker := &notifyWorker{
		svc: svc, st: st, dest: dest, provider: SlackProvider,
		post: func(_ context.Context, _ Provider, _ string, resp Response) error {
			posted = append(posted, resp)
			return nil
		},
	}
	worker.deliverNext(context.Background())
	if len(posted) != 1 || !strings.Contains(posted[0].Text, "needs attention (timeout)") {
		t.Fatalf("posted = %+v; want one timeout alert", posted)
	}
	if len(posted[0].Buttons) != 2 || !strings.HasPrefix(posted[0].Buttons[1].Value, "task:retry:") {
		t.Fatalf("alert buttons = %+v; want view + retry", posted[0].Buttons)
	}
	// Delivered rows are not claimed again.
	worker.deliverNext(context.Background())
	if len(posted) != 1 {
		t.Fatalf("posted %d alerts; want exactly one", len(posted))
	}
}

func TestNotifyWorkerRetriesRetryableFailure(t *testing.T) {
	svc, st := testService(t)
	dest := store.ChatDestination{ID: DestinationID(DiscordProvider, "999"), Kind: "discord", Target: "999"}
	if err := st.SetChatDestinations([]store.ChatDestination{dest}); err != nil {
		t.Fatal(err)
	}
	task := map[string]any{
		"id": "alert-task-2", "cycle_id": "cycle-1", "status": "published",
		"config": map[string]any{"github_repo": "fixture/project"},
	}
	data, _ := json.Marshal(task)
	if err := store.Put(st, store.Tasks.Key("alert-task-2").Raw(), data); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	worker := &notifyWorker{
		svc: svc, st: st, dest: dest, provider: DiscordProvider,
		post: func(_ context.Context, _ Provider, _ string, resp Response) error {
			attempts++
			if attempts == 1 {
				return &apiError{Status: 429, Msg: "slow down"}
			}
			return nil
		},
	}
	worker.deliverNext(context.Background())
	// The failed attempt backs off, so an immediate re-claim finds nothing
	// due; claim with a future clock to prove the row stayed pending.
	svc.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	worker.deliverNext(context.Background())
	if attempts != 2 {
		t.Fatalf("attempts = %d; want failure then success", attempts)
	}
}

func TestNotifyWorkerWarnsDeliveryError(t *testing.T) {
	svc, st := testService(t)
	var warnings bytes.Buffer
	svc.warnings = &warnings
	dest := store.ChatDestination{ID: DestinationID(SlackProvider, "C-BROKEN"), Kind: "slack", Target: "C-BROKEN"}
	if err := st.SetChatDestinations([]store.ChatDestination{dest}); err != nil {
		t.Fatal(err)
	}
	task := map[string]any{
		"id": "alert-task-3", "cycle_id": "cycle-1", "status": "blocked",
		"blocked_reason": "timeout",
		"config":         map[string]any{"github_repo": "fixture/project"},
	}
	data, _ := json.Marshal(task)
	if err := store.Put(st, store.Tasks.Key("alert-task-3").Raw(), data); err != nil {
		t.Fatal(err)
	}
	worker := &notifyWorker{
		svc: svc, st: st, dest: dest, provider: SlackProvider,
		post: func(_ context.Context, _ Provider, _ string, resp Response) error {
			return &apiError{Status: 403, Msg: "slack refused: channel_not_found"}
		},
	}
	worker.deliverNext(context.Background())
	if !strings.Contains(warnings.String(), "channel_not_found") {
		t.Fatalf("warnings = %q; want the original delivery error visible to the operator", warnings.String())
	}
}

func TestRunnerPostHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	discord := newDiscordAdapter(nil, &ProviderConfig{Provider: DiscordProvider, BotToken: "tok"})
	discord.apiBase = srv.URL
	runner := &Runner{discord: discord}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := runner.post(ctx, DiscordProvider, "123", Response{Text: "alert"})
	if err == nil {
		t.Fatal("cancelled proactive post reported success")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("proactive post ignored cancellation for %v", elapsed)
	}
}

func TestDigestRenders(t *testing.T) {
	svc, _ := testService(t)
	text := svc.Digest(context.Background())
	if !strings.HasPrefix(text, "Octomus digest (7 days)") {
		t.Fatalf("digest = %q", text)
	}
}
