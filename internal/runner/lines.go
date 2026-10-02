package runner

import (
	"bytes"
	"fmt"
	"io"
)

type lineResult struct {
	line []byte
	err  error
}

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
		var readErr error
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
			if readErr != nil {
				if readErr == io.EOF && len(backlog) > 0 {
					if len(backlog) > limit {
						send(over)
						return
					}
					send(lineResult{line: backlog})
				} else if readErr != io.EOF {
					send(lineResult{err: readErr})
				}
				return
			}
			// Process returned bytes before their error, without reading past it.
			var n int
			n, readErr = r.Read(buf)
			backlog = append(backlog, buf[:n]...)
		}
	}()
	return ch
}
