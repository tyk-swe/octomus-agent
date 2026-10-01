package egress

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestPublicAddressRefusesEveryInternalRange(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1", "10.1.2.3", "172.17.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"0.1.2.3", "198.18.0.1", "192.0.2.1", "203.0.113.9", "240.0.0.1", "255.255.255.255", "224.0.0.1",
		"::1", "::", "fe80::1", "fd00:ec2::254", "fc00::1", "ff02::1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::a00:1", "2002:a00:1::1", "2001:0:4136:e378::1", "2001:db8::1",
	} {
		if PublicAddress(netip.MustParseAddr(addr)) {
			t.Errorf("%s was treated as public", addr)
		}
	}
	for _, addr := range []string{"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if !PublicAddress(netip.MustParseAddr(addr)) {
			t.Errorf("%s was refused", addr)
		}
	}
}

func TestParseRulesAndPolicyByKind(t *testing.T) {
	model, err := ParseRules("api.openai.com, chatgpt.com")
	if err != nil {
		t.Fatal(err)
	}
	build, err := ParseRules("*.npmjs.org registry.example.com:8443\nproxy.golang.org")
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{Model: model, Build: build}
	for _, tc := range []struct {
		kind, host string
		port       uint16
		want       bool
	}{
		{"runner", "api.openai.com", 443, true},
		{"verify", "api.openai.com", 443, false},
		{"runner", "registry.npmjs.org", 443, true},
		{"verify", "registry.npmjs.org", 443, true},
		{"verify", "npmjs.org", 443, false},
		{"verify", "evil-npmjs.org", 443, false},
		{"verify", "registry.example.com", 443, false},
		{"verify", "registry.example.com", 8443, true},
		{"probe", "proxy.golang.org", 443, false},
		{"runner", "api.openai.com", 80, false},
	} {
		if got := policy.Allows(tc.kind, tc.host, tc.port); got != tc.want {
			t.Errorf("Allows(%s, %s:%d) = %v", tc.kind, tc.host, tc.port, got)
		}
	}
	for _, bad := range []string{"10.0.0.1", "[::1]", "localhost", "*.", "exa_mple.com", "host.com:0", "host.com:99999", "-bad.com"} {
		if _, err := ParseRules(bad); err == nil {
			t.Errorf("rule %q accepted", bad)
		}
	}
	if got := policy.Describe()["build"]; strings.Join(got, ",") != "*.npmjs.org,proxy.golang.org,registry.example.com:8443" {
		t.Fatalf("describe = %v", got)
	}
}

type recordingResolver struct {
	mu      sync.Mutex
	lookups []string
	answers map[string][]netip.Addr
}

func (r *recordingResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lookups = append(r.lookups, host)
	if answers, ok := r.answers[strings.TrimSuffix(host, ".")]; ok {
		return answers, nil
	}
	return nil, errors.New("no such host")
}

type gatewayFixture struct {
	gateway  *Gateway
	proxy    string
	token    string
	resolver *recordingResolver
	dialed   chan netip.AddrPort
	log      *testutil.SyncBuffer
	leases   string
}

