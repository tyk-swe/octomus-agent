package egress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// Resolver is the part of net.Resolver the gateway uses, replaceable in tests.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer opens the vetted upstream connection.
type Dialer func(ctx context.Context, address netip.AddrPort) (net.Conn, error)

type Gateway struct {
	policy  Policy
	leases  Leases
	resolve Resolver
	dial    Dialer
	log     io.Writer
	logMu   sync.Mutex
	budgets map[string]*logBudget
	statsMu sync.Mutex
	stats   map[string]*usage
	// collected remembers sandboxes the broker has collected, so a decision that lands after collection (a lookup
	// or dial still in flight) is not kept for a summary nobody will collect.
	collected map[string]collection
	open      map[string]int
	tunnels   map[*tunnel]struct{}
	running   sync.WaitGroup
	tunnelIDs atomic.Uint64
	// started is when the gateway began counting; summaries carry it so the broker can tell a sandbox that made no
	// connection from one whose connections a restarted gateway never saw.
	started time.Time
}

const (
	// maxTunnelsPerSandbox bounds how many tunnels one sandbox may hold open, so it cannot exhaust the gateway.
	maxTunnelsPerSandbox = 64
	// halfCloseIdle is how long a tunnel whose one direction has ended may go without a byte in the other.
	halfCloseIdle = 2 * time.Minute
	// maxTunnelLifetime matches the longest time limit the broker can give a sandbox; a revoked lease ends a tunnel
	// much sooner.
	maxTunnelLifetime = 7 * 24 * time.Hour
	// sweepEvery is how often the gateway checks that open tunnels' leases still exist and forgets dead summaries.
	sweepEvery = 2 * time.Second
	// collectedFor outlasts the longest lookup and dial (10 s each) a decision can still be waiting on.
	collectedFor = 2 * time.Minute
	// revokedGrace is how long a summary outlives its lease before the gateway forgets it uncollected.
	revokedGrace = 2 * time.Minute
	// Each logWindow, refusalLogBurst refusals per sandbox (or credential-less source) and tunnelLogBurst tunnels per
	// sandbox are logged; the rest are counted and reported in one line, so no sandbox can flood the log.
	refusalLogBurst = 20
	tunnelLogBurst  = 120
	logWindow       = time.Minute
	logBudgetKeys   = 1024
)

func New(policy Policy, leaseDir string, log io.Writer) *Gateway {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &Gateway{
		policy:  policy,
		leases:  Leases{Dir: leaseDir},
		resolve: net.DefaultResolver,
		dial: func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", address.String())
		},
		log:       log,
		budgets:   map[string]*logBudget{},
		stats:     map[string]*usage{},
		collected: map[string]collection{},
		open:      map[string]int{},
		tunnels:   map[*tunnel]struct{}{},
		started:   time.Now(),
	}
}

// WithNetwork replaces DNS resolution and dialing, for tests.
func (g *Gateway) WithNetwork(resolve Resolver, dial Dialer) *Gateway {
	g.resolve, g.dial = resolve, dial
	return g
}

// Decision is one logged gateway outcome: "allowed" when a tunnel opens and "closed" when it ends (both carry the
// tunnel's id), "denied" for a policy refusal, "failed" when an allowlisted host could not be reached, and
// "suppressed" for the refusals or tunnels a sandbox made beyond its log budget.
type Decision struct {
	Time       string `json:"time"`
	Sandbox    string `json:"sandbox,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Host       string `json:"host,omitempty"`
	Port       uint16 `json:"port,omitempty"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	Tunnel     uint64 `json:"tunnel,omitempty"`
	BytesUp    int64  `json:"bytes_up,omitempty"`
	BytesDown  int64  `json:"bytes_down,omitempty"`
	Millis     int64  `json:"ms,omitempty"`
	Suppressed int    `json:"suppressed,omitempty"`
}

