package egress

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// Resolver is the part of net.Resolver the gateway uses, replaceable in tests.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer opens the vetted upstream connection.
type Dialer func(ctx context.Context, address netip.AddrPort) (net.Conn, error)

type Gateway struct {
	policy   Policy
	leaseDir string
	resolve  Resolver
	dial     Dialer
	log      io.Writer
	logMu    sync.Mutex
	statsMu  sync.Mutex
	stats    map[string]*Summary
	open     map[string]int
	perBox   int
	now      func() time.Time
}

// maxTunnelsPerSandbox bounds how many tunnels one sandbox may hold open, so it cannot exhaust the gateway.
const maxTunnelsPerSandbox = 64

func New(policy Policy, leaseDir string, log io.Writer) *Gateway {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &Gateway{
		policy:   policy,
		leaseDir: leaseDir,
		resolve:  net.DefaultResolver,
		dial: func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address.String())
		},
		log:    log,
		stats:  map[string]*Summary{},
		open:   map[string]int{},
		perBox: maxTunnelsPerSandbox,
		now:    time.Now,
	}
}

// WithNetwork replaces DNS resolution and dialing, for tests.
func (g *Gateway) WithNetwork(resolve Resolver, dial Dialer) *Gateway {
	g.resolve, g.dial = resolve, dial
	return g
}

// Decision is one logged gateway outcome.
type Decision struct {
	Time      string `json:"time"`
	Sandbox   string `json:"sandbox,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Host      string `json:"host,omitempty"`
	Port      uint16 `json:"port,omitempty"`
	Decision  string `json:"decision"`
	Reason    string `json:"reason,omitempty"`
	BytesUp   int64  `json:"bytes_up,omitempty"`
	BytesDown int64  `json:"bytes_down,omitempty"`
	Millis    int64  `json:"ms,omitempty"`
}

// Summary is what one sandbox did through the gateway, kept until the broker collects it.
type Summary struct {
	Allowed map[string]HostCount `json:"allowed"`
	Denied  map[string]HostCount `json:"denied"`
}

type HostCount struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes,omitempty"`
}

const summaryHostLimit = 64

func (g *Gateway) record(d Decision) {
	g.logDecision(d)
	g.countDecision(d)
}

func (g *Gateway) logDecision(d Decision) {
	d.Time = g.now().UTC().Format(time.RFC3339Nano)
	if g.log != nil {
		data, _ := json.Marshal(d)
		g.logMu.Lock()
		_, _ = g.log.Write(append(data, '\n'))
		g.logMu.Unlock()
	}
}

func (g *Gateway) countDecision(d Decision) (*Summary, string) {
	if d.Sandbox == "" || d.Host == "" {
		return nil, ""
	}
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	summary := g.stats[d.Sandbox]
	if summary == nil {
		summary = &Summary{Allowed: map[string]HostCount{}, Denied: map[string]HostCount{}}
		g.stats[d.Sandbox] = summary
	}
	target := summary.Denied
	if d.Decision == "allowed" {
		target = summary.Allowed
	}
	key := d.Host + ":" + strconv.Itoa(int(d.Port))
	count, known := target[key]
	if !known && len(target) >= summaryHostLimit {
		key = "other"
		count = target[key]
	}
	count.Count++
	count.Bytes += d.BytesUp + d.BytesDown
	target[key] = count
	return summary, key
}

// addTunnelBytes updates an uncollected summary without recreating evidence the broker already collected.
func (g *Gateway) addTunnelBytes(sandboxName string, summary *Summary, key string, bytes int64) {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	if g.stats[sandboxName] == summary {
		count := summary.Allowed[key]
		count.Bytes += bytes
		summary.Allowed[key] = count
	}
}

// Collect returns and forgets what a finished sandbox did.
func (g *Gateway) Collect(sandboxName string) Summary {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	summary := g.stats[sandboxName]
	delete(g.stats, sandboxName)
	if summary == nil {
		return Summary{Allowed: map[string]HostCount{}, Denied: map[string]HostCount{}}
	}
	return *summary
}

