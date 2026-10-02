package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// Readers may return their last bytes and terminal error in the same Read.
// Reading again returns EOF, so callers must retain the original error.
type terminalBytesReader struct {
	input    *strings.Reader
	err      error
	reported bool
	reads    int
}

func (r *terminalBytesReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.input.Read(p)
	if r.input.Len() == 0 && !r.reported {
		r.reported = true
		return n, r.err
	}
	return n, err
}

func TestLineReaderTerminalErrorDoesNotDependOnReadBoundary(t *testing.T) {
	t.Parallel()
	failed := errors.New("fixture transport failed")
	for _, tc := range []struct {
		name, input string
		end         error
		lines       []string
	}{
		{"complete lines and error", "one\ntwo\n", failed, []string{"one", "two"}},
		{"error drops partial line", "one\npartial", failed, []string{"one"}},
		{"error without data", "", failed, nil},
		{"complete lines and EOF", "one\ntwo\n", io.EOF, []string{"one", "two"}},
		{"EOF keeps trailing line", "one\npartial", io.EOF, []string{"one", "partial"}},
		{"EOF without data", "", io.EOF, nil},
	} {
		for _, delivery := range []string{"same read", "separate read"} {
			t.Run(tc.name+"/"+delivery, func(t *testing.T) {
				combined := &terminalBytesReader{input: strings.NewReader(tc.input), err: tc.end}
				var input io.Reader = combined
				if delivery == "separate read" {
					input = io.MultiReader(strings.NewReader(tc.input), iotest.ErrReader(tc.end))
				}
				lines, err := readLines(t, input, 100)
				if !reflect.DeepEqual(lines, tc.lines) {
					t.Errorf("lines = %q; want %q", lines, tc.lines)
				}
				wantErr := tc.end
				if wantErr == io.EOF {
					wantErr = nil
				}
				if !errors.Is(err, wantErr) {
					t.Errorf("terminal error = %v; want %v", err, wantErr)
				}
				if delivery == "same read" && combined.reads != 1 {
					t.Errorf("read %d times after receiving terminal bytes; want one", combined.reads)
				}
			})
		}
	}
}

func TestSSETerminalErrorDoesNotDependOnReadBoundary(t *testing.T) {
	t.Parallel()
	failed := errors.New("fixture transport failed")
	for _, tc := range []struct {
		name, input string
		end         error
		values      []any
	}{
		{"complete frames and error", "data: 1\n\ndata: 2\n\n", failed, []any{json.Number("1"), json.Number("2")}},
		{"error drops partial frame", "data: 1\n\ndata: 2\n", failed, []any{json.Number("1")}},
		{"error without data", "", failed, nil},
		{"complete frames and EOF", "data: 1\n\ndata: 2\n\n", io.EOF, []any{json.Number("1"), json.Number("2")}},
		{"EOF drops partial frame", "data: 1\n\ndata: 2\n", io.EOF, []any{json.Number("1")}},
		{"EOF without data", "", io.EOF, nil},
	} {
		for _, delivery := range []string{"same read", "separate read"} {
			t.Run(tc.name+"/"+delivery, func(t *testing.T) {
				combined := &terminalBytesReader{input: strings.NewReader(tc.input), err: tc.end}
				var input io.Reader = combined
				if delivery == "separate read" {
					input = io.MultiReader(strings.NewReader(tc.input), iotest.ErrReader(tc.end))
				}
				results := sseResults(t, input)
				if len(results) != len(tc.values)+1 {
					t.Fatalf("got %d results; want %d complete frames and one terminal error", len(results), len(tc.values))
				}
				for i, want := range tc.values {
					if results[i].err != nil || !reflect.DeepEqual(results[i].value, want) {
						t.Errorf("frame %d = %+v; want %v before the error", i, results[i], want)
					}
				}
				last := results[len(results)-1]
				if tc.end == io.EOF {
					if last.err == nil || last.err.Error() != "OpenCode event stream disconnected" {
						t.Errorf("EOF result = %+v; want the disconnect error", last)
					}
				} else if !errors.Is(last.err, tc.end) {
					t.Errorf("terminal error = %v; want %v", last.err, tc.end)
				}
				if last.value != nil {
					t.Errorf("terminal error included a partial frame: %+v", last)
				}
				if delivery == "same read" && combined.reads != 1 {
					t.Errorf("read %d times after receiving terminal bytes; want one", combined.reads)
				}
			})
		}
	}
}

func TestCodexCompleteTurnPrecedesItsTerminalReadError(t *testing.T) {
	t.Parallel()
	const events = `{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"agentMessage","text":"Completed."}}}` + "\n" +
		`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}` + "\n"
	for _, terminal := range []error{io.EOF, errors.New("fixture transport failed after completion")} {
		t.Run(terminal.Error(), func(t *testing.T) {
			fixture := codexFixture(t)
			done := make(chan struct{})
			input := &terminalBytesReader{input: strings.NewReader(events), err: terminal}
			lines := lineReader(input, MaxMessage, done)
			t.Cleanup(func() {
				close(done)
				for range lines {
				}
			})
			client := Codex{ctx: context.Background(), state: fixture.state, entity: "fixture", lines: lines}
			deadline := time.Now().Add(10 * time.Second)
			if answer, err := client.awaitTurn("thread", "turn", deadline); err != nil || answer != "Completed." {
				t.Fatalf("complete turn = %q, %v; want its final answer before stream termination", answer, err)
			}
			wantErr := terminal
			if terminal == io.EOF {
				wantErr = errCodexDisconnected
			}
			if _, err := client.receive(deadline, "fixture read"); !errors.Is(err, wantErr) {
				t.Fatalf("next receive = %v; want %v", err, wantErr)
			}
			if _, err := client.receive(deadline, "fixture read"); !errors.Is(err, errCodexDisconnected) {
				t.Fatalf("terminal error repeated: %v", err)
			}
		})
	}
}
