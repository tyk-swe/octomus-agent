package process

import (
	"bytes"
	"strings"
	"testing"
)

func TestSafeCapturesScrubsAcrossStreamsAndKeepsFragments(t *testing.T) {
	t.Parallel()
	const credential = "opaque-cross-stream-credential"
	for _, tc := range []struct {
		name           string
		output         ProcessOutput
		stdout, stderr SafeCapture
	}{
		{
			name:   "complete streams",
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("Authorization: Bearer ")}, Stderr: Captured{Bytes: []byte(credential + "\nSTDERR-END")}},
			stdout: SafeCapture{Head: "Authorization: [redacted]"},
			stderr: SafeCapture{Head: "\nSTDERR-END"},
		},
		{
			name: "retained tails",
			output: ProcessOutput{
				Stdout: Captured{Bytes: []byte("STDOUT-HEAD\ncut"), Truncated: true, tail: []byte("cut\nAuthorization: Bearer ")},
				Stderr: Captured{Bytes: []byte(credential + "\nSTDERR-HEAD\ncut"), Truncated: true, tail: []byte("cut\nSTDERR-END")},
			},
			stdout: SafeCapture{Head: "STDOUT-HEAD", Tail: "Authorization: [redacted]", Truncated: true},
			stderr: SafeCapture{Head: "\nSTDERR-HEAD", Tail: "STDERR-END", Truncated: true},
		},
		{
			name: "overlapping environment secret",
			// TestMain installs the full two-line value as an environment secret.
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("ghp_abcdefghijklmnop")}, Stderr: Captured{Bytes: []byte("sensitive-suffix\nSTDERR-END")}},
			stdout: SafeCapture{Head: "[redacted]"},
			stderr: SafeCapture{Head: "\nSTDERR-END"},
		},
		{
			name:   "token starts stderr after ordinary stdout",
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("progress")}, Stderr: Captured{Bytes: []byte("sk-abcdefghijklmnop\nSTDERR-END")}},
			stdout: SafeCapture{Head: "progress"},
			stderr: SafeCapture{Head: "[redacted]\nSTDERR-END"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeOut := bytes.Clone(tc.output.Stdout.Bytes)
			beforeErr := bytes.Clone(tc.output.Stderr.Bytes)
			stdout, stderr := tc.output.SafeCaptures()
			if stdout != tc.stdout || stderr != tc.stderr {
				t.Fatalf("safe captures = %+v, %+v; want %+v, %+v", stdout, stderr, tc.stdout, tc.stderr)
			}
			if !bytes.Equal(beforeOut, tc.output.Stdout.Bytes) || !bytes.Equal(beforeErr, tc.output.Stderr.Bytes) {
				t.Fatal("redaction modified the original capture")
			}
		})
	}
}

func TestSafeCapturesRedactsAdjacentEnvironmentSecretAcrossStreams(t *testing.T) {
	t.Parallel()
	// TestMain already installs this synthetic, newline-free environment secret.
	const secret = "s3cr3tValue-0123456789"
	for split := 1; split < len(secret); split++ {
		for _, truncated := range []bool{false, true} {
			stdout := Captured{Bytes: []byte("STDOUT-HEAD\n" + secret[:split])}
			if truncated {
				stdout = Captured{Bytes: []byte("STDOUT-HEAD\ncut"), Truncated: true, tail: []byte("cut\n" + secret[:split])}
			}
			output := ProcessOutput{Status: ExitStatus(Exit{Code: 3}), Stdout: stdout,
				Stderr: Captured{Bytes: []byte(secret[split:] + "\nSTDERR-END")}}
			out, stderr := output.SafeCaptures()
			want := SafeCapture{Head: "STDOUT-HEAD\n[redacted]"}
			if truncated {
				want = SafeCapture{Head: "STDOUT-HEAD", Tail: "[redacted]", Truncated: true}
			}
			if out != want || stderr != (SafeCapture{Head: "\nSTDERR-END"}) {
				t.Fatalf("split %d, truncated %t: adjacent secret survived: %+v, %+v", split, truncated, out, stderr)
			}
			failure := failureText("fixture", &output)
			wantFailure := "fixture exited with exit status: 3: " + joinPreview(want.Head, want.Tail, want.Truncated) + "\nSTDERR-END"
			if failure != wantFailure {
				t.Fatalf("split %d, truncated %t: command failure = %q; want %q", split, truncated, failure, wantFailure)
			}
		}
	}
}

func TestFailureTextRedactsAdjacentSecretBeforeElidingCapturedParts(t *testing.T) {
	t.Parallel()
	output := ProcessOutput{Status: ExitStatus(Exit{Code: 3}),
		Stdout: Captured{Bytes: []byte(strings.Repeat("progress\n", 3000) + "cut"), Truncated: true,
			tail: []byte("cut\nSTDOUT-END\ns3cr3tValue-")},
		Stderr: Captured{Bytes: []byte("0123456789\nSTDERR-END")}}
	text := failureText("fixture", &output)
	if len(text) > failureTextLimit || strings.Contains(text, "s3cr3tValue-") || strings.Contains(text, "0123456789") {
		t.Fatal("bounded command failure retained an adjacent secret fragment or exceeded its limit")
	}
	for _, want := range []string{"progress", diagnosticTruncatedMarker, "STDOUT-END", "[redacted]", "[stderr]", "STDERR-END"} {
		if !strings.Contains(text, want) {
			t.Errorf("bounded command failure lost %q", want)
		}
	}
}

