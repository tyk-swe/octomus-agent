package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// maxConnections bounds the gateway's open client connections, tunnels included, so the sandboxes together
	// cannot exhaust its descriptors or memory; maxConnectionsPerSource leaves one sandbox its tunnels and some
	// headroom without letting it take the rest.
	maxConnections          = 4096
	maxConnectionsPerSource = 256
	// stopWait is how long a stopping gateway waits for cut tunnels to log their closing lines.
	stopWait = 5 * time.Second
)

// Serve answers sandboxes on listener until ctx ends. Refused connections are closed, idle and oversized requests
// are bounded, and every sweep ends the tunnels of revoked leases. On the way out it cuts every open tunnel and waits
// briefly for their closing lines.
func (g *Gateway) Serve(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout bounds reading a refused request's body. A CONNECT has none, so its lookup and dial are not
		// under it, and a hijacked tunnel leaves the server's deadlines behind; splice bounds the tunnel instead.
		ReadTimeout:    30 * time.Second,
		IdleTimeout:    30 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	go func() {
		ticker := time.NewTicker(sweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = server.Close()
				return
			case <-ticker.C:
				g.sweep()
			}
		}
	}()
	err := server.Serve(limitConnections(listener, maxConnections, maxConnectionsPerSource))
	cancel()
	g.stop()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// sweep ends tunnels whose lease has been revoked, forgets summaries whose lease has been gone past the grace period
// and collection marks past their use, and reports refusal counts whose log window has ended.
func (g *Gateway) sweep() {
	g.statsMu.Lock()
	leases := map[string]bool{}
	for t := range g.tunnels {
		leases[t.lease] = true
	}
	for _, entry := range g.stats {
		leases[entry.lease] = true
	}
	g.statsMu.Unlock()
	gone := map[string]bool{}
	for lease := range leases {
		if g.leases.revoked(lease) {
			gone[lease] = true
		}
	}
	now := time.Now()
	var revoked []*tunnel
	g.statsMu.Lock()
	for t := range g.tunnels {
		if gone[t.lease] {
			revoked = append(revoked, t)
		}
	}
	for name, entry := range g.stats {
		switch {
		case !gone[entry.lease]:
		case entry.revoked.IsZero():
			entry.revoked = now
		case now.Sub(entry.revoked) >= revokedGrace && g.open[name] == 0 && entry.pending == 0:
			delete(g.stats, name)
		}
	}
	for name, collected := range g.collected {
		if now.Sub(collected.at) >= collectedFor {
			delete(g.collected, name)
		}
	}
	g.statsMu.Unlock()
	for _, t := range revoked {
		t.cut("lease revoked")
	}
	g.flushBudgets(false)
}

// stop cuts every open tunnel, refuses new ones and waits briefly for their closing lines.
func (g *Gateway) stop() {
	g.statsMu.Lock()
	open := g.tunnels
	g.tunnels = nil
	g.statsMu.Unlock()
	for t := range open {
		t.cut("gateway stopped")
	}
	done := make(chan struct{})
	go func() {
		g.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(stopWait):
	}
	g.flushBudgets(true)
}

// limitConnections bounds open connections in total and per source address. A connection over either bound is
// closed at once, so one sandbox cannot take the gateway from the others.
func limitConnections(listener net.Listener, total, perSource int) net.Listener {
	return &limitedListener{Listener: listener, total: total, perSource: perSource, bySource: map[string]int{}}
}

type limitedListener struct {
	net.Listener
	mu               sync.Mutex
	open             int
	bySource         map[string]int
	total, perSource int
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		source := conn.RemoteAddr().String()
		if host, _, err := net.SplitHostPort(source); err == nil {
			source = host
		}
		if l.admit(source) {
			return &limitedConn{Conn: conn, release: sync.OnceFunc(func() { l.release(source) })}, nil
		}
		_ = conn.Close()
	}
}

func (l *limitedListener) admit(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open >= l.total || l.bySource[source] >= l.perSource {
		return false
	}
	l.open++
	l.bySource[source]++
	return true
}

func (l *limitedListener) release(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open--
	if l.bySource[source]--; l.bySource[source] <= 0 {
		delete(l.bySource, source)
	}
}

// limitedConn gives its slot back when closed, and still forwards a tunnel's half-close.
type limitedConn struct {
	net.Conn
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

func (c *limitedConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return nil
}