const (
	// summaryHostLimit bounds the hosts a summary names per decision; the rest are counted under "other". It is the
	// task record's own limit, so the broker's record of one sandbox keeps every host its summary named.
	summaryHostLimit = model.SandboxHostLimit
	// foldedNameLimit bounds, per sandbox, the hosts past summaryHostLimit whose first decision the log names whatever
	// its budget, so a sandbox cannot flood the log with names either.
	foldedNameLimit = 1024
)

// collection prevents late outcomes from recreating forgotten summaries. It preserves known incompleteness so a
// repeated collection cannot turn an unresolved request into apparently complete evidence.
type collection struct {
	at         time.Time
	incomplete bool
}

// usage is one sandbox's uncollected summary and the lease it was made under.
type usage struct {
	summary Summary
	lease   string
	revoked time.Time
	// pending counts authenticated requests until their refusal, failure or allowed tunnel is recorded. Open
	// tunnels whose allowed decision was recorded are not pending, even if their closing bytes arrive later.
	pending int
	// named holds the decision and host of each host the summary folded into "other" whose first decision was logged.
	named map[string]struct{}
}

// logBudget counts one sandbox's (or source's) logged lines of one class, refusals or tunnels, in the current window.
type logBudget struct {
	start      time.Time
	logged     int
	suppressed int
	class      string
	sandbox    string
	kind       string
}

func (g *Gateway) logDecision(d Decision) {
	g.logMu.Lock()
	defer g.logMu.Unlock()
	g.writeLocked(d)
}

func (g *Gateway) writeLocked(d Decision) {
	d.Time = time.Now().UTC().Format(time.RFC3339Nano)
	if g.log != nil {
		data, _ := json.Marshal(d)
		_, _ = g.log.Write(append(data, '\n'))
	}
}

// logRefusal logs a refusal unless its sandbox or source has spent this window's refusal budget. unnamed logs it
// regardless, without spending the budget: the sandbox's summary does not name its host.
func (g *Gateway) logRefusal(d Decision, source string, unnamed bool) {
	g.logMu.Lock()
	defer g.logMu.Unlock()
	if unnamed || g.spendLocked("refusals", source, refusalLogBurst, d) {
		g.writeLocked(d)
	}
}

// logOpened logs a tunnel's opening line and reports whether it did. Past its sandbox's tunnel budget the tunnel is
// only counted, and its closing line is left out too, so every logged tunnel has both lines. unnamed logs it
// regardless, without spending the budget: the sandbox's summary does not name its host.
func (g *Gateway) logOpened(d Decision, unnamed bool) bool {
	g.logMu.Lock()
	defer g.logMu.Unlock()
	if !unnamed && !g.spendLocked("tunnels", "sandbox "+d.Sandbox, tunnelLogBurst, d) {
		return false
	}
	g.writeLocked(d)
	return true
}

// spendLocked spends one line of a source's budget for a class of lines and reports whether it may be written. Past
// the burst a line is only counted; the count is logged as one "suppressed" line when the window ends.
func (g *Gateway) spendLocked(class, source string, burst int, d Decision) bool {
	now := time.Now()
	key := class + " " + source
	budget := g.budgets[key]
	if budget == nil {
		sandboxName, kind := d.Sandbox, d.Kind
		if len(g.budgets) >= logBudgetKeys {
			// Past the bound every new source shares one budget per class, reported without a sandbox name.
			key, sandboxName, kind = class, "", ""
			budget = g.budgets[key]
		}
		if budget == nil {
			budget = &logBudget{start: now, class: class, sandbox: sandboxName, kind: kind}
			g.budgets[key] = budget
		}
	}
	if now.Sub(budget.start) >= logWindow {
		g.flushLocked(budget)
		budget.start, budget.logged = now, 0
	}
	if budget.logged >= burst {
		budget.suppressed++
		return false
	}
	budget.logged++
	return true
}

func (g *Gateway) flushLocked(budget *logBudget) {
	if budget.suppressed > 0 {
		g.writeLocked(Decision{Sandbox: budget.sandbox, Kind: budget.kind, Decision: "suppressed",
			Reason: budget.class + " over the log budget", Suppressed: budget.suppressed})
		budget.suppressed = 0
	}
}

