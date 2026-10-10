package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// lineSplitter cuts a stream into lines for a reader that bounds each one.
type lineSplitter struct {
	r       io.Reader
	backlog []byte
	buf     []byte
	readErr error
}

var errLineLimit = errors.New("line exceeds the protocol limit")

// next returns the next line without its newline. At the end of the stream it returns the error that ended it, io.EOF
// for a clean end, with any unterminated rest as the line; the bytes a read returned beside its error come first. A
// line longer than limit bytes ends the stream with errLineLimit.
func (s *lineSplitter) next(limit int) ([]byte, error) {
	for {
		if end := bytes.IndexByte(s.backlog, '\n'); end >= 0 {
			if end > limit {
				return nil, errLineLimit
			}
			line := s.backlog[:end]
			s.backlog = s.backlog[end+1:]
			return line, nil
		}
		if len(s.backlog) > limit {
			return nil, errLineLimit
		}
		if s.readErr != nil {
			rest := s.backlog
			s.backlog = nil
			return rest, s.readErr
		}
		if s.buf == nil {
			s.buf = make([]byte, 32768)
		}
		var n int
		n, s.readErr = s.r.Read(s.buf)
		s.backlog = append(s.backlog, s.buf[:n]...)
	}
}

type lineResult struct {
	line []byte
	err  error
}

// lineReader delivers each line of r, at most limit bytes long, on the returned channel until the stream or done
// ends, then closes it.
func lineReader(r io.Reader, limit int, done <-chan struct{}) chan lineResult {
	ch := make(chan lineResult)
	go func() {
		defer close(ch)
		send := func(result lineResult) bool {
			select {
			case ch <- result:
				return true
			case <-done:
				return false
			}
		}
		split := lineSplitter{r: r}
		for {
			line, err := split.next(limit)
			switch {
			case err == errLineLimit:
				send(lineResult{err: fmt.Errorf("line exceeds the %d byte protocol limit", limit)})
				return
			case err == io.EOF:
				if len(line) > 0 {
					send(lineResult{line: line})
				}
				return
			case err != nil:
				send(lineResult{err: err})
				return
			}
			if !send(lineResult{line: bytes.Clone(line)}) {
				return
			}
		}
	}()
	return ch
}

// sseLoop frames body as server-sent events and sends each event's decoded data on out until the stream or ctx ends.
// A frame, with its line breaks, may be at most MaxMessage bytes.
func sseLoop(ctx context.Context, body io.Reader, out chan<- valueResult) {
	emit := func(e valueResult) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	split := lineSplitter{r: body}
	var data []byte
	frameBytes := 0
	for {
		// The line's own newline counts toward the frame.
		line, err := split.next(MaxMessage - frameBytes - 1)
		switch {
		case err == errLineLimit:
			emit(valueResult{err: fmt.Errorf("OpenCode event exceeds 16 MB protocol limit")})
			return
		case err == io.EOF:
			emit(valueResult{err: fmt.Errorf("OpenCode event stream disconnected")})
			return
		case err != nil:
			emit(valueResult{err: err})
			return
		}
		frameBytes += len(line) + 1
		if !utf8.Valid(line) {
			emit(valueResult{err: fmt.Errorf("Invalid OpenCode event encoding")})
			return
		}
		text := strings.TrimRight(string(line), "\r")
		if text == "" {
			frameBytes = 0
			if len(data) > 0 {
				value, err := wirejson.Parse(data)
				data = nil
				if err != nil {
					emit(valueResult{err: fmt.Errorf("Invalid OpenCode event JSON: %w", err)})
					return
				}
				if !emit(valueResult{value: value}) {
					return
				}
			}
		} else if rest, found := strings.CutPrefix(text, "data:"); found {
			rest = strings.TrimPrefix(rest, " ")
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, rest...)
		}
	}
}