func newGatewayFixture(t *testing.T, kind string, configure ...func(*Gateway)) *gatewayFixture {
	t.Helper()
	leases := t.TempDir()
	token := strings.Repeat("ab", 32)
	data, _ := json.Marshal(Lease{Sandbox: "octomus-test-" + kind, Kind: kind})
	if err := os.WriteFile(filepath.Join(leases, leaseFile(token)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	model, _ := ParseRules("api.openai.com")
	build, _ := ParseRules("registry.npmjs.org, internal.example.com")
	f := &gatewayFixture{
		token: token, dialed: make(chan netip.AddrPort, 8), log: &testutil.SyncBuffer{},
		resolver: &recordingResolver{answers: map[string][]netip.Addr{
			"api.openai.com":       {netip.MustParseAddr("10.9.9.9"), netip.MustParseAddr("93.184.216.34")},
			"registry.npmjs.org":   {netip.MustParseAddr("104.16.0.35")},
			"internal.example.com": {netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("10.0.0.5")},
		}},
	}
	f.gateway = New(Policy{Model: model, Build: build}, leases, f.log).WithNetwork(f.resolver,
		func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
			f.dialed <- address
			return (&net.Dialer{}).DialContext(ctx, "tcp", echo.Addr().String())
		})
	for _, option := range configure {
		option(f.gateway)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.gateway.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	f.proxy = listener.Addr().String()
	f.leases = leases
	return f
}

// connect opens a CONNECT tunnel as a client honouring HTTPS_PROXY with credentials would.
func (f *gatewayFixture) connect(t *testing.T, target, credential string) (int, net.Conn) {
	t.Helper()
	status, tunnel, err := f.tryConnect(target, credential)
	if err != nil {
		t.Fatal(err)
	}
	return status, tunnel
}

func (f *gatewayFixture) tryConnect(target, credential string) (int, net.Conn, error) {
	conn, err := net.Dial("tcp", f.proxy)
	if err != nil {
		return 0, nil, err
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if credential != "" {
		request += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(credential)) + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		conn.Close()
		return 0, nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return resp.StatusCode, nil, nil
	}
	return resp.StatusCode, &bufferedConn{Conn: conn, r: reader}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func TestGatewayTunnelsOnlyAllowlistedHostsToPublicAddresses(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	credential := ProxyUser + ":" + f.token
	status, tunnel := f.connect(t, "api.openai.com:443", credential)
	if status != http.StatusOK {
		t.Fatalf("allowlisted tunnel = %d", status)
	}
	if address := <-f.dialed; address != netip.MustParseAddrPort("93.184.216.34:443") {
		t.Fatalf("dialed %s; want the vetted public address, skipping the private answer", address)
	}
	if _, err := io.WriteString(tunnel, "ping"); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, 4)
	if _, err := io.ReadFull(tunnel, echoed); err != nil || string(echoed) != "ping" {
		t.Fatalf("tunnel echo = %q, %v", echoed, err)
	}
	tunnel.Close()

	for target, want := range map[string]int{
		"attacker.example.net:443": http.StatusForbidden,
		"api.openai.com:80":        http.StatusForbidden,
		"93.184.216.34:443":        http.StatusForbidden,
		"[::1]:443":                http.StatusForbidden,
		"internal.example.com:443": http.StatusForbidden,
	} {
		if status, _ := f.connect(t, target, credential); status != want {
			t.Errorf("CONNECT %s = %d; want %d", target, status, want)
		}
	}
	f.resolver.mu.Lock()
	lookups := strings.Join(f.resolver.lookups, ",")
	f.resolver.mu.Unlock()
	if lookups != "api.openai.com.,internal.example.com." {
		t.Fatalf("lookups = %s; a refused name must never reach DNS", lookups)
	}
	summary := f.gateway.Collect("octomus-test-runner")
	if summary.Allowed["api.openai.com:443"].Count != 1 || summary.Denied["attacker.example.net:443"].Count != 1 ||
		summary.Denied["internal.example.com:443"].Count != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if again := f.gateway.Collect("octomus-test-runner"); len(again.Allowed)+len(again.Denied) != 0 {
		t.Fatal("a collected summary must be forgotten")
	}
	if log := f.log.String(); !strings.Contains(log, `"decision":"denied"`) || !strings.Contains(log, `"host":"attacker.example.net"`) {
		t.Fatalf("decision log = %s", log)
	}
}

func TestGatewayRequiresALiveLeaseAndAppliesItsKind(t *testing.T) {
	f := newGatewayFixture(t, "verify")
	for name, credential := range map[string]string{
		"missing":     "",
		"wrong user":  "someone:" + f.token,
		"short token": ProxyUser + ":abc",
		"unknown":     ProxyUser + ":" + strings.Repeat("cd", 32),
		"not hex":     ProxyUser + ":" + strings.Repeat("zz", 32),
	} {
		if status, _ := f.connect(t, "registry.npmjs.org:443", credential); status != http.StatusProxyAuthRequired {
			t.Errorf("%s credential = %d; want 407", name, status)
		}
	}
	credential := ProxyUser + ":" + f.token
	if status, _ := f.connect(t, "api.openai.com:443", credential); status != http.StatusForbidden {
		t.Errorf("verification sandbox reached a model host: %d", status)
	}
	if status, tunnel := f.connect(t, "registry.npmjs.org:443", credential); status != http.StatusOK {
		t.Errorf("verification sandbox could not reach a build host: %d", status)
	} else {
		tunnel.Close()
	}
	conn, err := net.Dial("tcp", f.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "GET http://registry.npmjs.org/ HTTP/1.1\r\nHost: registry.npmjs.org\r\nProxy-Authorization: Basic "+
		base64.StdEncoding.EncodeToString([]byte(credential))+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("plain HTTP proxying = %v, %v; want it refused", resp, err)
	}
}

func TestGatewayBoundsTunnelsPerSandbox(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	f.gateway.perBox = 2
	credential := ProxyUser + ":" + f.token
	var open []net.Conn
	for range 2 {
		status, tunnel := f.connect(t, "api.openai.com:443", credential)
		if status != http.StatusOK {
			t.Fatalf("tunnel = %d", status)
		}
		open = append(open, tunnel)
	}
	if status, _ := f.connect(t, "api.openai.com:443", credential); status != http.StatusTooManyRequests {
		t.Fatalf("third tunnel = %d; want 429", status)
	}
	for _, tunnel := range open {
		tunnel.Close()
	}
}

func TestGatewayCollectsOpenTunnelWithoutRecreatingSummary(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	status, tunnel := f.connect(t, "api.openai.com:443", ProxyUser+":"+f.token)
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	defer tunnel.Close()
	_ = tunnel.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(tunnel, "ping"); err != nil {
		t.Fatal(err)
	}
	var echoed [4]byte
	if _, err := io.ReadFull(tunnel, echoed[:]); err != nil {
		t.Fatal(err)
	}
	summary := f.gateway.Collect("octomus-test-runner")
	if summary.Allowed["api.openai.com:443"].Count != 1 {
		t.Fatalf("open tunnel absent from summary: %+v", summary)
	}
	_ = tunnel.Close()
	if !testutil.WaitUntil(2*time.Second, func() bool {
		f.gateway.statsMu.Lock()
		defer f.gateway.statsMu.Unlock()
		return f.gateway.open["octomus-test-runner"] == 0
	}) {
		t.Fatal("tunnel did not close")
	}
	if again := f.gateway.Collect("octomus-test-runner"); len(again.Allowed)+len(again.Denied) != 0 {
		t.Fatalf("closing a collected tunnel recreated its summary: %+v", again)
	}
}

func TestGatewayRetainsCompletedTunnelBytes(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	_, tunnel := f.connect(t, "api.openai.com:443", ProxyUser+":"+f.token)
	defer tunnel.Close()
	_ = tunnel.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(tunnel, "ping")
	var echoed [4]byte
	if _, err := io.ReadFull(tunnel, echoed[:]); err != nil {
		t.Fatal(err)
	}
	_ = tunnel.Close()
	if !testutil.WaitUntil(2*time.Second, func() bool {
		f.gateway.statsMu.Lock()
		defer f.gateway.statsMu.Unlock()
		return f.gateway.open["octomus-test-runner"] == 0
	}) {
		t.Fatal("tunnel did not close")
	}
	summary := f.gateway.Collect("octomus-test-runner")
	if got := summary.Allowed["api.openai.com:443"]; got.Count != 1 || got.Bytes != 8 {
		t.Fatalf("completed tunnel = %+v; want one connection and eight bytes", got)
	}
}

func TestGatewayRetriesOnlyPublicDNSAddresses(t *testing.T) {
	for name, tc := range map[string]struct {
		answers []netip.Addr
		want    []netip.AddrPort
		status  int
	}{
		"IPv6 fallback": {
			answers: []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("10.0.0.1"),
				netip.MustParseAddr("::ffff:93.184.216.34"), netip.MustParseAddr("1.1.1.1")},
			want:   []netip.AddrPort{netip.MustParseAddrPort("[2606:4700:4700::1111]:443"), netip.MustParseAddrPort("93.184.216.34:443")},
			status: http.StatusOK,
		},
		"all public addresses fail": {
			answers: []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("1.1.1.1")},
			want:   []netip.AddrPort{netip.MustParseAddrPort("93.184.216.34:443"), netip.MustParseAddrPort("1.1.1.1:443")},
			status: http.StatusBadGateway,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t, "runner")
			f.resolver.answers["api.openai.com"] = tc.answers
			dial := f.gateway.dial
			attempts := make(chan netip.AddrPort, 8)
			f.gateway.dial = func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
				attempts <- address
				if tc.status == http.StatusOK && address == tc.want[len(tc.want)-1] {
					return dial(ctx, address)
				}
				return nil, errors.New("unreachable address")
			}
			status, tunnel := f.connect(t, "api.openai.com:443", ProxyUser+":"+f.token)
			if tunnel != nil {
				defer tunnel.Close()
				_ = tunnel.SetDeadline(time.Now().Add(time.Second))
				if _, err := io.WriteString(tunnel, "echo"); err != nil {
					t.Fatal(err)
				}
				var echoed [4]byte
				if _, err := io.ReadFull(tunnel, echoed[:]); err != nil || string(echoed[:]) != "echo" {
					t.Fatalf("fallback tunnel = %q, %v", echoed, err)
				}
			}
			if status != tc.status {
				t.Fatalf("CONNECT = %d; want %d", status, tc.status)
			}
			for _, want := range tc.want {
				select {
				case got := <-attempts:
					if got != want {
						t.Fatalf("dialed %v; want %v", got, want)
					}
				default:
					t.Fatalf("did not try %v", want)
				}
			}
			select {
			case extra := <-attempts:
				t.Fatalf("unexpected extra dial: %v", extra)
			default:
			}
		})
	}
}

