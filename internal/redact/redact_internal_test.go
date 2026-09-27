package redact

import (
	"strings"
	"testing"
	"time"
)

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

func TestScrubPartsPreservesRedactionAcrossBoundaries(t *testing.T) {
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
		"PASS=correct horse battery staple":              "PASS=correct horse battery staple",
		"PASS=corr":                                      "PASS=corr",
		"KEY=s3cr3tValu":                                 "KEY=s3cr3tValu",
		"plain log line":                                 "plain log line",
		"":                                               "",
	} {
		if got := trimCutSecretEnd(input, values); got != want {
			t.Errorf("trimCutSecretEnd(%q) = %q; want %q", input, got, want)
		}
	}
}

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
		"correct horse battery staple":       "correct horse battery staple",
		"aple word":                          "aple word",
		"plain log line":                     "plain log line",
		"":                                   "",
	} {
		if got := trimCutSecretStart(input, values); got != want {
			t.Errorf("trimCutSecretStart(%q) = %q; want %q", input, got, want)
		}
	}
}

var fragmentValues = []string{
	"correct horse battery staple",
	"first-line-of-key\nsecond-line-of-key\nthird-line",
	"s3cr3tValue-0123456789",
	"opaque value then tokenbearer",
	"ends with a mark !",
	"invalid \xff",
}

func TestHeadLineCutDropsThePartialLastLine(t *testing.T) {
	for input, want := range map[string]string{
		"line one\nline two\nkey s3cr3tVal":                    "line one\nline two",
		"line one\n":                                           "line one",
		"word one https://bot:s3cr3tpass":                      "word one",
		"https://bot:s3cr3tpass":                               "",
		"done\nnext":                                           "done",
		"PASS=correct horse batt":                              "PASS=",
		"done\nKEY=first-line-of-key\nsecond-line-of-key\nthi": "done\nKEY=",
		"one unbroken":                                         "one",
		"unbroken":                                             "",
		"":                                                     "",
	} {
		if got := cutFragment(input, HeadLineCut, fragmentValues); got != want {
			t.Errorf("cutFragment(%q, HeadLineCut) = %q; want %q", input, got, want)
		}
	}
}

func TestHeadWordCutDropsThePartialLastWord(t *testing.T) {
	for input, want := range map[string]string{
		"denied for token ghp_Zq9Zq9Zq9Zq9Zq9Zq9Zq9": "denied for token",
		"body ends here":          "body ",
		"trailing ":               "trailing",
		"x correct horse batt":    "x ",
		"x correct horse battery": "x ",
		"unbroken":                "",
		"":                        "",
	} {
		if got := cutFragment(input, HeadWordCut, fragmentValues); got != want {
			t.Errorf("cutFragment(%q, HeadWordCut) = %q; want %q", input, got, want)
		}
	}
}

func TestTailTwoWordsCutDropsTheCutPrefix(t *testing.T) {
	for input, want := range map[string]string{
		"cut first line\nkept line":                            "kept line",
		"earer abcdefghijklmnop kept":                          "kept",
		"rrect horse battery staple kept words":                " kept words",
		"st-line-of-key\nsecond-line-of-key\nthird-line\nkept": "\nkept",
		"aa bb cc": "cc",
		"aa":       "",
		"unbroken": "",
		"":         "",
	} {
		if got := cutFragment(input, TailTwoWordsCut, fragmentValues); got != want {
			t.Errorf("cutFragment(%q, TailTwoWordsCut) = %q; want %q", input, got, want)
		}
	}
}

func TestTailLineCutDropsThePartialFirstLineAWindowCut(t *testing.T) {
	for _, test := range []struct {
		name string
		tail string
		want string
	}{
		{name: "cut inside a line", tail: "3cr3tVal\nline two\nline three\n", want: "line two\nline three\n"},
		{name: "one line with words", tail: "3cr3tpass word one two", want: "word one two"},
		{name: "one unbroken token", tail: "bot:s3cr3tpassword@github.com/x", want: ""},
		{name: "cut inside a character", tail: "\uFFFD\uFFFD rest\nnext \uFFFD", want: "next \uFFFD"},
		{name: "not a bearer prefix", tail: "number abcdefghijklmnop", want: "abcdefghijklmnop"},
		{name: "cut inside a bearer prefix", tail: "arer abcdefghijklmnop kept words", want: "kept words"},
		{name: "cut after a bearer prefix", tail: " abcdefghijklmnop kept words", want: "kept words"},
		{name: "bearer prefix before a line break", tail: "x Authorization: BEARER \n\n  abcdefghijklmnop\nkept\n", want: "kept\n"},
		{name: "cut bearer prefix before a line break", tail: "rer\nabcdefghijklmnop\nkept\n", want: "kept\n"},
		{name: "bearer token alone", tail: "rer abcdefghijklmnop", want: ""},
		{name: "whole bearer prefix kept", tail: "xx Bearer abcdefghijklmnop kept", want: "Bearer abcdefghijklmnop kept"},
		{name: "whole bearer prefix after a cut one", tail: "rer\nBearer abcdefghijklmnop kept", want: "kept"},
		{name: "cut escape sequence before a key", tail: "[2 qsk-abcdefghijklmnop kept", want: "kept"},
		{name: "cut escape sequence with spaced intermediates", tail: "\x1b ! Fsk-abcdefghijklmnop kept", want: "kept"},
		{name: "escape sequence after a cut bearer prefix", tail: "rer\n\x1b[2 qsk-abcdefghijklmnop\nkept\n", want: "kept\n"},
		{name: "key not after an escape", tail: "xx task-abcdefghijklmnop kept", want: "task-abcdefghijklmnop kept"},
		{name: "cut secret ending like a bearer prefix", tail: "que value then tokenbearer abcdefghijklmnop kept", want: "kept"},
		{name: "cut inside a passphrase", tail: "rse battery staple kept", want: "kept"},
		{name: "cut passphrase ending inside a URL credential", tail: "rrect horse battery staple://bot:s3cr3tpass@github.com/x kept", want: "kept"},
		{name: "cut inside a multi-line key", tail: "st-line-of-key\nsecond-line-of-key\nthird-line\nkept\n", want: "kept\n"},
		{name: "cut after a multi-line key's first line", tail: "cond-line-of-key\nthird-line\nkept\n", want: "kept\n"},
		{name: "only a cut secret's end", tail: "cond-line-of-key\nthird-line\n", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cutFragment(test.tail, TailLineCut, fragmentValues); got != test.want {
				t.Fatalf("cutFragment(%q, TailLineCut) = %q; want %q", test.tail, got, test.want)
			}
		})
	}
}

func TestTailLineCutStaysLinearInEscapeIntermediates(t *testing.T) {
	tail := "x " + strings.Repeat("! ", 64*1024/2-8) + "kept"
	start := time.Now()
	got := cutFragment(tail, TailLineCut, fragmentValues)
	if elapsed := time.Since(start); got != "kept" || elapsed > 5*time.Second {
		t.Fatalf("cutFragment(TailLineCut) = %.40q after %v; want %q within 5s", got, elapsed, "kept")
	}
}