// lease identifies the sandbox behind a proxy credential. The credential is only ever compared through the digest
// that names its lease file.
func (g *Gateway) lease(header string) (sandbox.Lease, bool) {
	encoded, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return sandbox.Lease{}, false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return sandbox.Lease{}, false
	}
	user, token, ok := strings.Cut(string(decoded), ":")
	if !ok || user != sandbox.ProxyUser || len(token) != 64 {
		return sandbox.Lease{}, false
	}
	if _, err := hex.DecodeString(token); err != nil {
		return sandbox.Lease{}, false
	}
	data, err := os.ReadFile(filepath.Join(g.leaseDir, sandbox.LeaseFile(token)))
	if err != nil || len(data) > 4096 {
		return sandbox.Lease{}, false
	}
	var lease sandbox.Lease
	if json.Unmarshal(data, &lease) != nil || lease.Sandbox == "" {
		return sandbox.Lease{}, false
	}
	return lease, true
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lease, ok := g.lease(r.Header.Get("Proxy-Authorization"))
	if !ok {
		g.record(Decision{Decision: "denied", Reason: "no sandbox credential"})
		w.Header().Set("Proxy-Authenticate", `Basic realm="octomus-egress"`)
		http.Error(w, "Octomus egress: only sandboxes with a live lease may connect", http.StatusProxyAuthRequired)
		return
	}
	deny := func(status int, host string, port uint16, reason string) {
		g.record(Decision{Sandbox: lease.Sandbox, Kind: lease.Kind, Host: host, Port: port, Decision: "denied", Reason: reason})
		http.Error(w, "Octomus egress blocked this connection: "+reason, status)
	}
	if r.Method != http.MethodConnect {
		host := ""
		if r.URL != nil {
			host, _ = NormalizeHost(r.URL.Hostname())
		}
		deny(http.StatusForbidden, host, 80, "only HTTPS tunnels are allowed")
		return
	}
	hostText, portText, err := net.SplitHostPort(r.Host)
	port, portErr := strconv.ParseUint(portText, 10, 16)
	if err != nil || portErr != nil || port == 0 {
		deny(http.StatusBadRequest, "", 0, "malformed tunnel target")
		return
	}
	host, err := NormalizeHost(hostText)
	if err != nil {
		deny(http.StatusForbidden, "", uint16(port), "target is not an allowlisted host name ("+err.Error()+")")
		return
	}
	// The allowlist is decided before any lookup, so a refused name never reaches DNS and cannot carry data out.
	if !g.policy.Allows(lease.Kind, host, uint16(port)) {
		deny(http.StatusForbidden, host, uint16(port), "host is not on the "+lease.Kind+" allowlist")
		return
	}
	if !g.reserve(lease.Sandbox) {
		deny(http.StatusTooManyRequests, host, uint16(port), "too many open tunnels")
		return
	}
	defer g.releaseTunnel(lease.Sandbox)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	// The rooted name is looked up as is: no resolver search domain is ever appended to an allowlisted name.
	addresses, err := g.resolve.LookupNetIP(ctx, "ip", host+".")
	cancel()
	if err != nil {
		deny(http.StatusBadGateway, host, uint16(port), "name did not resolve")
		return
	}
	var targets []netip.Addr
	for _, address := range addresses {
		if PublicAddress(address) {
			targets = append(targets, address.Unmap())
		}
	}
	if len(targets) == 0 {
		deny(http.StatusForbidden, host, uint16(port), "name resolves only to non-public addresses")
		return
	}
	upstream, err := g.dialAddresses(r.Context(), targets, uint16(port))
	if err != nil {
		deny(http.StatusBadGateway, host, uint16(port), "upstream connection failed")
		return
	}
	defer upstream.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		deny(http.StatusInternalServerError, host, uint16(port), "tunnel unsupported")
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	decision := Decision{Sandbox: lease.Sandbox, Kind: lease.Kind, Host: host, Port: uint16(port), Decision: "allowed"}
	summary, key := g.countDecision(decision)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		g.logDecision(decision)
		return
	}
	started := g.now()
	up, down := splice(client, buffered, upstream)
	g.addTunnelBytes(lease.Sandbox, summary, key, up+down)
	decision.BytesUp, decision.BytesDown, decision.Millis = up, down, g.now().Sub(started).Milliseconds()
	g.logDecision(decision)
}

// dialAddresses shares a bounded connection budget among the remaining vetted addresses, so a stalled attempt
// leaves time to try the others. Dialing literal addresses keeps every retry within the original DNS decision.
func (g *Gateway) dialAddresses(ctx context.Context, addresses []netip.Addr, port uint16) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var err error
	for i, address := range addresses {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attempt, cancelAttempt := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(addresses)-i))
		var conn net.Conn
		conn, err = g.dial(attempt, netip.AddrPortFrom(address, port))
		cancelAttempt()
		if err == nil {
			return conn, nil
		}
	}
	return nil, err
}

func (g *Gateway) reserve(sandboxName string) bool {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	if g.open[sandboxName] >= g.perBox {
		return false
	}
	g.open[sandboxName]++
	return true
}

func (g *Gateway) releaseTunnel(sandboxName string) {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	if g.open[sandboxName]--; g.open[sandboxName] <= 0 {
		delete(g.open, sandboxName)
	}
}

// splice copies both directions until either side closes and reports the bytes each way.
func splice(client net.Conn, buffered io.Reader, upstream net.Conn) (int64, int64) {
	upCh, downCh := make(chan int64, 1), make(chan int64, 1)
	go func() {
		n, _ := io.Copy(upstream, buffered)
		if half, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
		upCh <- n
	}()
	go func() {
		n, _ := io.Copy(client, upstream)
		if half, ok := client.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
		downCh <- n
	}()
	var up, down int64
	select {
	case up = <-upCh:
		select {
		case down = <-downCh:
		case <-time.After(5 * time.Second):
			_ = client.Close()
			_ = upstream.Close()
			down = <-downCh
		}
	case down = <-downCh:
		select {
		case up = <-upCh:
		case <-time.After(5 * time.Second):
			_ = client.Close()
			_ = upstream.Close()
			up = <-upCh
		}
	}
	_ = client.Close()
	_ = upstream.Close()
	return up, down
}

// Describe lists the effective allowlists for logs and the dashboard.
func (p Policy) Describe() map[string][]string {
	describe := func(rules []Rule) []string {
		out := []string{}
		for _, rule := range rules {
			out = append(out, rule.String())
		}
		sort.Strings(out)
		return out
	}
	return map[string][]string{"model": describe(p.Model), "build": describe(p.Build)}
}

// ServeCollector answers the broker's request for a finished sandbox's summary on a local socket.
func (g *Gateway) ServeCollector(ctx context.Context, listener net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/summary", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("sandbox")
		if name == "" || len(name) > 128 {
			http.Error(w, "sandbox required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(g.Collect(name))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	err := server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (d Decision) String() string {
	return fmt.Sprintf("%s %s:%d %s", d.Decision, d.Host, d.Port, d.Reason)
}
