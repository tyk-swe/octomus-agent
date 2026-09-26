package redact

import "testing"

// Every token match and every occurrence of each secret value is found in the
// original text and overlapping ones are replaced together, so replacing one
// secret never splits another and leaves part of it visible.
func TestScrubRedactsOverlappingSecretsWhole(t *testing.T) {
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
		// One value inside another, whichever the environment lists first.
		"value abcdefgh-ZYXWVUTS end": "value [redacted] end",
		// Two values that overlap without either containing the other.
		"x abcdefgh12345678xyz y": "x [redacted] y",
		// A token match inside a value, and a value inside a token match.
		"POST https://alice:hunter22@hooks.example.com/private/route-7f3a failed": "POST [redacted] failed",
		"key sk-proj-ABCDEFGHIJKLMN end":                                          "key [redacted] end",
		"Bearer abcdefgh-ZYXWVUTS":                                                "[redacted]",
		// Overlapping occurrences of one value.
		"a repeatrepeatrepeat b": "a [redacted] b",
		// Separate and adjacent matches stay separate, as before.
		"a abcdefgh b abcdefgh c":                  "a [redacted] b [redacted] c",
		"abcdefghabcdefgh":                         "[redacted][redacted]",
		"ghp_abcdefghijklmnop sk-abcdefghijklmnop": "[redacted] [redacted]",
		"plain text with no secret":                "plain text with no secret",
		"":                                         "",
	} {
		if got := scrub(input, values); got != want {
			t.Errorf("scrub(%q) = %q; want %q", input, got, want)
		}
	}
}