func TestGatewayStalledDialLeavesTimeForFallback(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("93.184.216.34")}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	attempts := 0
	g := &Gateway{dial: func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
		attempts++
		if address.Addr() == addresses[0] {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return client, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	conn, err := g.dialAddresses(ctx, addresses, 443)
	if err != nil || conn != client || attempts != 2 || ctx.Err() != nil {
		t.Fatalf("fallback = %v, %v after %d attempts (parent: %v)", conn, err, attempts, ctx.Err())
	}
}

func TestGatewayDialBudgetAndCancellation(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("1.1.1.1")}
	for _, cancelled := range []bool{false, true} {
		name := "deadline"
		if cancelled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			attempts := 0
			g := &Gateway{dial: func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
				attempts++
				if cancelled {
					cancel()
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			conn, err := g.dialAddresses(ctx, addresses, 443)
			want, wantAttempts := context.DeadlineExceeded, 2
			if cancelled {
				want, wantAttempts = context.Canceled, 1
			}
			if conn != nil || !errors.Is(err, want) || attempts != wantAttempts {
				t.Fatalf("dial = %v, %v after %d attempts; want %v after %d", conn, err, attempts, want, wantAttempts)
			}
		})
	}
}

func TestPublicAddressRefusesEmbeddedAndSpecialIPv6(t *testing.T) {
	for _, addr := range []string{
		"::7f00:1", "::a9fe:a9fe", "::ffff:0:a9fe:a9fe", "::ffff:0:7f00:1", "fec0::1", "3fff::1", "2001:2::1",
		"2001:10::1", "2001:20::1", "5f00::1",
	} {
		if PublicAddress(netip.MustParseAddr(addr)) {
			t.Errorf("%s was treated as public", addr)
		}
	}
	for _, addr := range []string{"2a00:1450:4001:80b::200e", "2001:4860:4860::8888"} {
		if !PublicAddress(netip.MustParseAddr(addr)) {
			t.Errorf("%s was refused", addr)
		}
	}
}

