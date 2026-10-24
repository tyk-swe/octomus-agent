package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
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
	messages := svc.DigestMessages(context.Background())
	if len(messages) != 1 || !strings.HasPrefix(messages[0], "Octomus digest (7 days)") {
		t.Fatalf("digest messages = %q", messages)
	}
}

func TestPackDigestSectionsKeepsFollowingRepositoryAfterOversizedSection(t *testing.T) {
	messages := packDigestSections([]string{
		"owner/huge — " + strings.Repeat("x", 5000),
		"owner/next — paused, cycle #1",
	})
	if len(messages) != 2 {
		t.Fatalf("packed digest = %d messages; want oversized section isolated from following repository", len(messages))
	}
	for i, message := range messages {
		if len(message) > maxTextLen {
			t.Fatalf("packed digest page %d is %d bytes; limit is %d", i+1, len(message), maxTextLen)
		}
	}
	if !strings.Contains(messages[1], "owner/next") {
		t.Fatalf("following repository missing after oversized section: %q", messages)
	}
}

func TestDigestIncludesEveryRepository(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	repos := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		repo := fmt.Sprintf("fixture/repository-number-%02d", i)
		repos = append(repos, repo)
		dataDir := "repos/" + store.RepositorySlug(repo)
		if i == 1 {
			dataDir = "."
		}
		if err := st.RegisterRepository(store.RepositoryEntry{GitHubRepo: repo, DataDir: dataDir}); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.GitHubRepo = repo
		scoped := st.ForRepository(repo)
		if err := store.Put(scoped, scoped.ConfigKey(), cfg); err != nil {
			t.Fatal(err)
		}
		if err := scoped.SaveControl(model.DefaultControl()); err != nil {
			t.Fatal(err)
		}
	}
	fleet, err := engine.NewFleet(st, t.TempDir(), engine.FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	svc := NewService(testConfig(), fleet, st, nil)

	messages := svc.DigestMessages(context.Background())
	if len(messages) < 2 {
		t.Fatalf("digest produced %d messages; want pagination for a 12-repository fleet", len(messages))
	}
	combined := strings.Join(messages, "\n")
	for _, repo := range repos {
		if !strings.Contains(combined, repo) {
			t.Fatalf("digest omits %s:\n%s", repo, combined)
		}
	}
	for i, message := range messages {
		if len(message) > maxTextLen {
			t.Fatalf("digest page %d is %d bytes; limit is %d", i+1, len(message), maxTextLen)
		}
		if strings.Contains(message, "truncated") {
			t.Fatalf("digest page %d truncates a repository section:\n%s", i+1, message)
		}
		if want := fmt.Sprintf("Page %d of %d", i+1, len(messages)); !strings.HasSuffix(message, want) {
			t.Fatalf("digest page %d does not end with %q:\n%s", i+1, want, message)
		}
	}

	// On demand, buttons traverse the same pages rather than losing the
	// repositories that do not fit in the first response.
	seen := ""
	resp, handled := svc.Handle(context.Background(), Invocation{
		Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Text: "digest",
	})
	if !handled {
		t.Fatal("digest command was not handled")
	}
	for page := 1; ; page++ {
		if resp.Text != messages[page-1] {
			t.Fatalf("digest response page %d = %q; want generated page %q", page, resp.Text, messages[page-1])
		}
		seen += resp.Text + "\n"
		var next string
		for _, button := range resp.Buttons {
			if button.Label == "Next page" {
				next = button.Value
			}
		}
		if next == "" {
			if page != len(messages) {
				t.Fatalf("digest navigation stopped at page %d of %d", page, len(messages))
			}
			break
		}
		resp, handled = svc.Handle(context.Background(), Invocation{
			Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Action: next,
		})
		if !handled {
			t.Fatalf("digest button %q was not handled", next)
		}
	}
	for _, repo := range repos {
		if !strings.Contains(seen, repo) {
			t.Fatalf("paged digest responses omit %s", repo)
		}
	}

	// A scheduled digest posts every generated page to alert channels.
	svc.cfg.Slack.AlertChannel = "C-ALERT"
	svc.cfg.Discord = nil
	var posted []string
	svc.sendDigest(context.Background(), func(_ context.Context, provider Provider, channel string, resp Response) error {
		if provider != SlackProvider || channel != "C-ALERT" {
			t.Fatalf("scheduled digest posted to %s/%s; want slack C-ALERT", provider, channel)
		}
		posted = append(posted, resp.Text)
		return nil
	})
	if len(posted) != len(messages) {
		t.Fatalf("scheduled digest posted %d messages; want %d", len(posted), len(messages))
	}
	for i := range messages {
		if posted[i] != messages[i] {
			t.Fatalf("scheduled digest page %d differs from generated page", i+1)
		}
	}
}
