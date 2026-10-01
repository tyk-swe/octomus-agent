package testutil

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// SocketPath is name in a fresh directory under /tmp, removed when the test ends. A unix socket path must fit the
// kernel's 108-byte sun_path, which a test's own temporary directory may not.
func SocketPath(t testing.TB, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "octomus-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// ListenUnix listens on a fresh SocketPath until the test ends and returns the listener and its path.
func ListenUnix(t testing.TB, name string) (net.Listener, string) {
	t.Helper()
	socket := SocketPath(t, name)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, socket
}

// UnixHTTPServer serves handler on a fresh unix socket until the test ends and returns the socket's path.
func UnixHTTPServer(t testing.TB, handler http.Handler) string {
	t.Helper()
	listener, socket := ListenUnix(t, "server.sock")
	server := &http.Server{Handler: handler}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

// SwitchProtocols takes over w's connection and answers 101 Switching Protocols to protocol, returning the connection
// and its buffered stream. It reports a connection it cannot take over as a test error and returns nil.
func SwitchProtocols(t testing.TB, w http.ResponseWriter, protocol string) (net.Conn, *bufio.ReadWriter) {
	t.Helper()
	conn, stream, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return nil, nil
	}
	_, _ = stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + protocol + "\r\n\r\n")
	_ = stream.Flush()
	return conn, stream
}

// SyncBuffer is a writer a test can read while other goroutines write it.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
