// The egress gateway: address vetting, allowlists by sandbox kind, CONNECT tunnelling, leases and the probe target.

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

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestPublicAddress(t *testing.T) {
	for _, tc := range []struct {
		name   string
		public bool
		addrs  []string
	}{
		{"internal IPv4 and IPv6 ranges", false, []string{
			"127.0.0.1", "10.1.2.3", "172.17.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
			"0.1.2.3", "198.18.0.1", "192.0.2.1", "203.0.113.9", "240.0.0.1", "255.255.255.255", "224.0.0.1",
			"::1", "::", "fe80::1", "fd00:ec2::254", "fc00::1", "ff02::1", "::ffff:10.0.0.1", "::ffff:169.254.169.254",
			"64:ff9b::a00:1", "2002:a00:1::1", "2001:0:4136:e378::1", "2001:db8::1",
		}},
		{"embedded and special IPv6", false, []string{
			"::7f00:1", "::a9fe:a9fe", "::ffff:0:a9fe:a9fe", "::ffff:0:7f00:1", "fec0::1", "3fff::1", "2001:2::1",
			"2001:10::1", "2001:20::1", "5f00::1",
		}},
		{"public addresses", true, []string{
			"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111", "::ffff:8.8.8.8", "2a00:1450:4001:80b::200e", "2001:4860:4860::8888",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, addr := range tc.addrs {
				if publicAddress(netip.MustParseAddr(addr)) != tc.public {
					t.Errorf("publicAddress(%s) = %v; want %v", addr, !tc.public, tc.public)
				}
			}
		})
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
		if got := policy.allows(tc.kind, tc.host, tc.port); got != tc.want {
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

func TestGatewayAllowlist(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	credential := proxyUser + ":" + f.token
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
	summary := f.gateway.collect("octomus-test-runner")
	if summary.Allowed["api.openai.com:443"].Count != 1 || summary.Denied["attacker.example.net:443"].Count != 1 ||
		summary.Denied["internal.example.com:443"].Count != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if again := f.gateway.collect("octomus-test-runner"); len(again.Allowed)+len(again.Denied) != 0 {
		t.Fatal("a collected summary must be forgotten")
	}
	if log := f.log.String(); !strings.Contains(log, `"decision":"denied"`) || !strings.Contains(log, `"host":"attacker.example.net"`) {
		t.Fatalf("decision log = %s", log)
	}
}

func TestGatewayLeases(t *testing.T) {
	f := newGatewayFixture(t, "verify")
	for name, credential := range map[string]string{
		"missing":     "",
		"wrong user":  "someone:" + f.token,
		"short token": proxyUser + ":abc",
		"unknown":     proxyUser + ":" + strings.Repeat("cd", 32),
		"not hex":     proxyUser + ":" + strings.Repeat("zz", 32),
	} {
		if status, _ := f.connect(t, "registry.npmjs.org:443", credential); status != http.StatusProxyAuthRequired {
			t.Errorf("%s credential = %d; want 407", name, status)
		}
	}
	credential := proxyUser + ":" + f.token
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

func TestGatewayClosesDeniedConnections(t *testing.T) {
	f := newGatewayFixture(t, "runner")
	credential := "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUser+":"+f.token)) + "\r\n"
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

func TestLeases(t *testing.T) {
	leases := Leases{Dir: t.TempDir()}
	token, err := leases.Grant("octomus-test-runner", "runner")
	if err != nil {
		t.Fatal(err)
	}
	lease, file, ok := leases.lookup(token)
	if !ok || lease != (Lease{Sandbox: "octomus-test-runner", Kind: "runner"}) || file != leaseFile(token) {
		t.Fatalf("lookup = %+v, %q, %v", lease, file, ok)
	}
	info, err := os.Stat(filepath.Join(leases.Dir, file))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lease file = %v, %v; want it private", info, err)
	}
	if other, _ := leases.Grant("octomus-test-verify", "verify"); other == token {
		t.Fatal("two grants share a credential")
	}
	leases.Revoke(token)
	if _, _, ok := leases.lookup(token); ok {
		t.Fatal("a revoked lease is still live")
	}
	if want := "http://sandbox:" + token + "@egress:3128"; ProxyURL("egress:3128", token) != want {
		t.Fatalf("proxy URL = %q; want %q", ProxyURL("egress:3128", token), want)
	}
}

func TestProbeTarget(t *testing.T) {
	rule := func(host string, wildcard bool) Rule { return Rule{Host: host, Wildcard: wildcard, Port: 443} }
	cases := []struct {
		name   string
		policy Policy
		want   string
	}{
		{"empty policy", Policy{}, "example.com:443"},
		{"model allows old target", Policy{Model: []Rule{rule("example.com", false)}}, "octomus-probe-0.invalid:443"},
		{"build allows old target", Policy{Build: []Rule{rule("example.com", false)}}, "octomus-probe-0.invalid:443"},
		// A broad suffix is not accepted by ParseRules, but the selector still handles such a policy without relying
		// on the old fixed name or changing what the policy permits.
		{"wildcard covers old target", Policy{Model: []Rule{rule("com", true)}}, "octomus-probe-0.invalid:443"},
		{"reserved candidates allowed in both lists", Policy{
			Model: []Rule{rule("example.com", false), rule("octomus-probe-0.invalid", false)},
			Build: []Rule{rule("octomus-probe-1.invalid", false), rule("probe.invalid", true)},
		}, "octomus-probe-2.invalid:443"},
		{"different port does not allow target", Policy{Build: []Rule{{Host: "example.com", Port: 8443}}}, "example.com:443"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, err := c.policy.probeTarget()
			if err != nil || target != c.want || !wire.ValidProbeTarget(target) {
				t.Fatalf("target = %q, %v; want %q", target, err, c.want)
			}
			host, _, _ := net.SplitHostPort(target)
			if c.policy.allows(wire.KindRunner, host, 443) {
				t.Fatalf("selected target %q is allowed by runner policy", target)
			}
		})
	}
	// An unvalidated policy can exhaust the reserved-name search; it must never yield an allowed target.
	all := Policy{Model: []Rule{rule("example.com", false), rule("invalid", true)}}
	if target, err := all.probeTarget(); err == nil || target != "" {
		t.Fatalf("exhausted candidates = %q, %v; want failure", target, err)
	}
}
