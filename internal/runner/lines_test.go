package runner

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// readLines drains a lineReader to its close and returns the lines it sent
// and the error that ended it, if any.
func readLines(t *testing.T, r io.Reader, limit int) ([]string, error) {
	t.Helper()
	done := make(chan struct{})
	defer close(done)
	ch := lineReader(r, limit, done)
	var lines []string
	var err error
	timeout := time.After(10 * time.Second)
	for {
		select {
		case result, ok := <-ch:
			if !ok {
				return lines, err
			}
			if err != nil {
				t.Fatalf("lineReader sent %q after its error %v", result.line, err)
			}
			if result.err != nil {
				err = result.err
				continue
			}
			lines = append(lines, string(result.line))
		case <-timeout:
			t.Fatal("lineReader did not close")
		}
	}
}

// A final line without a newline is still delivered at end of input, under
// the same per-line bound as every other line.
func TestLineReaderTrailingLine(t *testing.T) {
	boom := errors.New("connection reset")
	for _, tc := range []struct {
		name  string
		input io.Reader
		limit int
		lines []string
		err   string
	}{
		{"trailing line", iotest.OneByteReader(strings.NewReader("a\nb")), 16, []string{"a", "b"}, ""},
		{"trailing line at the bound", strings.NewReader("a\nab"), 2, []string{"a", "ab"}, ""},
		{"trailing line over the bound", iotest.OneByteReader(strings.NewReader("abc")), 2, nil, "line exceeds the 2 byte protocol limit"},
		{"terminated input", strings.NewReader("a\n"), 16, []string{"a"}, ""},
		{"read error drops the partial line", io.MultiReader(strings.NewReader("a\nb"), iotest.ErrReader(boom)), 16, []string{"a"}, boom.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, err := readLines(t, tc.input, tc.limit)
			if !reflect.DeepEqual(lines, tc.lines) {
				t.Fatalf("lines %q, want %q", lines, tc.lines)
			}
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || err.Error() != tc.err) {
				t.Fatalf("error %v, want %q", err, tc.err)
			}
		})
	}
}

// endless yields newline-terminated lines forever.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = "a\n"[i%2]
	}
	return len(p), nil
}

// Closing done abandons an endless stream: the reader stops sending and
// closes its channel instead of producing lines forever.
func TestLineReaderStopsWhenAbandoned(t *testing.T) {
	done := make(chan struct{})
	ch := lineReader(endless{}, 16, done)
	if result := <-ch; result.err != nil || string(result.line) != "a" {
		t.Fatalf("first line: %q %v", result.line, result.err)
	}
	close(done)
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("an abandoned lineReader must close")
		}
	}
}
