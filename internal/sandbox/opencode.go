package sandbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	whatwg "github.com/nlnwa/whatwg-url/url"
)

const openCodeStartupTimeout = "OpenCode startup timed out"

var errOpenCodeExited = errors.New("OpenCode exited before server readiness")

const (
	readyLineLimit = 16_384
	readyLineCount = 1000
	readyPrefix    = "opencode server listening on "
)

type readyResult struct {
	base string
	err  error
}

// watchReadyLine waits in the background for OpenCode's readiness line on r. The returned reader holds the output
// after it, for the caller to drain once the server is ready.
func watchReadyLine(r io.Reader) (*bufio.Reader, <-chan readyResult) {
	lines := bufio.NewReaderSize(r, readyLineLimit+1)
	ready := make(chan readyResult, 1)
	go func() {
		base, err := awaitReadyLine(lines)
		ready <- readyResult{base, err}
	}()
	return lines, ready
}

func awaitReadyLine(r *bufio.Reader) (string, error) {
	for range readyLineCount {
		line, err := readBoundedLine(r)
		if len(line) > 0 || err == nil {
			if endpoint, ok := strings.CutPrefix(string(line), readyPrefix); ok {
				return ParseLoopbackURL(strings.TrimSpace(endpoint))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", errOpenCodeExited
			}
			return "", err
		}
	}
	return "", errors.New("OpenCode exceeded the startup output limit")
}

func readBoundedLine(r *bufio.Reader) ([]byte, error) {
	chunk, err := r.ReadSlice('\n')
	line := bytes.TrimSuffix(chunk, []byte("\n"))
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > readyLineLimit {
		return nil, fmt.Errorf("line exceeds the %d byte protocol limit", readyLineLimit)
	}
	return bytes.Clone(line), err
}

// ParseLoopbackURL accepts only the plain loopback address a runner server reports once it is listening.
func ParseLoopbackURL(endpoint string) (string, error) {
	u, err := whatwg.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("Invalid OpenCode server address: %w", err)
	}
	port, perr := strconv.Atoi(u.Port())
	authority := endpoint
	if i := strings.Index(authority, "://"); i >= 0 {
		authority = authority[i+3:]
	}
	if i := strings.IndexByte(authority, '/'); i >= 0 {
		authority = authority[:i]
	}
	if u.Scheme() != "http" || u.Hostname() != "127.0.0.1" || perr != nil || port <= 0 ||
		u.Username() != "" || u.Password() != "" || strings.Contains(authority, "@") ||
		u.Pathname() != "/" || strings.ContainsAny(endpoint, "?#") {
		return "", fmt.Errorf("OpenCode did not bind to a local server address")
	}
	return fmt.Sprintf("http://127.0.0.1:%s", u.Port()), nil
}

var loopback = sync.OnceValue(func() *http.Transport {
	return &http.Transport{
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
})

// LoopbackTransport reaches servers on this host's loopback and never consults a proxy.
func LoopbackTransport() http.RoundTripper { return loopback().Clone() }

const (
	handshakeReady  = "OCTOMUS-READY"
	handshakeFailed = "OCTOMUS-FAILED "
	handshakeLimit  = 4096
)

// readHandshake reads the sandbox helper's one-line verdict byte by byte, so nothing of the HTTP/2 stream behind it is
// consumed.
func readHandshake(r io.Reader) error {
	var line []byte
	var one [1]byte
	for len(line) < handshakeLimit {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return errOpenCodeExited
		}
		if one[0] == '\n' {
			text := string(line)
			if text == handshakeReady {
				return nil
			}
			if message, ok := strings.CutPrefix(text, handshakeFailed); ok {
				return errors.New(strings.ToValidUTF8(message, "�"))
			}
			return errors.New("OpenCode sandbox answered an unexpected handshake")
		}
		line = append(line, one[0])
	}
	return errors.New("OpenCode sandbox handshake exceeded its size limit")
}

// pipeConn presents a reader and a writer as the single connection HTTP/2 needs between the control plane and a
// sandbox. Closing it closes neither: their owners end them.
type pipeConn struct {
	r             io.Reader
	w             io.Writer
	local, remote string
}

func (c *pipeConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *pipeConn) Write(p []byte) (int, error)      { return c.w.Write(p) }
func (c *pipeConn) Close() error                     { return nil }
func (c *pipeConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: c.local, Net: "unix"} }
func (c *pipeConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: c.remote, Net: "unix"} }
func (c *pipeConn) SetDeadline(time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error { return nil }
