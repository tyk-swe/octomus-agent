package chat

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	testSlackUser   = "U-OPERATOR"
	testDiscordUser = "D-OPERATOR"
)

func TestBoundTextLimitPreservesUTF8(t *testing.T) {
	const suffix = "\n… (truncated)"
	tests := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{"empty", "", 0, ""},
		{"unicode unchanged", "  café 世界 😀  ", 100, "café 世界 😀"},
		{"unicode exact limit", "café", len("café"), "café"},
		{"ascii truncation", strings.Repeat("a", 30), 20, "aaaa" + suffix},
		{"reported boundary", strings.Repeat("a", 1883) + strings.Repeat("é", 20), 1900, strings.Repeat("a", 1883) + suffix},
		{"two byte split", "aé" + strings.Repeat("x", 20), len(suffix) + 2, "a" + suffix},
		{"three byte split first", "a界" + strings.Repeat("x", 20), len(suffix) + 2, "a" + suffix},
		{"three byte split second", "a界" + strings.Repeat("x", 20), len(suffix) + 3, "a" + suffix},
		{"four byte split first", "a😀" + strings.Repeat("x", 20), len(suffix) + 2, "a" + suffix},
		{"four byte split second", "a😀" + strings.Repeat("x", 20), len(suffix) + 3, "a" + suffix},
		{"four byte split third", "a😀" + strings.Repeat("x", 20), len(suffix) + 4, "a" + suffix},
		{"exact multibyte boundary", "a😀" + strings.Repeat("x", 20), len(suffix) + 5, "a😀" + suffix},
		{"no content code point fits", strings.Repeat("😀", 10), len(suffix) + 1, suffix},
		{"zero limit", "hello", 0, ""},
		{"newline only", "hello", 1, "\n"},
		{"suffix split first", "hello", 2, "\n"},
		{"suffix split second", "hello", 3, "\n"},
		{"suffix ellipsis fits", "hello", 4, "\n…"},
		{"exact suffix limit", strings.Repeat("x", 30), len(suffix), suffix},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundTextLimit(tt.text, tt.limit)
			if !utf8.ValidString(got) {
				t.Fatalf("output is invalid UTF-8: %q", got)
			}
			if len(got) > tt.limit {
				t.Fatalf("output is %d bytes, exceeding limit %d", len(got), tt.limit)
			}
			if got != tt.want {
				t.Fatalf("boundTextLimit = %q; want %q", got, tt.want)
			}
		})
	}
}

func testConfig() Config {
	return Config{
		Slack: &ProviderConfig{
			Provider:  SlackProvider,
			Operators: map[string]bool{testSlackUser: true},
			Bindings:  map[string]string{"C-BOUND": "acme/widgets"},
		},
		Discord: &ProviderConfig{
			Provider:  DiscordProvider,
			Operators: map[string]bool{testDiscordUser: true},
			Bindings:  map[string]string{},
		},
		Timezone: time.UTC,
	}
}

func testService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fleet, err := engine.NewFleet(st, t.TempDir(), engine.FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	return NewService(testConfig(), fleet, st, nil), st
}

func TestUnauthorizedSenderIgnored(t *testing.T) {
	svc, _ := testService(t)
	resp, handled := svc.Handle(context.Background(), Invocation{
		Provider: SlackProvider, UserID: "U-STRANGER", ChannelID: "C1", Text: "status",
	})
	if handled || resp.Text != "" {
		t.Fatalf("stranger got handled=%v resp=%+v; want silent ignore", handled, resp)
	}
	resp, handled = svc.Handle(context.Background(), Invocation{
		Provider: DiscordProvider, UserID: testSlackUser, ChannelID: "C1", Text: "help",
	})
	if handled {
		t.Fatalf("slack user ID authorized on discord: %+v", resp)
	}
}

func TestDuplicateEventExecutesOnce(t *testing.T) {
	svc, _ := testService(t)
	inv := Invocation{Provider: SlackProvider, EventID: "env-42", UserID: testSlackUser, ChannelID: "C1", Text: "help"}
	first, handled := svc.Handle(context.Background(), inv)
	if !handled || !strings.Contains(first.Text, "Octomus commands") {
		t.Fatalf("first delivery = handled %v text %q", handled, first.Text)
	}
	second, handled := svc.Handle(context.Background(), inv)
	if !handled || second.Text != "" || len(second.Buttons) != 0 {
		t.Fatalf("redelivery = handled %v resp %+v; want empty response", handled, second)
	}
}

