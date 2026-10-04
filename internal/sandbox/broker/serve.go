package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// Serve answers the control plane until ctx ends, then stops every sandbox it started.
func (b *Broker) Serve(ctx context.Context, listener net.Listener) error {
	requests, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.InfoPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, b.Info())
	})
	mux.HandleFunc("POST "+wire.SandboxesPath, func(w http.ResponseWriter, r *http.Request) { b.handleSandbox(requests, w, r) })
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return requests }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		cancelRequests()
		close(b.closing)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(shutdown)
		cancel()
		if shutdownErr != nil {
			_ = server.Close()
		}
		// Shutdown does not track hijacked streams: their handlers are still removing their sandboxes. Racing them
		// would only make the sweep's removals collide with theirs. The steps take at most 5s + 10s + 10s, inside
		// compose's 30s stop grace period with time left to exit.
		b.awaitSandboxes(10 * time.Second)
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		return errors.Join(shutdownErr, b.sweep(cleanup))
	case err := <-done:
		return err
	}
}

// awaitSandboxes waits, up to limit, until every sandbox has given its slot back, which each does once its removal
// is confirmed. The sweep after it then meets only what a crash or a failed removal left behind.
func (b *Broker) awaitSandboxes(limit time.Duration) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	taken := 0
	defer func() {
		for range taken {
			<-b.slots
		}
	}()
	for taken < cap(b.slots) {
		select {
		case b.slots <- struct{}{}:
			taken++
		case <-timer.C:
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// handleSandbox serves one sandbox request. base is the broker's serving lifetime, which outlasts the request.
func (b *Broker) handleSandbox(base context.Context, w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, message string) {
		writeJSON(w, status, map[string]string{"error": message})
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), wire.UpgradeProtocol) {
		refuse(http.StatusUpgradeRequired, "Sandbox requests must upgrade to "+wire.UpgradeProtocol)
		return
	}
	var req wire.Request
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "Invalid sandbox request: "+err.Error())
		return
	}
	p, err := b.cfg.plan(req)
	if err != nil {
		refuse(http.StatusBadRequest, err.Error())
		return
	}
	// A full broker makes the request wait for as long as the client does. The control plane keeps its own count of
	// the same slots, but one can stay taken after it counts it free: a removal the broker is still retrying, or a
	// teardown the client stopped waiting for.
	select {
	case b.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	release := sync.OnceFunc(func() { <-b.slots })
	if p.image, err = b.image(r.Context()); err != nil {
		release()
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	prepared, err := b.prepare(r.Context(), base, p, release)
	if err != nil {
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		prepared.discard()
		refuse(http.StatusInternalServerError, "Sandbox stream cannot be hijacked")
		return
	}
	conn, stream, err := hijacker.Hijack()
	if err != nil {
		prepared.discard()
		return
	}
	defer conn.Close()
	if _, err := stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " +
		wire.UpgradeProtocol + "\r\n\r\n"); err != nil || stream.Flush() != nil {
		prepared.discard()
		return
	}
	b.stream(r.Context(), prepared, p, conn, stream.Reader)
}

// streamWriter is a sandbox stream's connection that stays failed after its first failed write: a frame cut short
// leaves the stream unreadable, so nothing may follow it.
type streamWriter struct {
	conn   net.Conn
	failed atomic.Bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.failed.Load() {
		return 0, net.ErrClosed
	}
	n, err := w.conn.Write(p)
	if err != nil {
		w.failed.Store(true)
	}
	return n, err
}

// stream serves one sandbox over an upgraded connection. The connection is the sandbox's lifeline: if it closes,
// the container is killed and removed.
func (b *Broker) stream(ctx context.Context, s *prepared, p plan, conn net.Conn, reader *bufio.Reader) {
	written := &streamWriter{conn: conn}
	out := wire.NewFrameWriter(written)
	controls := make(chan control)
	lifeline, cut := context.WithCancel(ctx)
	defer cut()
	go func() {
		defer cut()
		for {
			kind, payload, err := wire.ReadFrame(reader)
			if err != nil {
				return
			}
			var msg control
			switch kind {
			case wire.FrameStdin:
				if !p.stdin {
					continue
				}
				msg.stdin = payload
			case wire.FrameStdinEOF:
				msg.eof = true
			case wire.FrameSignal:
				msg.signal = string(payload)
				if msg.signal != wire.SignalTerminate && msg.signal != wire.SignalKill {
					continue
				}
			default:
				return
			}
			select {
			case controls <- msg:
			case <-lifeline.Done():
				return
			}
		}
	}()
	forward := func(kind byte) func([]byte) error {
		return func(data []byte) error {
			_, err := out.Data(kind, data)
			return err
		}
	}
	report, err := s.execute(lifeline, p.timeout, output{stdout: forward(wire.FrameStdout), stderr: forward(wire.FrameStderr),
		interrupt: func() { _ = conn.SetWriteDeadline(time.Now()) }}, controls)
	if err != nil {
		report = wire.ExitReport{Error: err.Error(), Sandbox: report.Sandbox}
	}
	if lifeline.Err() != nil && report.Error == streamClosed || written.failed.Load() {
		return
	}
	payload, _ := json.Marshal(report)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = out.Frame(wire.FrameExit, payload)
}
