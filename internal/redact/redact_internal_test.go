package redact

import "testing"

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