func TestConfirmFlowPausesOnce(t *testing.T) {
	svc, _ := testService(t)
	ctx := context.Background()
	resp, handled := svc.Handle(ctx, Invocation{Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Text: "pause"})
	if !handled || len(resp.Buttons) != 2 || !strings.HasPrefix(resp.Buttons[0].Value, "confirm:") {
		t.Fatalf("pause = handled %v resp %+v; want a confirmation prompt", handled, resp)
	}
	token := resp.Buttons[0].Value
	// A different (but allowlisted) operator identity cannot confirm.
	resp, _ = svc.Handle(ctx, Invocation{Provider: DiscordProvider, UserID: testDiscordUser, ChannelID: "C1", Action: token})
	if !strings.Contains(resp.Text, "expired") {
		t.Fatalf("cross-user confirm = %q; want refusal", resp.Text)
	}
	// The cross-user attempt consumed nothing: the owner can still confirm.
	resp, _ = svc.Handle(ctx, Invocation{Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Action: token})
	if !strings.Contains(resp.Text, "paused") {
		t.Fatalf("confirm result = %q; want paused status", resp.Text)
	}
	// Tokens are one-shot.
	resp, _ = svc.Handle(ctx, Invocation{Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Action: token})
	if !strings.Contains(resp.Text, "expired") {
		t.Fatalf("reused confirm = %q; want refusal", resp.Text)
	}
	status, _ := svc.Handle(ctx, Invocation{Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Text: "status"})
	if !strings.Contains(status.Text, "paused") {
		t.Fatalf("status after pause = %q; want paused", status.Text)
	}
}

func TestUnknownRepoRejected(t *testing.T) {
	svc, _ := testService(t)
	resp, handled := svc.Handle(context.Background(), Invocation{
		Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Text: "status --repo ghost/nothing",
	})
	if !handled || !strings.Contains(resp.Text, "not served by this instance") {
		t.Fatalf("unknown repo = handled %v %q", handled, resp.Text)
	}
}

func TestParseCommandRepoFlag(t *testing.T) {
	verb, fields, rest, repo, err := parseCommand("request Fix the thing :: Details here --repo acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if verb != "request" || repo != "acme/widgets" || !strings.Contains(rest, "Fix the thing :: Details here") || strings.Contains(rest, "--repo") {
		t.Fatalf("parse = verb %q repo %q rest %q fields %v", verb, repo, rest, fields)
	}
	if _, _, _, _, err := parseCommand("status --bogus"); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestParseCommandPreservesRequestWhitespace(t *testing.T) {
	_, _, rest, _, err := parseCommand("request Title :: line one\n  indented code\n\ttabbed line")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rest, "line one\n  indented code\n\ttabbed line") {
		t.Fatalf("rest = %q; want newlines and indentation preserved", rest)
	}
	_, _, rest, repo, err := parseCommand("request Title :: first\nsecond --repo acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if repo != "acme/widgets" || strings.Contains(rest, "--repo") || !strings.Contains(rest, "first\nsecond") {
		t.Fatalf("rest = %q repo = %q; want the flag span removed and the newline kept", rest, repo)
	}
}

func TestRecordActionRejectsConflictingRepoSelection(t *testing.T) {
	svc, _ := testService(t)
	resp, handled := svc.Handle(context.Background(), Invocation{
		Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Text: "task missing-1 --repo ghost/nothing",
	})
	if !handled || !strings.Contains(resp.Text, "Record not found") {
		t.Fatalf("conflicting selection = handled %v %q; want owner-aware Record not found", handled, resp.Text)
	}
}

func TestAlertButtonsFitDiscordLimitForLongRepos(t *testing.T) {
	svc, _ := testService(t)
	repo := strings.Repeat("a", 30) + "/" + strings.Repeat("b", 30)
	id := "12345678-1234-1234-1234-123456789012"
	resp := svc.alertResponse(&store.NotificationDelivery{Repository: repo, Category: "timeout", TaskID: &id})
	if len(resp.Buttons) != 2 {
		t.Fatalf("buttons = %+v; want view + retry", resp.Buttons)
	}
	for _, b := range resp.Buttons {
		if len(b.Value) > 100 {
			t.Fatalf("button value %q is %d chars; Discord custom_id limit is 100", b.Value, len(b.Value))
		}
	}
}

func TestRequestTextKeepsEmbeddedFlags(t *testing.T) {
	verb, _, rest, _, err := parseCommand("request Fix help :: Make --help and --dry-run work")
	if err != nil {
		t.Fatalf("request with CLI flags in its description rejected: %v", err)
	}
	if verb != "request" || !strings.Contains(rest, "--help") || !strings.Contains(rest, "--dry-run") {
		t.Fatalf("verb=%q rest=%q; want the flags preserved as content", verb, rest)
	}
	if _, _, _, _, err := parseCommand("status --bogus"); err == nil {
		t.Fatal("unknown flag on a non-request command accepted")
	}
	// A --repo before the verb must not let the verb be located inside
	// the repository name.
	_, _, rest, repo, err := parseCommand("--repo acme/requests request Title :: Details")
	if err != nil {
		t.Fatal(err)
	}
	if repo != "acme/requests" || rest != "Title :: Details" {
		t.Fatalf("repo=%q rest=%q; want acme/requests and the exact title", repo, rest)
	}
}

func TestDedupeFailureBlocksDispatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fleet, err := engine.NewFleet(st, t.TempDir(), engine.FleetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fleet.Shutdown)
	svc := NewService(testConfig(), fleet, st, nil)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("DROP TABLE chat_events"); err != nil {
		t.Fatal(err)
	}
	resp, handled := svc.Handle(context.Background(), Invocation{
		Provider: SlackProvider, EventID: "env-dedupe-fail", UserID: testSlackUser, ChannelID: "C1", Text: "help",
	})
	if strings.Contains(resp.Text, "Octomus commands") {
		t.Fatalf("command dispatched despite dedupe failure: %q", resp.Text)
	}
	if !handled || resp.Text == "" {
		t.Fatalf("resp=%+v handled=%v; want a handled refusal", resp, handled)
	}
}

func TestTaskButtonRemembersContext(t *testing.T) {
	svc, st := testService(t)
	resp, handled := svc.Handle(context.Background(), Invocation{
		Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C-CTX", Action: "task:retry:task-ctx-1",
	})
	if !handled || len(resp.Buttons) != 2 {
		t.Fatalf("retry button = handled %v resp %+v; want a confirmation prompt", handled, resp)
	}
	got, err := st.ChatContext("slack", "C-CTX", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskID != "task-ctx-1" {
		t.Fatalf("remembered context = %+v; want the button's task remembered like a text command", got)
	}
}

func TestRunnerStopJoinsInflightHandlers(t *testing.T) {
	svc, _ := testService(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	svc.goHandle(func() {
		started <- struct{}{}
		<-release
	})
	<-started
	runner := &Runner{svc: svc, cancel: func() {}}
	stopped := make(chan struct{})
	go func() {
		runner.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a handler was still in flight")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the handler finished")
	}
}

func TestAttentionCountUsesStatusCountsNotPreview(t *testing.T) {
	view := &engine.StateView{Dashboard: store.Dashboard{
		Counts:         map[string]int64{"blocked": 4, "failed": 3, "executing": 9},
		AttentionTasks: make([]store.TaskSummary, 5), // the five-item preview
	}}
	if got := attentionCount(view); got != 7 {
		t.Fatalf("attentionCount = %d; want 7 from status counts, not the preview length", got)
	}
}

func TestFromEnvValidation(t *testing.T) {
	env := func(vars map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	}
	// Half-configured Slack is an error, never a silent half-enable.
	if _, err := FromEnv(env(map[string]string{"OCTOMUS_SLACK_BOT_TOKEN": "xoxb-x"})); err == nil {
		t.Fatal("slack bot token alone accepted")
	}
	// Tokens without an operator allowlist are refused: chat is operator-only.
	if _, err := FromEnv(env(map[string]string{
		"OCTOMUS_SLACK_APP_TOKEN": "xapp-x", "OCTOMUS_SLACK_BOT_TOKEN": "xoxb-x",
	})); err == nil {
		t.Fatal("slack tokens without operators accepted")
	}
	cfg, err := FromEnv(env(map[string]string{
		"OCTOMUS_SLACK_APP_TOKEN": "xapp-x", "OCTOMUS_SLACK_BOT_TOKEN": "xoxb-x",
		"OCTOMUS_SLACK_OPERATOR_IDS": "U1, U2", "OCTOMUS_SLACK_CHANNELS": "C1=acme/widgets",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled() || cfg.Discord != nil || cfg.Slack.Bindings["C1"] != "acme/widgets" || !cfg.Slack.Operators["U2"] {
		t.Fatalf("config = %+v", cfg.Slack)
	}

	tzCfg, err := FromEnv(env(map[string]string{"OCTOMUS_CHAT_TIMEZONE": "Europe/London"}))
	if err != nil {
		t.Fatal(err)
	}
	if tzCfg.Timezone == nil || tzCfg.Timezone.String() != "Europe/London" {
		t.Fatalf("timezone = %v; want Europe/London", tzCfg.Timezone)
	}
}

func TestRequestsPaginationKeepsEveryIDReachable(t *testing.T) {
	for i, fixture := range []struct{ title, target string }{
		{strings.Repeat("x", 200), "main"},
		{strings.Repeat("界", 66) + "ab", strings.Repeat("b", 200)},
		{strings.Repeat("界", 66) + "ab", strings.Repeat("branch", 1000)},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			title, target := fixture.title, fixture.target
			st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			cfg := config.Default()
			cfg.GitHubRepo = "fixture/requests"
			if err := store.Put(st, st.ConfigKey(), cfg); err != nil {
				t.Fatal(err)
			}
			if err := st.RegisterRepository(store.RepositoryEntry{GitHubRepo: cfg.GitHubRepo, DataDir: "."}); err != nil {
				t.Fatal(err)
			}
			fleet, err := engine.NewFleet(st, t.TempDir(), engine.FleetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(fleet.Shutdown)
			svc := NewService(testConfig(), fleet, st, nil)
			var rows []string
			for i := 0; i < model.RequestPendingLimit+defaultList+1; i++ {
				status := model.RequestPending
				if i >= model.RequestPendingLimit {
					status = model.RequestWithdrawn
				}
				r := model.OperatorRequest{ID: model.ID(), Repository: cfg.GitHubRepo, Title: title, Target: target, Status: status, CreatedAt: model.Now(), UpdatedAt: model.Now()}
				if err := store.Put(st, store.Requests.Key(r.ID), r); err != nil {
					t.Fatal(err)
				}
				if status == model.RequestPending {
					displayTarget := target
					if len(target) > 200 {
						displayTarget = target[:197] + "..."
					}
					rows = append(rows, fmt.Sprintf("- %s %s (target %s)", r.ID, title, displayTarget))
				} else if i > model.RequestPendingLimit {
					rows = append(rows, fmt.Sprintf("- %s [%s] %s", r.ID, status, title))
				}
			}
			inv := Invocation{Provider: SlackProvider, UserID: testSlackUser, ChannelID: "C1", Text: "requests"}
			first, _ := svc.Handle(context.Background(), inv)
			var page, total int
			for _, line := range strings.Split(first.Text, "\n") {
				if strings.HasPrefix(line, "Page ") {
					if _, err := fmt.Sscanf(line, "Page %d of %d", &page, &total); err != nil {
						t.Fatal(err)
					}
				}
			}
			if page != 1 || total < 2 {
				t.Fatalf("full queue was not paginated: %q", first.Text)
			}
			inv.ChannelID = "C-BOUND"
			var messages []string
			for n := 1; n <= total; n++ {
				inv.Text = fmt.Sprintf("requests %d --repo %s", n, cfg.GitHubRepo)
				resp, handled := svc.Handle(context.Background(), inv)
				if !handled || len(resp.Text) > maxTextLen || !utf8.ValidString(resp.Text) || strings.Contains(resp.Text, "truncated") {
					t.Fatalf("page %d is not a complete bounded response: %+v", n, resp)
				}
				for _, want := range []string{"Work requests for " + cfg.GitHubRepo, fmt.Sprintf("Page %d of %d", n, total), "requests <page> --repo " + cfg.GitHubRepo} {
					if !strings.Contains(resp.Text, want) {
						t.Fatalf("page %d lacks %q: %q", n, want, resp.Text)
					}
				}
				if n == 1 && resp.Text != first.Text {
					t.Fatal("default and explicit first pages differ")
				}
				messages = append(messages, resp.Text)
			}
			combined := strings.Join(messages, "\n")
			if strings.Count(combined, "\n- ") != len(rows) || !strings.Contains(combined, "Recent:") {
				t.Fatalf("unexpected pending/recent rows: %q", combined)
			}
			for _, row := range rows {
				if strings.Count(combined, row+"\n") != 1 {
					t.Fatalf("row is missing, duplicated or incomplete: %q", row)
				}
			}
			for _, input := range []string{"0", "-1", "bad", "1.5", "1 2", "99999999999999999999999999", fmt.Sprint(total + 1)} {
				inv.Text = "requests " + input
				resp, _ := svc.Handle(context.Background(), inv)
				if !strings.Contains(resp.Text, "page") || strings.Contains(resp.Text, "\n- ") || len(resp.Buttons) != 0 {
					t.Fatalf("invalid page %q was not rejected: %+v", input, resp)
				}
			}
		})
	}
}

func TestRequestsEmptyQueuePages(t *testing.T) {
	svc, _ := testService(t)
	for _, command := range []string{"requests", "requests 1", "requests 2"} {
		resp, _ := svc.Handle(context.Background(), Invocation{Provider: SlackProvider, UserID: testSlackUser, Text: command})
		want := "No work requests"
		if command == "requests 2" {
			want = "has 1 page(s)"
		}
		if !strings.Contains(resp.Text, want) {
			t.Fatalf("%s = %q, want %q", command, resp.Text, want)
		}
	}
}