func TestGatewayResolvesRootedNames(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	status, tunnel := f.connect(t, "api.openai.com.:443", ProxyUser+":"+f.token)
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	tunnel.Close()
	f.resolver.mu.Lock()
	defer f.resolver.mu.Unlock()
	if strings.Join(f.resolver.lookups, ",") != "api.openai.com." {
		t.Fatalf("lookups = %v; want only the rooted name, so no search domain is ever appended", f.resolver.lookups)
	}
}

// plainRequest sends one raw request on a fresh connection and returns the response and whether the gateway then
// closed the connection.
func (f *gatewayFixture) plainRequest(t *testing.T, request string) (*http.Response, bool) {
	t.Helper()
	conn, err := net.Dial("tcp", f.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: strings.Fields(request)[0]})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_, err = reader.ReadByte()
	return resp, errors.Is(err, io.EOF)
}

func TestGatewayRecordsRefusedLiteralsAndPlainHTTPPorts(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	credential := ProxyUser + ":" + f.token
	auth := "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(credential)) + "\r\n"
	for _, target := range []string{"169.254.169.254:443", "[::ffff:a9fe:a9fe]:443", "[::1]:443", "localhost:443", "bad_name.example:443"} {
		if status, _ := f.connect(t, target, credential); status != http.StatusForbidden {
			t.Errorf("CONNECT %s = %d", target, status)
		}
	}
	for _, request := range []string{
		"GET http://169.254.169.254/latest/meta-data/ HTTP/1.1\r\nHost: 169.254.169.254\r\n" + auth + "\r\n",
		"GET https://evil.example:8443/ HTTP/1.1\r\nHost: evil.example:8443\r\n" + auth + "\r\n",
		"GET https://evil.example/ HTTP/1.1\r\nHost: evil.example\r\n" + auth + "\r\n",
	} {
		if resp, _ := f.plainRequest(t, request); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%q = %d", request, resp.StatusCode)
		}
	}
	denied := f.gateway.Collect("octomus-test-runner").Denied
	for key, want := range map[string]int{
		"169.254.169.254:443": 2, "[::1]:443": 1, "localhost:443": 1, "(invalid host):443": 1, "169.254.169.254:80": 1,
		"evil.example:8443": 1, "evil.example:443": 1,
	} {
		if denied[key].Count != want {
			t.Errorf("denied[%s] = %d; want %d (all: %v)", key, denied[key].Count, want, denied)
		}
	}
	if len(denied) != 7 {
		t.Errorf("denied = %v", denied)
	}
}