// flushBudgets reports and forgets the budgets whose window has ended, or every budget when the gateway stops.
func (g *Gateway) flushBudgets(all bool) {
	g.logMu.Lock()
	defer g.logMu.Unlock()
	now := time.Now()
	for key, budget := range g.budgets {
		if all || now.Sub(budget.start) >= logWindow {
			g.flushLocked(budget)
			delete(g.budgets, key)
		}
	}
}

func summaryKey(host string, port uint16) string {
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// beginDecision registers a request before policy, DNS or dialing can leave its outcome pending. Collection and
// admission share the lock: a request arriving after collection cannot open a tunnel absent from that evidence.
func (g *Gateway) beginDecision(sandboxName, lease string) *usage {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	if _, done := g.collected[sandboxName]; done {
		return nil
	}
	entry := g.stats[sandboxName]
	if entry == nil {
		entry = &usage{summary: emptySummary(), lease: lease}
		g.stats[sandboxName] = entry
	}
	entry.pending++
	return entry
}

// abandonDecision leaves evidence incomplete if the request ended without a decision, for example when its
// client went away while the HTTP connection was being hijacked.
func (g *Gateway) abandonDecision(entry *usage) {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	entry.pending--
	entry.summary.Incomplete = true
}

// countDecision adds a decision to its sandbox's summary and returns the entry and key it counted under. unnamed
// reports the first decision for a host the summary counts only under "other", up to foldedNameLimit such hosts, so
// its log line names the host whatever the log budget. pending, when supplied, settles that request atomically with
// recording its outcome, so collection cannot mistake a recorded decision for an unresolved one.
func (g *Gateway) countDecision(d Decision, lease string, pending *usage) (entry *usage, key string, unnamed bool) {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	if pending != nil {
		pending.pending--
	}
	if d.Sandbox == "" || d.Host == "" {
		return nil, "", false
	}
	if _, done := g.collected[d.Sandbox]; done {
		return nil, "", false
	}
	entry = g.stats[d.Sandbox]
	if entry == nil {
		entry = &usage{summary: emptySummary(), lease: lease}
		g.stats[d.Sandbox] = entry
	}
	target := entry.summary.Denied
	switch d.Decision {
	case "allowed":
		target = entry.summary.Allowed
	case "failed":
		target = entry.summary.Failed
	}
	key = summaryKey(d.Host, d.Port)
	count, known := target[key]
	if !known && len(target) >= summaryHostLimit {
		folded := d.Decision + " " + key
		if _, logged := entry.named[folded]; !logged && len(entry.named) < foldedNameLimit {
			if entry.named == nil {
				entry.named = map[string]struct{}{}
			}
			entry.named[folded], unnamed = struct{}{}, true
		}
		key = "other"
		count = target[key]
	}
	count.Count++
	target[key] = count
	return entry, key, unnamed
}

// lease identifies the sandbox behind a proxy credential and names its lease file. The credential is only ever
// compared through the digest that names that file.
func (g *Gateway) lease(header string) (Lease, string, bool) {
	encoded, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return Lease{}, "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return Lease{}, "", false
	}
	user, token, ok := strings.Cut(string(decoded), ":")
	if !ok || user != ProxyUser {
		return Lease{}, "", false
	}
	return g.leases.lookup(token)
}

