package store_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestRedactsTokens(t *testing.T) {
	redacted := store.Redact("Bearer secretkey123 ghp_abcdefghijklmnop")
	if strings.Contains(redacted, "secretkey") || strings.Contains(redacted, "ghp_abcdef") {
		t.Fatal(redacted)
	}
	if store.Redact("git clone https://user:pass@example.com/repo") == "git clone https://user:pass@example.com/repo" {
		t.Fatal("URL credentials survived")
	}
	value := store.RedactJSON(map[string]any{"nested": []any{"sk-abcdefghijklmnopqrstuvwxyz"}})
	if canonical(t, value) != `{"nested":["[redacted]"]}` {
		t.Fatal(canonical(t, value))
	}
}

func TestRedactsTokensUnicodeWhitespace(t *testing.T) {
	// Every Unicode White_Space code point, including ASCII vertical tab.
	whitespace := "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	for _, separator := range whitespace {
		t.Run(fmt.Sprintf("U+%04X", separator), func(t *testing.T) {
			input := "before bEaReR" + string(separator) + "\t" + "synthetic-private-credential after"
			const want = "before [redacted] after"
			if got := store.RedactSecrets(input); got != want {
				t.Fatalf("RedactSecrets = %q, want %q", got, want)
			}
			value, err := store.RedactedValue(map[string]any{"nested": []any{input}})
			must(t, err)
			if got := canonical(t, value); got != `{"nested":["before [redacted] after"]}` {
				t.Fatalf("redacted export = %s", got)
			}
			// URL userinfo cannot cross whitespace.
			for _, url := range []string{
				"https://user" + string(separator) + "name:pass@example.com",
				"https://user:pass" + string(separator) + "word@example.com",
			} {
				if got := store.RedactSecrets(url); got != url {
					t.Fatalf("URL whitespace boundary changed: %q", got)
				}
			}
		})
	}
	for _, separator := range []rune{'\u001c', '\u180e', '\u200b', '\ufeff'} {
		input := "Bearer" + string(separator) + "synthetic-private-credential"
		if got := store.RedactSecrets(input); got != input {
			t.Errorf("non-whitespace U+%04X matched: %q", separator, got)
		}
	}
}

// TestDisplayJSONReportsEveryTransformedString proves the display view walks
// nested maps and arrays, applies redaction before the length bound, and
// records each changed string by top-level field, kinds and JSON path so the
// settings contract can label previews precisely.
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
	display, fields := store.DisplayJSON(object)
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
	// Untouched objects report no transforms at all, and ordering is stable.
	display, again := store.DisplayJSON(map[string]any{"a": "plain", "b": []any{"also plain"}})
	if len(again) != 0 || display["a"] != "plain" {
		t.Fatalf("clean object: %v %+v", display, again)
	}
}
