// The scrubber on overlapping secrets and on text parts cut at capture boundaries.

package redact

import (
	"strings"
	"testing"
)

func TestScrubOverlappingSecrets(t *testing.T) {
	values := []string{
		"abcdefgh",
		"abcdefgh-ZYXWVUTS",
		"abcdefgh12",
		"12345678xyz",
		"sk-proj-ABCDEFGHIJ",
		"https://alice:hunter22@hooks.example.com/private/route-7f3a",
		"repeatrepeat",
	}
	for input, want := range map[string]string{
		"value abcdefgh-ZYXWVUTS end": "value [redacted] end",
		"x abcdefgh12345678xyz y":     "x [redacted] y",
		"POST https://alice:hunter22@hooks.example.com/private/route-7f3a failed": "POST [redacted] failed",
		"key sk-proj-ABCDEFGHIJKLMN end":                                          "key [redacted] end",
		"Bearer abcdefgh-ZYXWVUTS":                                                "[redacted]",
		"a repeatrepeatrepeat b":                                                  "a [redacted] b",
		"a abcdefgh b abcdefgh c":                                                 "a [redacted] b [redacted] c",
		"abcdefghabcdefgh":                                                        "[redacted][redacted]",
		"ghp_abcdefghijklmnop sk-abcdefghijklmnop":                                "[redacted] [redacted]",
		"plain text with no secret":                                               "plain text with no secret",
		"":                                                                        "",
	} {
		if got := scrub(input, values); got != want {
			t.Errorf("scrub(%q) = %q; want %q", input, got, want)
		}
	}
}

func TestScrubParts(t *testing.T) {
	for input, want := range map[string]string{
		"before ghp_abcdefghijklmnop\nsensitive-suffix after": "before [redacted] after",
		"before sensitive-prefix\nghp_abcdefghijklmnop after": "before [redacted] after",
		"Authorization: Bearer opaque-credential":             "Authorization: [redacted]",
		"界 ghp_abcdefghijklmnop sk-abcdefghijklmnop fin":      "界 [redacted] [redacted] fin",
		"plain text": "plain text",
		"":           "",
	} {
		for split := 0; split <= len(input); split++ {
			parts := []string{input[:split], "", input[split:]}
			got := scrubParts(parts, []string{"ghp_abcdefghijklmnop\nsensitive-suffix", "sensitive-prefix\nghp_abcdefghijklmnop"})
			if len(got) != 3 || strings.Join(got, "") != want {
				t.Fatalf("scrubbing parts at boundary %d changed the redaction or lost context", split)
			}
		}
	}
}