func TestGatewayKeepsHalfClosedTunnelUntilTheReply(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		request, _ := io.ReadAll(conn)
		time.Sleep(6 * time.Second)
		_, _ = io.WriteString(conn, "reply to "+string(request))
	}()
	f.gateway.dial = func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Addr().String())
	}
	status, tunnel := f.connect(t, "api.openai.com:443", ProxyUser+":"+f.token)
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	defer tunnel.Close()
	_ = tunnel.SetDeadline(time.Now().Add(15 * time.Second))
	_, _ = io.WriteString(tunnel, "request")
	if err := tunnel.(*bufferedConn).Conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(tunnel)
	if err != nil || string(reply) != "reply to request" {
		t.Fatalf("reply = %q, %v; a half-closed tunnel must stay open for the reply", reply, err)
	}
}

func TestGatewayClosesDeniedConnections(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	credential := "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(ProxyUser+":"+f.token)) + "\r\n"
	for name, request := range map[string]string{
		"no credential": "CONNECT api.openai.com:443 HTTP/1.1\r\nHost: api.openai.com:443\r\n\r\n",
		"unlisted":      "CONNECT attacker.example.net:443 HTTP/1.1\r\nHost: attacker.example.net:443\r\n" + credential + "\r\n",
		"plain HTTP":    "GET http://registry.npmjs.org/ HTTP/1.1\r\nHost: registry.npmjs.org\r\n" + credential + "\r\n",
	} {
		resp, closed := f.plainRequest(t, request)
		if resp.StatusCode < 400 || !resp.Close || !closed {
			t.Errorf("%s: status %d, Connection: close %v, closed %v; a denied connection must not be kept alive",
				name, resp.StatusCode, resp.Close, closed)
		}
	}
	resp, _ := f.plainRequest(t, "CONNECT api.openai.com:443 HTTP/1.1\r\nHost: api.openai.com:443\r\nX-Pad: "+
		strings.Repeat("a", 64<<10)+"\r\n"+credential+"\r\n")
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("oversized request = %d; want 431", resp.StatusCode)
	}
}

func TestGatewayDropsDecisionsArrivingAfterCollect(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	release := make(chan struct{})
	started := make(chan struct{})
	f.gateway.resolve = blockingResolver{started: started, release: release}
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, _, err := f.tryConnect("api.openai.com:443", f.credential())
		done <- result{status, err}
	}()
	<-started
	f.gateway.Collect("octomus-test-runner")
	close(release)
	if got := <-done; got.err != nil || got.status != http.StatusBadGateway {
		t.Fatalf("CONNECT = %d, %v", got.status, got.err)
	}
	f.gateway.statsMu.Lock()
	defer f.gateway.statsMu.Unlock()
	if len(f.gateway.stats) != 0 {
		t.Fatalf("a late decision recreated a collected summary that nobody will collect: %v", f.gateway.stats)
	}
}

