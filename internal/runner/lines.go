package runner

import (
	"bytes"
	"fmt"
	"io"
)

// lineResult is one newline-delimited line (without the newline) or a terminal
// stream error. The channel closes when the stream ends or fails.
type lineResult struct {
	line []byte
	err  error
}

// lineReader fragments r into newline-terminated lines with the exact per-line
// bound: a line of limit bytes is accepted; once content or backlog is over the
// bound the stream fails once and closes. It never uses bufio.Scanner. Closing
// done abandons the reader so a pending send never strands the goroutine.
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
		over := lineResult{err: fmt.Errorf("line exceeds the %d byte protocol limit", limit)}
		var backlog []byte
		buf := make([]byte, 32768)
		for {
			if end := bytes.IndexByte(backlog, '\n'); end >= 0 {
				if end > limit {
					send(over)
					return
				}
				line := make([]byte, end)
				copy(line, backlog[:end])
				backlog = backlog[end+1:]
				if !send(lineResult{line: line}) {
					return
				}
				continue
			}
			if len(backlog) > limit {
				send(over)
				return
			}
			n, err := r.Read(buf)
			if n > 0 {
				backlog = append(backlog, buf[:n]...)
				continue
			}
			if err != nil {
				if err == io.EOF && len(backlog) > 0 {
					// A trailing unterminated line is still delivered, matching
					// the reference codec's decode_eof.
					if len(backlog) > limit {
						send(over)
						return
					}
					send(lineResult{line: backlog})
				} else if err != io.EOF {
					send(lineResult{err: err})
				}
				return
			}
		}
	}()
	return ch
}