func TestSafeCapturesKeepsSecretContextFromDiscardedCutLines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		output         ProcessOutput
		stdout, stderr SafeCapture
	}{
		{
			name: "stderr head discarded",
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("STDOUT-HEAD\ns3cr3tValue-")},
				Stderr: Captured{Bytes: []byte("0123456789" + strings.Repeat("x", DiagnosticLimit)), Truncated: true, tail: []byte("cut\nSTDERR-END")}},
			stdout: SafeCapture{Head: "STDOUT-HEAD\n[redacted]"},
			stderr: SafeCapture{Tail: "STDERR-END", Truncated: true},
		},
		{
			name: "stdout tail discarded",
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("STDOUT-HEAD\ncut"), Truncated: true, tail: []byte("cut-s3cr3tValue-")},
				Stderr: Captured{Bytes: []byte("0123456789\nSTDERR-END")}},
			stdout: SafeCapture{Head: "STDOUT-HEAD", Truncated: true},
			stderr: SafeCapture{Head: "[redacted]\nSTDERR-END"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := tc.output.SafeCaptures()
			if stdout != tc.stdout || stderr != tc.stderr {
				t.Fatalf("safe captures = %+v, %+v; want %+v, %+v", stdout, stderr, tc.stdout, tc.stderr)
			}
			failure := failureText("fixture", &tc.output)
			if strings.Contains(failure, "s3cr3tValue-") || strings.Contains(failure, "0123456789") {
				t.Fatal("command failure lost secret context at a capture cut")
			}
		})
	}
}

func TestBoundedReadKeepsTheHeadAndTheRealEnd(t *testing.T) {
	t.Parallel()
	var stream bytes.Buffer
	for i := 0; stream.Len() < 3*TailLimit; i++ {
		stream.WriteString(strings.Repeat(string(rune('a'+i%26)), i%97))
		stream.WriteByte('\n')
	}
	data := stream.Bytes()
	for _, limit := range []int{0, 10, len(data) - TailLimit - 1, len(data) - TailLimit, len(data) - 1} {
		captured, err := boundedRead(bytes.NewReader(data), limit)
		if err != nil {
			t.Fatal(err)
		}
		wantTail := data[max(limit, len(data)-TailLimit):]
		if !bytes.Equal(captured.Bytes, data[:limit]) || !captured.Truncated || !bytes.Equal(captured.tail, wantTail) {
			t.Fatalf("limit %d: kept %d head bytes and %d tail bytes; want %d and the last %d in order",
				limit, len(captured.Bytes), len(captured.tail), limit, len(wantTail))
		}
		if cap(captured.tail) > TailLimit {
			t.Fatalf("limit %d: tail window grew to %d bytes; want at most %d", limit, cap(captured.tail), TailLimit)
		}
	}
	captured, err := boundedRead(bytes.NewReader(data), len(data))
	if err != nil || captured.Truncated || captured.tail != nil || !bytes.Equal(captured.Bytes, data) {
		t.Fatalf("complete stream: truncated=%t, tail window %v, err %v; want the whole stream and no window",
			captured.Truncated, captured.tail != nil, err)
	}
}

func TestTailWindowKeepsTheLastBytesAcrossWraps(t *testing.T) {
	t.Parallel()
	var window tailWindow
	var written []byte
	for i, size := range []int{1, 8192, TailLimit - 1, 3, TailLimit + 5, 40000, 1, TailLimit, 12345} {
		chunk := bytes.Repeat([]byte{byte('A' + i)}, size)
		chunk[0] = '0' + byte(i)
		window.write(chunk)
		written = append(written, chunk...)
		want := written[max(len(written)-TailLimit, 0):]
		if got := window.bytes(); !bytes.Equal(got, want) {
			t.Fatalf("after write %d (%d bytes): window holds %d bytes, not the last %d in order", i, size, len(got), len(want))
		}
	}
}

func TestSafeTailTextDropsThePartialFirstLineAWindowCut(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		tail string
		want string
	}{
		{name: "cut inside a line", tail: "3cr3tVal\nline two\nline three\n", want: "line two\nline three\n"},
		{name: "one line with words", tail: "3cr3tpass word one two", want: "word one two"},
		{name: "one unbroken token", tail: "bot:s3cr3tpassword@github.com/x", want: ""},
		{name: "cut inside a character", tail: "\x82\xac rest\nnext \xff", want: "next �"},
		{name: "not a bearer prefix", tail: "number abcdefghijklmnop", want: "abcdefghijklmnop"},
		{name: "cut inside a bearer prefix", tail: "arer abcdefghijklmnop kept words", want: "kept words"},
		{name: "cut after a bearer prefix", tail: "  abcdefghijklmnop kept words", want: "kept words"},
		{name: "bearer prefix before a line break", tail: "x Authorization: BEARER \n\n  abcdefghijklmnop\nkept\n", want: "kept\n"},
		{name: "cut bearer prefix before a line break", tail: "rer\nabcdefghijklmnop\nkept\n", want: "kept\n"},
		{name: "bearer token alone", tail: "rer abcdefghijklmnop", want: ""},
		{name: "whole bearer prefix kept", tail: "xx Bearer abcdefghijklmnop kept", want: "[redacted] kept"},
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
			captured := Captured{Bytes: []byte("head line\npartial"), Truncated: true, tail: []byte(test.tail)}
			if got := captured.SafeTailText(); got != test.want {
				t.Fatalf("SafeTailText() = %q; want %q", got, test.want)
			}
			want := "head line\n[diagnostic output truncated]"
			if test.want != "" {
				want += "\n" + test.want
			}
			if got := captured.SafePreview(); got != want {
				t.Fatalf("SafePreview() = %q; want %q", got, want)
			}
		})
	}
	if got := (Captured{Bytes: []byte("whole\n"), tail: []byte("ignored\n")}).SafeTailText(); got != "" {
		t.Fatalf("complete capture SafeTailText() = %q; want none", got)
	}
}
