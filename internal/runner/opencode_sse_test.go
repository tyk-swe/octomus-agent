package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// sseResults runs sseLoop to its end over body and returns everything it
// sent, in order.
func sseResults(t *testing.T, body io.Reader) []valueResult {
	t.Helper()
	out := make(chan valueResult, 64)
	finished := make(chan struct{})
	go func() {
		sseLoop(context.Background(), body, out)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("sseLoop did not end")
	}
	close(out)
	var results []valueResult
	for result := range out {
		results = append(results, result)
	}
	return results
}

// Each frame's data lines are joined and decoded once its blank line
// arrives, however the stream is fragmented: CRLF or LF endings, comments and
// other fields ignored, "data:" with or without its space, and a multibyte
// character split across reads. The loop then ends with exactly one error.
func TestSSEFraming(t *testing.T) {
	boom := errors.New("connection reset")
	for _, tc := range []struct {
		name   string
		body   io.Reader
		values []any
		err    string // the final error's text, or for a wrapped error its prefix
	}{
		{
			name: "mixed framing one byte at a time",
			body: iotest.OneByteReader(strings.NewReader(
				": ping\r\n\r\n" +
					"event: x\r\ndata: {\"a\":\r\ndata:1}\r\n\r\n" +
					"data: 2\r\n\r\n" +
					"id: 3\nretry: 10\n\n" +
					"id: 4\ndata: \"\u00e9\"\n\n")),
			values: []any{map[string]any{"a": json.Number("1")}, json.Number("2"), "\u00e9"},
			err:    "OpenCode event stream disconnected",
		},
		{
			name:   "a frame without its blank line is not dispatched",
			body:   strings.NewReader("data: 1\n\ndata: 2\n"),
			values: []any{json.Number("1")},
			err:    "OpenCode event stream disconnected",
		},
		{
			name: "invalid JSON",
			body: strings.NewReader("data: x\n\ndata: 1\n\n"),
			err:  "Invalid OpenCode event JSON: ",
		},
		{
			name: "invalid UTF-8",
			body: strings.NewReader("data: \"\xff\"\n\n"),
			err:  "Invalid OpenCode event encoding",
		},
		{
			name:   "a read error ends the stream as itself",
			body:   io.MultiReader(strings.NewReader("data: 1\n\ndata: 2\n"), iotest.ErrReader(boom)),
			values: []any{json.Number("1")},
			err:    boom.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := sseResults(t, tc.body)
			if len(results) != len(tc.values)+1 {
				sent := make([]string, len(results))
				for i, result := range results {
					sent[i] = fmt.Sprintf("%#v %v", result.value, result.err)
				}
				t.Fatalf("sent %q, want %d values and one error", sent, len(tc.values))
			}
			for i, want := range tc.values {
				if results[i].err != nil || !reflect.DeepEqual(results[i].value, want) {
					t.Fatalf("result %d: %#v %v, want %#v", i, results[i].value, results[i].err, want)
				}
			}
			if last := results[len(results)-1]; last.err == nil || !strings.HasPrefix(last.err.Error(), tc.err) || last.value != nil {
				t.Fatalf("final result: %#v %v, want error %q", last.value, last.err, tc.err)
			}
		})
	}
}

// A cancelled owner never blocks the loop on a send nobody receives, and the
// loop does not close the channel it was given.
func TestSSELoopEndsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan valueResult)
	finished := make(chan struct{})
	go func() {
		sseLoop(ctx, strings.NewReader("data: 1\n\n"), out)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled sseLoop must not wait for a receiver")
	}
	select {
	case result, ok := <-out:
		t.Fatalf("a cancelled sseLoop sent %+v (open: %v)", result, ok)
	default:
	}
}