type blockingResolver struct{ started, release chan struct{} }

func (r blockingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	close(r.started)
	<-r.release
	return nil, errors.New("no such host")
}

// fakeClock is a settable clock safe to read from the gateway's goroutines.
type fakeClock struct{ at atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.at.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.at.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.at.Add(int64(d)) }
func (c *fakeClock) install(g *Gateway)      { g.now = c.now }

func (f *gatewayFixture) credential() string { return ProxyUser + ":" + f.token }

func (f *gatewayFixture) summaries() int {
	f.gateway.statsMu.Lock()
	defer f.gateway.statsMu.Unlock()
	return len(f.gateway.stats)
}

// revoke removes the sandbox's lease file, as the broker does when it removes the sandbox.
func (f *gatewayFixture) revoke(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.leases, leaseFile(f.token))); err != nil {
		t.Fatal(err)
	}
}

func TestGatewaySweepsSummariesNobodyCollects(t *testing.T) {
	clock := newFakeClock()
	f := newGatewayFixture(t, "runner", clock.install)
	if status, _ := f.connect(t, "attacker.example.net:443", f.credential()); status != http.StatusForbidden {
		t.Fatalf("unlisted = %d", status)
	}
	f.gateway.sweep()
	clock.advance(revokedGrace)
	f.gateway.sweep()
	if f.summaries() != 1 {
		t.Fatal("a summary was dropped while its lease was still live")
	}
	f.revoke(t)
	f.gateway.sweep()
	clock.advance(revokedGrace - time.Second)
	f.gateway.sweep()
	if f.summaries() != 1 {
		t.Fatal("a summary was dropped before the broker had time to collect it")
	}
	clock.advance(time.Second)
	f.gateway.sweep()
	if f.summaries() != 0 {
		t.Fatal("a summary whose lease is gone was kept for the gateway's lifetime")
	}

	f.gateway.Collect("octomus-gone")
	clock.advance(collectedFor)
	f.gateway.sweep()
	f.gateway.statsMu.Lock()
	defer f.gateway.statsMu.Unlock()
	if len(f.gateway.collected) != 0 {
		t.Fatalf("collection marks outlived their use: %v", f.gateway.collected)
	}
}

func TestGatewayEndsTunnelsOfARevokedLease(t *testing.T) {
	f := newGatewayFixture(t, "runner", func(g *Gateway) { g.sweepEvery = 20 * time.Millisecond })
	status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	defer tunnel.Close()
	_ = tunnel.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(tunnel, "ping")
	var echoed [4]byte
	if _, err := io.ReadFull(tunnel, echoed[:]); err != nil {
		t.Fatal(err)
	}
	f.revoke(t)
	started := time.Now()
	if _, err := io.ReadAll(tunnel); err != nil || time.Since(started) > 2*time.Second {
		t.Fatalf("tunnel after revocation: %v after %s; want it closed promptly", err, time.Since(started))
	}
	if !testutil.WaitUntil(2*time.Second, func() bool { return strings.Contains(f.log.String(), `"reason":"lease revoked"`) }) {
		t.Fatalf("closing line = %s", f.log.String())
	}
}

func TestGatewayBoundsTunnelLifetimeAndHalfClosedIdle(t *testing.T) {
	for name, tc := range map[string]struct {
		configure func(*Gateway)
		halfClose bool
		reason    string
	}{
		"lifetime":         {func(g *Gateway) { g.lifetime = 200 * time.Millisecond }, false, "tunnel lifetime reached"},
		"half-closed idle": {func(g *Gateway) { g.halfCloseIdle = 200 * time.Millisecond }, true, "idle after one side closed"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t, "runner", tc.configure)
			upstream, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Close()
			hold := make(chan struct{})
			defer close(hold)
			go func() {
				conn, err := upstream.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				<-hold
			}()
			f.gateway.dial = func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Addr().String())
			}
			status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
			if status != http.StatusOK {
				t.Fatalf("tunnel = %d", status)
			}
			defer tunnel.Close()
			_ = tunnel.SetDeadline(time.Now().Add(5 * time.Second))
			if tc.halfClose {
				_ = tunnel.(*bufferedConn).Conn.(*net.TCPConn).CloseWrite()
			}
			if _, err := io.ReadAll(tunnel); err != nil {
				t.Fatalf("tunnel was not ended: %v", err)
			}
			if !testutil.WaitUntil(2*time.Second, func() bool { return strings.Contains(f.log.String(), `"reason":"`+tc.reason+`"`) }) {
				t.Fatalf("closing line = %s", f.log.String())
			}
		})
	}
}

