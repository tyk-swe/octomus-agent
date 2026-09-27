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
