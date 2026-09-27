package redact_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/redact"
)

const (
	operatorToken = "synthetic-operator-access-value"
	webhookURL    = "https://hooks.example.invalid/T000/synthetic-path"
)

func TestMain(m *testing.M) {
	if err := os.Setenv(redact.TokenEnv, operatorToken); err != nil {
		panic(err)
	}
	if err := os.Setenv(redact.WebhookEnv, webhookURL); err != nil {
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

func TestPartsScrubsAcrossNormalizedCaptureCuts(t *testing.T) {
	parts := redact.Parts(
		redact.Part{Text: "prefix\nBearer \ncut", CutEnd: true},
		redact.Part{Text: "\n"},
		redact.Part{Text: "discarded\nopaque-credential kept", CutStart: true},
		redact.Part{Text: "unbroken", CutStart: true, Prefix: "must not appear"},
	)
	if !reflect.DeepEqual(parts, []string{"prefix\n[redacted]", "", " kept", ""}) {
		t.Fatalf("Parts lost cross-part context or exposed cut text: %q", parts)
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

func TestSkKeysMustStartAToken(t *testing.T) {
	for _, kept := range []string{
		"/opt/task-automation/bin/codex",
		"/srv/projects/task-management-app",
		"/home/me/desk-applications/codex",
		"Fix task-scheduling race",
		"Harden risk-assessment module",
		"Add disk-usage-reporting",
		"pip install flask-sqlalchemy-utils",
		"FIX TASK-SCHEDULING-RACE",
		"[Ask-the-maintainers-first] before merging",
	} {
		if got := redact.Secrets(kept); got != kept {
			t.Errorf("Secrets(%q) = %q; want it unchanged", kept, got)
		}
	}
	for input, want := range map[string]string{
		"sk-live0123456789abc":                         "[redacted]",
		"Use sk-live0123456789abc":                     "Use [redacted]",
		"OPENAI_API_KEY=sk-proj-abcdefghijklmnop":      "OPENAI_API_KEY=[redacted]",
		"key: 'sk-abcdefghijklmnop'":                   "key: '[redacted]'",
		`{"key":"sk-abcdefghijklmnop"}`:                `{"key":"[redacted]"}`,
		"line\nsk-abcdefghijklmnop":                    "line\n[redacted]",
		"KEY_sk-abcdefghijklmnop":                      "KEY_[redacted]",
		"key-sk-abcdefghijklmnop":                      "key-[redacted]",
		"v2sk-abcdefghijklmnop":                        "v2[redacted]",
		"(sk-abcdefghijklmnop)":                        "([redacted])",
		"https://api.example/?key=sk-abcdefghijklmnop": "https://api.example/?key=[redacted]",
		`"line\nsk-abcdefghijklmnop"`:                  `"line\n[redacted]"`,
		`"\u003esk-abcdefghijklmnop"`:                  `"\u003e[redacted]"`,
		"?next=%3Fkey%3Dsk-abcdefghijklmnop":           "?next=%3Fkey%3D[redacted]",
		"\x1b[32msk-abcdefghijklmnop\x1b[0m":           "\x1b[32m[redacted]\x1b[0m",
		`b'\x0bsk-abcdefghijklmnop'`:                   `b'\x0b[redacted]'`,
		"\r\x1b[2K\x1b[1Gsk-abcdefghijklmnop":          "\r\x1b[2K\x1b[1G[redacted]",
		"\x1b[?25hsk-abcdefghijklmnop":                 "\x1b[?25h[redacted]",
		`"\u001b[2Ksk-abcdefghijklmnop"`:               `"\u001b[2K[redacted]"`,
		`"\x1b[2Ksk-abcdefghijklmnop"`:                 `"\x1b[2K[redacted]"`,
		`printf '\e[2Ksk-abcdefghijklmnop'`:            `printf '\e[2K[redacted]'`,
		`echo "\033[Ksk-abcdefghijklmnop"`:             `echo "\033[K[redacted]"`,
		"\x1b[2 qsk-abcdefghijklmnop":                  "\x1b[2 q[redacted]",
		"\x1b(Bsk-abcdefghijklmnop":                    "\x1b(B[redacted]",
		"\x1b[m\x1b(Bsk-abcdefghijklmnop":              "\x1b[m\x1b(B[redacted]",
		"\x1b[m\x0f\x1b(Bsk-abcdefghijklmnop":          "\x1b[m\x0f\x1b(B[redacted]",
		"\x1bcsk-abcdefghijklmnop":                     "\x1bc[redacted]",
		"\x1bMsk-abcdefghijklmnop":                     "\x1bM[redacted]",
		`"\u001b(Bsk-abcdefghijklmnop"`:                `"\u001b(B[redacted]"`,
		`printf '\e(Bsk-abcdefghijklmnop'`:             `printf '\e(B[redacted]'`,
		`echo "\033(Bsk-abcdefghijklmnop"`:             `echo "\033(B[redacted]"`,
		`"\x1bcsk-abcdefghijklmnop"`:                   `"\x1bc[redacted]"`,
		"task-ghp_abcdefghijklmnop":                    "task-[redacted]",
		"xghp_abcdefghijklmnop":                        "x[redacted]",
		"Fix task-scheduling with sk-abcdefghijklmnop": "Fix task-scheduling with [redacted]",
	} {
		if got := redact.Secrets(input); got != want {
			t.Errorf("Secrets(%q) = %q; want %q", input, got, want)
		}
	}
}

func TestRedactsTokensUnicodeWhitespace(t *testing.T) {
	whitespace := "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	for _, separator := range whitespace {
		t.Run(fmt.Sprintf("U+%04X", separator), func(t *testing.T) {
			input := "before bEaReR" + string(separator) + "\t" + "synthetic-private-credential after"
			const want = "before [redacted] after"
			if got := redact.Secrets(input); got != want {
				t.Fatalf("Secrets = %q, want %q", got, want)
			}
			for _, url := range []string{
				"https://user" + string(separator) + "name:pass@example.com",
				"https://user:pass" + string(separator) + "word@example.com",
			} {
				if got := redact.Secrets(url); got != url {
					t.Fatalf("URL whitespace boundary changed: %q", got)
				}
			}
		})
	}
	for _, separator := range []rune{'\u001c', '\u180e', '\u200b', '\ufeff'} {
		input := "Bearer" + string(separator) + "synthetic-private-credential"
		if got := redact.Secrets(input); got != input {
			t.Errorf("non-whitespace U+%04X matched: %q", separator, got)
		}
	}
}

func TestDisplayJSONReportsEveryTransformedString(t *testing.T) {
	long := strings.Repeat("synthetic-", 2000)
	object := map[string]any{
		"nested": map[string]any{
			"inner":  map[string]any{"token": "value ghp_abcdefghijklmnop", "kept": "plain"},
			"listed": []any{"sk-abcdefghijklmnopqrstuvwxyz", "fine", long},
		},
		"other":   "untouched",
		"numeric": 7,
	}
	display, fields := redact.DisplayJSON(object)
	nested := display["nested"].(map[string]any)
	if nested["inner"].(map[string]any)["token"] != "value [redacted]" {
		t.Fatalf("nested map value: %v", nested["inner"])
	}
	listed := nested["listed"].([]any)
	if listed[0] != "[redacted]" || listed[1] != "fine" {
		t.Fatalf("nested list values: %v", listed)
	}
	shortened := listed[2].(string)
	if utf8.RuneCountInString(shortened) != 16384 || !utf8.ValidString(shortened) {
		t.Fatalf("shortened display text: %d runes", utf8.RuneCountInString(shortened))
	}
	if display["other"] != "untouched" || display["numeric"] != 7 {
		t.Fatalf("untouched values changed: %v", display)
	}
	if len(fields) != 1 {
		t.Fatalf("transform fields: %+v", fields)
	}
	entry := fields[0]
	if entry.Field != "nested" {
		t.Fatalf("transform field: %q", entry.Field)
	}
	if !reflect.DeepEqual(entry.Kinds, []string{"redacted", "shortened"}) {
		t.Fatalf("transform kinds: %v", entry.Kinds)
	}
	want := [][]any{
		{"nested", "inner", "token"},
		{"nested", "listed", 0},
		{"nested", "listed", 2},
	}
	if !reflect.DeepEqual(entry.Paths, want) {
		t.Fatalf("transform paths: %v", entry.Paths)
	}
	display, again := redact.DisplayJSON(map[string]any{"a": "plain", "b": []any{"also plain"}})
	if len(again) != 0 || display["a"] != "plain" {
		t.Fatalf("clean object: %v %+v", display, again)
	}
}

func TestDisplayJSONPathsFollowKeyAndIndexOrder(t *testing.T) {
	secrets := make([]any, 12)
	for i := range secrets {
		secrets[i] = fmt.Sprintf("sk-abcdefghijklmnop%02d", i)
	}
	object := map[string]any{
		"field": map[string]any{
			"b": map[string]any{"c": "ghp_abcdefghijklmnop", "a": []any{"plain", "ghp_abcdefghijklmnop"}},
			"a": secrets,
			"B": "ghp_abcdefghijklmnop",
		},
	}
	_, fields := redact.DisplayJSON(object)
	if len(fields) != 1 {
		t.Fatalf("transform fields: %+v", fields)
	}
	want := [][]any{{"field", "B"}}
	for i := range secrets {
		want = append(want, []any{"field", "a", i})
	}
	want = append(want, []any{"field", "b", "a", 1}, []any{"field", "b", "c"})
	if got := fields[0].Paths; !reflect.DeepEqual(got, want) {
		t.Fatalf("transform paths:\n got %v\nwant %v", got, want)
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
		{name: "environment value cut", input: "before " + operatorToken + "\npartial", kind: redact.HeadLineCut, want: "before [redacted]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := redact.Fragment(test.input, test.kind); got != test.want {
				t.Fatalf("Fragment(%q) = %q; want %q", test.input, got, test.want)
			}
		})
	}
}
