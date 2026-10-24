package chat

import (
	"context"
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