// refusedHost names a refused target for the summary without carrying arbitrary text into it: the normalized host
// name, a single valid label, the canonical form of an IP literal, or a fixed label for anything else.
func refusedHost(text string) string {
	if host, err := NormalizeHost(text); err == nil {
		return host
	}
	if addr, err := netip.ParseAddr(strings.Trim(text, "[]")); err == nil {
		return addr.WithZone("").Unmap().String()
	}
	if label := strings.TrimSuffix(strings.ToLower(text), "."); labelPattern.MatchString(label) {
		return label
	}
	return "(invalid host)"
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only a tunnel keeps its connection; every refusal closes it, so a refused client cannot hold it idle.
	w.Header().Set("Connection", "close")
	lease, leaseFile, ok := g.lease(r.Header.Get("Proxy-Authorization"))
	if !ok {
		source, _, _ := net.SplitHostPort(r.RemoteAddr)
		g.logRefusal(Decision{Decision: "denied", Reason: "no sandbox credential"}, "source "+source, false)
		w.Header().Set("Proxy-Authenticate", `Basic realm="octomus-egress"`)
		http.Error(w, "Octomus egress: only sandboxes with a live lease may connect", http.StatusProxyAuthRequired)
		return
	}
	pending := g.beginDecision(lease.Sandbox, leaseFile)
	if pending == nil {
		g.logRefusal(Decision{Sandbox: lease.Sandbox, Kind: lease.Kind, Decision: "denied", Reason: "sandbox evidence already collected"},
			"sandbox "+lease.Sandbox, false)
		http.Error(w, "Octomus egress: this sandbox's evidence has already been collected", http.StatusForbidden)
		return
	}
	settled := false
	defer func() {
		if !settled {
			g.abandonDecision(pending)
		}
	}()
	count := func(d Decision) (*usage, string, bool) {
		settled = true
		return g.countDecision(d, leaseFile, pending)
	}
	refuse := func(decision string, status int, host string, port uint16, reason string) {
		d := Decision{Sandbox: lease.Sandbox, Kind: lease.Kind, Host: host, Port: port, Decision: decision, Reason: reason}
		_, _, unnamed := count(d)
		g.logRefusal(d, "sandbox "+lease.Sandbox, unnamed)
		http.Error(w, "Octomus egress blocked this connection: "+reason, status)
	}
	deny := func(status int, host string, port uint16, reason string) {
		refuse("denied", status, host, port, reason)
	}
	// fail reports an allowlisted host the gateway could not reach, which is not a refusal.
	fail := func(status int, host string, port uint16, reason string) {
		refuse("failed", status, host, port, reason)
	}
	if r.Method != http.MethodConnect {
		host, port := "(invalid host)", uint16(80)
		if r.URL != nil {
			host = refusedHost(r.URL.Hostname())
			if r.URL.Scheme == "https" {
				port = 443
			}
			if explicit, err := strconv.ParseUint(r.URL.Port(), 10, 16); err == nil {
				port = uint16(explicit)
			}
		}
		deny(http.StatusForbidden, host, port, "only HTTPS tunnels are allowed")
		return
	}
	hostText, portText, err := net.SplitHostPort(r.Host)
	if err != nil {
		deny(http.StatusBadRequest, "(malformed target)", 0, "malformed tunnel target")
		return
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		deny(http.StatusBadRequest, refusedHost(hostText), 0, "malformed tunnel target")
		return
	}
	host, err := NormalizeHost(hostText)
	if err != nil {
		deny(http.StatusForbidden, refusedHost(hostText), uint16(port), "target is not an allowlisted host name ("+err.Error()+")")
		return
	}
	// The allowlist is decided before any lookup, so a refused name never reaches DNS and cannot carry data out.
	if !g.policy.Allows(lease.Kind, host, uint16(port)) {
		deny(http.StatusForbidden, host, uint16(port), "host is not on the "+lease.Kind+" allowlist")
		return
	}
	if !g.reserve(lease.Sandbox) {
		fail(http.StatusTooManyRequests, host, uint16(port), "too many open tunnels")
		return
	}
	defer g.releaseTunnel(lease.Sandbox)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	// The rooted name is looked up as is: no resolver search domain is ever appended to an allowlisted name.
	addresses, err := g.resolve.LookupNetIP(ctx, "ip", host+".")
	cancel()
	if err != nil {
		fail(http.StatusBadGateway, host, uint16(port), "name did not resolve")
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
		fail(http.StatusBadGateway, host, uint16(port), "upstream connection failed")
		return
	}
	defer upstream.Close()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		fail(http.StatusInternalServerError, host, uint16(port), "tunnel unsupported")
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	t, ok := g.track(leaseFile, client, upstream)
	if !ok {
		return
	}
	defer g.untrack(t)
	decision := Decision{Sandbox: lease.Sandbox, Kind: lease.Kind, Host: host, Port: uint16(port), Decision: "allowed",
		Tunnel: g.tunnelIDs.Add(1)}
	_, _, unnamed := count(decision)
	logged := g.logOpened(decision, unnamed)
	started := time.Now()
	var up, down int64
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		t.cut("client went away")
	} else {
		up, down = g.splice(t, buffered)
	}
	if logged {
		closed := decision
		closed.Decision, closed.Reason = "closed", t.reason()
		closed.BytesUp, closed.BytesDown, closed.Millis = up, down, time.Now().Sub(started).Milliseconds()
		g.logDecision(closed)
	}
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
	if g.open[sandboxName] >= maxTunnelsPerSandbox {
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

// tunnel is one open CONNECT tunnel, tied to the lease that admitted it so revoking the lease ends it.
type tunnel struct {
	lease            string
	client, upstream net.Conn
	cause            atomic.Pointer[string]
}

// cut ends the tunnel by closing both ends, keeping the first reason given for its closing line.
func (t *tunnel) cut(reason string) {
	if t.cause.CompareAndSwap(nil, &reason) {
		_ = t.client.Close()
		_ = t.upstream.Close()
	}
}

func (t *tunnel) reason() string {
	if cause := t.cause.Load(); cause != nil {
		return *cause
	}
	return ""
}

// track registers an open tunnel; it refuses once the gateway is stopping.
func (g *Gateway) track(lease string, client, upstream net.Conn) (*tunnel, bool) {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	if g.tunnels == nil {
		return nil, false
	}
	t := &tunnel{lease: lease, client: client, upstream: upstream}
	g.tunnels[t] = struct{}{}
	g.running.Add(1)
	return t, true
}

func (g *Gateway) untrack(t *tunnel) {
	g.statsMu.Lock()
	defer g.statsMu.Unlock()
	if g.tunnels != nil {
		delete(g.tunnels, t)
	}
	g.running.Done()
}

// activity records when a tunnel last carried a byte, as time since the tunnel started.
type activity struct {
	r     io.Reader
	start time.Time
	last  *atomic.Int64
}

func (a activity) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.last.Store(int64(time.Since(a.start)))
	}
	return n, err
}

