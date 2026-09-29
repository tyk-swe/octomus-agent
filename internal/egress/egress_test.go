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
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
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
	if answers, ok := r.answers[host]; ok {
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
	log      *lockedBuffer
}

type lockedBuffer struct {
	mu   sync.Mutex
	data strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func newGatewayFixture(t *testing.T, kind string) *gatewayFixture {
	t.Helper()
	leases := t.TempDir()
	token := strings.Repeat("ab", 32)
	data, _ := json.Marshal(sandbox.Lease{Sandbox: "octomus-test-" + kind, Kind: kind})
	if err := os.WriteFile(filepath.Join(leases, sandbox.LeaseFile(token)), data, 0o600); err != nil {
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
		token: token, dialed: make(chan netip.AddrPort, 8), log: &lockedBuffer{},
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: f.gateway}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	f.proxy = listener.Addr().String()
	return f
}

// connect opens a CONNECT tunnel as a client honouring HTTPS_PROXY with credentials would.
func (f *gatewayFixture) connect(t *testing.T, target, credential string) (int, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", f.proxy)
	if err != nil {
		t.Fatal(err)
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if credential != "" {
		request += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(credential)) + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return resp.StatusCode, nil
	}
	return resp.StatusCode, &bufferedConn{Conn: conn, r: reader}
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func TestGatewayTunnelsOnlyAllowlistedHostsToPublicAddresses(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	credential := sandbox.ProxyUser + ":" + f.token
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
	if lookups != "api.openai.com,internal.example.com" {
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
		"short token": sandbox.ProxyUser + ":abc",
		"unknown":     sandbox.ProxyUser + ":" + strings.Repeat("cd", 32),
		"not hex":     sandbox.ProxyUser + ":" + strings.Repeat("zz", 32),
	} {
		if status, _ := f.connect(t, "registry.npmjs.org:443", credential); status != http.StatusProxyAuthRequired {
			t.Errorf("%s credential = %d; want 407", name, status)
		}
	}
	credential := sandbox.ProxyUser + ":" + f.token
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
	credential := sandbox.ProxyUser + ":" + f.token
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
			status, tunnel := f.connect(t, "api.openai.com:443", sandbox.ProxyUser+":"+f.token)
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
