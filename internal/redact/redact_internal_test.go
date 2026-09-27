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

// Text cut back to whitespace inside a multi-word or multi-line secret value
// still ends with the value's complete first words or lines, which no longer
// match the whole value; they are removed. A value that ends the text whole is
// left for scrubbing, and text that stops inside a word is left as it is: the
// capture rule has already dropped such a partial word.
func TestTrimCutSecretDropsTheLeadingPartOfACutValue(t *testing.T) {
	values := []string{
		"correct horse battery staple",
		"first-line-of-key\nsecond-line-of-key\nthird-line",
		"s3cr3tValue-0123456789",
	}
	for input, want := range map[string]string{
		"PASS=correct horse": "PASS=",
		"PASS=correct":       "PASS=",
		"log\nKEY=first-line-of-key\nsecond-line-of-key": "log\nKEY=",
		"log\nKEY=first-line-of-key":                     "log\nKEY=",
		"first-line-of-key":                              "",
		// Nothing of a value is left before whitespace in it.
		"PASS=correct horse battery staple": "PASS=correct horse battery staple",
		"PASS=corr":                         "PASS=corr",
		"KEY=s3cr3tValu":                    "KEY=s3cr3tValu",
		"plain log line":                    "plain log line",
		"":                                  "",
	} {
		if got := trimCutSecretEnd(input, values); got != want {
			t.Errorf("trimCutSecretEnd(%q) = %q; want %q", input, got, want)
		}
	}
}

// Text whose start was cut inside a multi-word or multi-line secret value and
// then advanced past whitespace still starts with the value's complete last
// words or lines; they are removed. A value that starts the text whole is left
// for scrubbing, and text that starts inside a word is left as it is.
func TestTrimCutSecretStartDropsTheTrailingPartOfACutValue(t *testing.T) {
	values := []string{
		"correct horse battery staple",
		"first-line-of-key\nsecond-line-of-key\nthird-line",
		"invalid \xff",
	}
	for input, want := range map[string]string{
		"battery staple word":                " word",
		"staple\nnext":                       "\nnext",
		"second-line-of-key\nthird-line\nok": "\nok",
		"third-line":                         "",
		// Nothing of a value is left after whitespace in it.
		"correct horse battery staple": "correct horse battery staple",
		"aple word":                    "aple word",
		"plain log line":               "plain log line",
		"":                             "",
	} {
		if got := trimCutSecretStart(input, values); got != want {
			t.Errorf("trimCutSecretStart(%q) = %q; want %q", input, got, want)
		}
	}
}
