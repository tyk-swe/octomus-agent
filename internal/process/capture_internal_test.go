package process

import (
	"bytes"
	"strings"
	"testing"
)

// TestBoundedReadKeepsTheHeadAndTheRealEnd: a stream past its limit keeps its
// first limit bytes and its last TailLimit bytes in order, whatever the read
// sizes, and a stream within its limit never allocates the tail window.
func TestBoundedReadKeepsTheHeadAndTheRealEnd(t *testing.T) {
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

// TestTailWindowKeepsTheLastBytesAcrossWraps: writes of every size, including
// one larger than the window and ones that wrap the ring, leave exactly the
// last TailLimit bytes, oldest first, and reading them in between (which
// reorders the ring in place) leaves later writes correct.
func TestTailWindowKeepsTheLastBytesAcrossWraps(t *testing.T) {
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

// TestTailTextDropsThePartialFirstLineAWindowCut pins the rule every caller
// relies on before scrubbing the real end of a truncated capture: the window
// loses the line it began inside, or, when it holds no newline, the word, or
// everything when there is no whitespace either. The next word goes too when
// what was dropped could end a bearer prefix, whose token redaction would no
// longer recognise, and so do the remaining words or lines of an environment
// secret the window began inside (the passphrase and the multi-line key that
// TestMain exports). A complete capture has no tail text.
func TestTailTextDropsThePartialFirstLineAWindowCut(t *testing.T) {
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
		{name: "cut inside a passphrase", tail: "rse battery staple kept", want: "kept"},
		{name: "cut inside a multi-line key", tail: "st-line-of-key\nsecond-line-of-key\nthird-line\nkept\n", want: "kept\n"},
		{name: "cut after a multi-line key's first line", tail: "cond-line-of-key\nthird-line\nkept\n", want: "kept\n"},
		{name: "only a cut secret's end", tail: "cond-line-of-key\nthird-line\n", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			captured := Captured{Bytes: []byte("head line\npartial"), Truncated: true, tail: []byte(test.tail)}
			if got := captured.TailText(); got != test.want {
				t.Fatalf("TailText() = %q; want %q", got, test.want)
			}
			want := "head line\n[diagnostic output truncated]"
			if test.want != "" {
				want += "\n" + test.want
			}
			if got := captured.Preview(); got != want {
				t.Fatalf("Preview() = %q; want %q", got, want)
			}
		})
	}
	if got := (Captured{Bytes: []byte("whole\n"), tail: []byte("ignored\n")}).TailText(); got != "" {
		t.Fatalf("complete capture TailText() = %q; want none", got)
	}
}
