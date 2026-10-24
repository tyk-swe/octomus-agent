package chat

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	testSlackUser   = "U-OPERATOR"
	testDiscordUser = "D-OPERATOR"
)

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
}