func TestGatewayKeepsAnActiveHalfClosedTunnel(t *testing.T) {
	f := newGatewayFixture(t, "runner", func(g *Gateway) { g.halfCloseIdle = 400 * time.Millisecond })
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	const chunks = 15
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.ReadAll(conn)
		// A download that outlasts the idle bound several times over, but never pauses for as long as it.
		for range chunks {
			time.Sleep(100 * time.Millisecond)
			if _, err := conn.Write([]byte{'x'}); err != nil {
				return
			}
		}
	}()
	f.gateway.dial = func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Addr().String())
	}
	status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	defer tunnel.Close()
	_ = tunnel.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tunnel.(*bufferedConn).Conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(tunnel)
	if err != nil || len(received) != chunks {
		t.Fatalf("received %d of %d bytes, %v; a half-closed tunnel still carrying data was cut", len(received), chunks, err)
	}
	if !testutil.WaitUntil(2*time.Second, func() bool { return strings.Contains(f.log.String(), `"decision":"closed"`) }) {
		t.Fatalf("no closing line: %s", f.log.String())
	}
	if log := f.log.String(); strings.Contains(log, "idle after one side closed") {
		t.Fatalf("log = %s; an active half-closed tunnel was treated as idle", log)
	}
}

func TestGatewayLogsAllowedTunnelWhenItOpens(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	_ = tunnel.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(tunnel, "ping")
	var echoed [4]byte
	if _, err := io.ReadFull(tunnel, echoed[:]); err != nil {
		t.Fatal(err)
	}
	opened := decisions(t, f.log.String())
	if len(opened) != 1 || opened[0].Decision != "allowed" || opened[0].Host != "api.openai.com" || opened[0].Tunnel == 0 {
		t.Fatalf("log while the tunnel is open = %+v; want its allowed line", opened)
	}
	tunnel.Close()
	if !testutil.WaitUntil(2*time.Second, func() bool { return len(decisions(t, f.log.String())) == 2 }) {
		t.Fatalf("no closing line: %s", f.log.String())
	}
	closed := decisions(t, f.log.String())[1]
	if closed.Decision != "closed" || closed.Tunnel != opened[0].Tunnel || closed.BytesUp != 4 || closed.BytesDown != 4 {
		t.Fatalf("closing line = %+v", closed)
	}
}

func TestGatewayStopEndsTunnelsAndLogsThem(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	defer tunnel.Close()
	f.gateway.stop()
	_ = tunnel.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(tunnel); err != nil {
		t.Fatalf("tunnel survived the gateway stopping: %v", err)
	}
	if log := f.log.String(); !strings.Contains(log, `"decision":"closed"`) || !strings.Contains(log, `"reason":"gateway stopped"`) {
		t.Fatalf("log = %s; a stopping gateway must still log its tunnels' closing lines", log)
	}
}

func decisions(t *testing.T, log string) []Decision {
	t.Helper()
	var out []Decision
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		var d Decision
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, d)
	}
	return out
}

func TestGatewaySeparatesFailuresFromRefusals(t *testing.T) {
	f := newGatewayFixture(t, "runner", func(g *Gateway) { g.perBox = 1 })
	status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
	if status != http.StatusOK {
		t.Fatalf("tunnel = %d", status)
	}
	if status, _ := f.connect(t, "registry.npmjs.org:443", f.credential()); status != http.StatusTooManyRequests {
		t.Fatalf("over the cap = %d", status)
	}
	tunnel.Close()
	f.resolver.mu.Lock()
	delete(f.resolver.answers, "registry.npmjs.org")
	f.resolver.mu.Unlock()
	if !testutil.WaitUntil(2*time.Second, func() bool {
		status, _ := f.connect(t, "registry.npmjs.org:443", f.credential())
		return status == http.StatusBadGateway
	}) {
		t.Fatal("an unresolvable allowlisted host was not a gateway failure")
	}
	summary := f.gateway.Collect("octomus-test-runner")
	if len(summary.Denied) != 0 || summary.Failed["registry.npmjs.org:443"].Count < 2 {
		t.Fatalf("summary = %+v; an allowlisted host that could not be reached was not refused", summary)
	}
	if !strings.Contains(f.log.String(), `"decision":"failed"`) {
		t.Fatalf("log = %s", f.log.String())
	}
}

