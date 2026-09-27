package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

func sseLoop(ctx context.Context, body io.Reader, out chan<- valueResult) {
	emit := func(e valueResult) bool {
		select {
		case out <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var buffer []byte
	var data []byte
	frameBytes := 0
	chunk := make([]byte, 32768)
	for {
		for {
			end := bytes.IndexByte(buffer, '\n')
			if end < 0 {
				break
			}
			line := buffer[:end+1]
			buffer = buffer[end+1:]
			frameBytes += len(line)
			if frameBytes > MaxMessage {
				emit(valueResult{err: fmt.Errorf("OpenCode event exceeds 16 MB protocol limit")})
				return
			}
			if !utf8.Valid(line) {
				emit(valueResult{err: fmt.Errorf("Invalid OpenCode event encoding")})
				return
			}
			text := strings.TrimRight(string(line), "\r\n")
			if text == "" {
				frameBytes = 0
				if len(data) > 0 {
					value, err := decodeJSON(data)
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
		if frameBytes+len(buffer) > MaxMessage {
			emit(valueResult{err: fmt.Errorf("OpenCode event backlog exceeds 16 MB")})
			return
		}
		n, err := body.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			continue
		}
		if err != nil {
			if err == io.EOF {
				emit(valueResult{err: fmt.Errorf("OpenCode event stream disconnected")})
			} else {
				emit(valueResult{err: err})
			}
			return
		}
	}
}