func closeWrite(conn net.Conn) {
	if half, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = half.CloseWrite()
	}
}

// splice copies both directions and reports the bytes each way. A half-close is forwarded and the other direction
// keeps running until its own end, so a client may finish sending and still wait for the reply; it is cut only if
// it then carries nothing for the half-close idle time. The tunnel's lifetime bounds it, and cutting it (a revoked
// lease or a stopping gateway) ends both directions at once.
func (g *Gateway) splice(t *tunnel, buffered io.Reader) (int64, int64) {
	start := time.Now()
	var last atomic.Int64
	upCh, downCh := make(chan int64, 1), make(chan int64, 1)
	go func() {
		n, _ := io.Copy(t.upstream, activity{r: buffered, start: start, last: &last})
		closeWrite(t.upstream)
		upCh <- n
	}()
	go func() {
		n, _ := io.Copy(t.client, activity{r: t.upstream, start: start, last: &last})
		closeWrite(t.client)
		downCh <- n
	}()
	lifetime := time.NewTimer(maxTunnelLifetime)
	defer lifetime.Stop()
	var idle *time.Ticker
	var idleTick <-chan time.Time
	var up, down int64
	for pending := 2; pending > 0; {
		select {
		case up = <-upCh:
			pending--
		case down = <-downCh:
			pending--
		case <-lifetime.C:
			t.cut("tunnel lifetime reached")
		case <-idleTick:
			if time.Since(start)-time.Duration(last.Load()) >= halfCloseIdle {
				t.cut("idle after one side closed")
			}
		}
		if pending == 1 && idle == nil {
			last.Store(int64(time.Since(start)))
			idle = time.NewTicker(max(halfCloseIdle/4, time.Millisecond))
			idleTick = idle.C
		}
	}
	if idle != nil {
		idle.Stop()
	}
	_ = t.client.Close()
	_ = t.upstream.Close()
	return up, down
}