func TestGatewayBoundsRefusalLogging(t *testing.T) {
	clock := newFakeClock()
	f := newGatewayFixture(t, "runner", clock.install, func(g *Gateway) { g.refusalBurst = 3 })
	for range 10 {
		f.connect(t, "attacker.example.net:443", f.credential())
		f.connect(t, "attacker.example.net:443", "")
	}
	if got := strings.Count(f.log.String(), "\n"); got != 6 {
		t.Fatalf("logged %d lines for 20 refusals; want 3 per sandbox and 3 per credential-less source:\n%s", got, f.log.String())
	}
	if summary := f.gateway.Collect("octomus-test-runner"); summary.Denied["attacker.example.net:443"].Count != 10 {
		t.Fatalf("summary = %+v; suppressed lines must still be counted", summary)
	}
	clock.advance(logWindow)
	f.gateway.sweep()
	var suppressed []Decision
	for _, d := range decisions(t, f.log.String()) {
		if d.Decision == "suppressed" {
			suppressed = append(suppressed, d)
		}
	}
	if len(suppressed) != 2 || suppressed[0].Suppressed != 7 || suppressed[1].Suppressed != 7 {
		t.Fatalf("suppressed lines = %+v; want one count of 7 per source", suppressed)
	}
	f.connect(t, "attacker.example.net:443", f.credential())
	if last := decisions(t, f.log.String()); last[len(last)-1].Decision != "denied" {
		t.Fatalf("a new window did not log again: %+v", last[len(last)-1])
	}
}

func TestGatewayBoundsTunnelLogging(t *testing.T) {
	clock := newFakeClock()
	f := newGatewayFixture(t, "runner", clock.install, func(g *Gateway) { g.tunnelBurst = 2 })
	for range 5 {
		status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
		if status != http.StatusOK {
			t.Fatalf("tunnel = %d", status)
		}
		tunnel.Close()
	}
	if !testutil.WaitUntil(2*time.Second, func() bool {
		f.gateway.statsMu.Lock()
		defer f.gateway.statsMu.Unlock()
		return len(f.gateway.open) == 0
	}) {
		t.Fatal("tunnels did not end")
	}
	byDecision := map[string]int{}
	for _, d := range decisions(t, f.log.String()) {
		byDecision[d.Decision]++
	}
	if byDecision["allowed"] != 2 || byDecision["closed"] != 2 || len(byDecision) != 2 {
		t.Fatalf("log = %v; want the opening and closing lines of the first 2 tunnels only", byDecision)
	}
	if summary := f.gateway.Collect("octomus-test-runner"); summary.Allowed["api.openai.com:443"].Count != 5 {
		t.Fatalf("summary = %+v; tunnels past the log budget must still be counted", summary)
	}
	clock.advance(logWindow)
	f.gateway.sweep()
	last := decisions(t, f.log.String())
	if got := last[len(last)-1]; got.Decision != "suppressed" || got.Suppressed != 3 || got.Sandbox != "octomus-test-runner" ||
		got.Reason != "tunnels over the log budget" {
		t.Fatalf("suppressed line = %+v; want a count of 3 tunnels", got)
	}
}

func TestGatewayBoundsConnectionsPerSource(t *testing.T) {
	f := newGatewayFixture(t, "runner", func(g *Gateway) { g.maxPerSource = 2 })
	var held []net.Conn
	for range 2 {
		status, tunnel := f.connect(t, "api.openai.com:443", f.credential())
		if status != http.StatusOK {
			t.Fatalf("tunnel = %d", status)
		}
		defer tunnel.Close()
		held = append(held, tunnel)
	}
	extra, err := net.Dial("tcp", f.proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	_ = extra.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("connection over the per-source bound = %v; want it closed at once", err)
	}
	held[0].Close()
	if !testutil.WaitUntil(2*time.Second, func() bool {
		status, _, _ := f.tryConnect("attacker.example.net:443", f.credential())
		return status == http.StatusForbidden
	}) {
		t.Fatal("a released slot was not reusable")
	}
}
