// Secret redaction from the environment, token patterns and cut fragments.

package redact_test

import (
	"encoding/json"
	"errors"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"os"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/redact"
)

const (
	operatorToken = "synthetic-operator-access-value"
	webhookURL    = "https://hooks.example.invalid/T000/synthetic-path"
)

func TestMain(m *testing.M) {
	if err := os.Setenv(config.TokenEnv, operatorToken); err != nil {
		panic(err)
	}
	if err := os.Setenv(config.WebhookEnv, webhookURL); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestScrubsSecretEnvironmentValues(t *testing.T) {
	for _, value := range []string{operatorToken, webhookURL} {
		if got := redact.Secrets("before " + value + " after"); got != "before [redacted] after" {
			t.Fatalf("Secrets(%q) = %q", value, got)
		}
		if got := redact.Error(errors.New("failed: " + value)); got != "failed: [redacted]" {
			t.Fatalf("Error(%q) = %q", value, got)
		}
	}
}

func canonical(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRedactsTokens(t *testing.T) {
	redacted := redact.Text("Bearer secretkey123 ghp_abcdefghijklmnop")
	if strings.Contains(redacted, "secretkey") || strings.Contains(redacted, "ghp_abcdef") {
		t.Fatal(redacted)
	}
	if redact.Text("git clone https://user:pass@example.com/repo") == "git clone https://user:pass@example.com/repo" {
		t.Fatal("URL credentials survived")
	}
	value := redact.JSON(map[string]any{"nested": []any{"sk-abcdefghijklmnopqrstuvwxyz"}})
	if canonical(t, value) != `{"nested":["[redacted]"]}` {
		t.Fatal(canonical(t, value))
	}
}

func TestFragmentScrubsCutBoundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		kind  redact.FragmentKind
		want  string
	}{
		{name: "head line", input: "Bearer abcdefghijklmnop partial\nlast line", kind: redact.HeadLineCut, want: "[redacted] partial"},
		{name: "head word", input: "denied for ghp_abcdefghijklmnop", kind: redact.HeadWordCut, want: "denied for"},
		{name: "tail line", input: "xx Bearer abcdefghijklmnop kept", kind: redact.TailLineCut, want: "[redacted] kept"},
		{name: "tail two words", input: "cut\nBearer abcdefghijklmnop kept", kind: redact.TailTwoWordsCut, want: "[redacted] kept"},
		{name: "tail two words after a cut bearer line", input: "arer\nabcdefghijklmnop kept", kind: redact.TailTwoWordsCut, want: "kept"},
		{name: "environment value cut", input: "before " + operatorToken + "\npartial", kind: redact.HeadLineCut, want: "before [redacted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := redact.Fragment(test.input, test.kind); got != test.want {
				t.Fatalf("Fragment(%q) = %q; want %q", test.input, got, test.want)
			}
		})
	}
}

func TestJSONDiscardsAnnotatedCutFragments(t *testing.T) {
	for _, fragment := range []string{
		"ghp_abcdef",
		"https://owner:partial-password",
		"unknown-old-credential-prefix",
		operatorToken[:12],
	} {
		value := map[string]any{"nested": []any{map[string]any{
			"title": "Known context " + fragment, "title_truncated": true,
			"body": "Retained paragraph.\n" + fragment, "body_truncated": true,
		}}}
		shown := redact.JSON(value).(map[string]any)["nested"].([]any)[0].(map[string]any)
		if shown["title"] != "Known context" || shown["body"] != "Retained paragraph." {
			t.Fatalf("cut fragment %q survived or useful complete text was lost: %v", fragment, shown)
		}
		if shown["title_truncated"] != true || shown["body_truncated"] != true {
			t.Fatal("presentation lost its evidence that source text was omitted")
		}
	}
	uncut := map[string]any{"title": "A complete title", "title_truncated": false, "body": "A complete body", "body_truncated": false}
	if got := canonical(t, redact.JSON(uncut)); got != `{"body":"A complete body","body_truncated":false,"title":"A complete title","title_truncated":false}` {
		t.Fatalf("uncut fields changed: %s", got)
	}
}
