package egress

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestProbeTargetIsOutsideTheEffectiveRunnerPolicy(t *testing.T) {
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
			if err != nil || target != c.want || !ValidProbeTarget(target) {
				t.Fatalf("target = %q, %v; want %q", target, err, c.want)
			}
			host, _, _ := net.SplitHostPort(target)
			if c.policy.Allows(wire.KindRunner, host, 443) {
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

func TestFetchProbeTargetReadsEffectivePolicyWithoutCollectingEvidence(t *testing.T) {
	rules, err := ParseRules("example.com, octomus-probe-0.invalid")
	if err != nil {
		t.Fatal(err)
	}
	gateway := New(Policy{Build: rules}, t.TempDir(), io.Discard)
	gateway.countDecision(Decision{Sandbox: "probe", Host: "other.example", Port: 443, Decision: "denied"}, "lease", nil)
	listener, socket := testutil.ListenUnix(t, "collector.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = gateway.ServeCollector(ctx, listener) }()
	for range 2 {
		if target, err := FetchProbeTarget(ctx, socket); err != nil || target != "octomus-probe-1.invalid:443" {
			t.Fatalf("collector target = %q, %v; want effective build policy respected", target, err)
		}
	}
	if got := gateway.Collect("probe").Denied["other.example:443"].Count; got != 1 {
		t.Fatalf("target lookup consumed evidence: denial count %d", got)
	}
}

func TestFetchProbeTargetFailsClosed(t *testing.T) {
	for _, target := range []string{"", "localhost:443", "169.254.169.254:443", "[::1]:443", "example.com:80", "EXAMPLE.COM:443", "example.com.:443", "example.com:443\r\nHeader: bad"} {
		t.Run(target, func(t *testing.T) {
			listener, socket := testutil.ListenUnix(t, "collector.sock")
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(target)
			})}
			go func() { _ = server.Serve(listener) }()
			defer server.Close()
			if got, err := FetchProbeTarget(context.Background(), socket); err == nil || got != "" {
				t.Fatalf("collector target = %q, %v; want invalid target rejected", got, err)
			}
		})
	}
	for _, socket := range []string{"", filepath.Join(t.TempDir(), "missing.sock")} {
		if got, err := FetchProbeTarget(context.Background(), socket); err == nil || got != "" {
			t.Fatalf("unavailable collector target = %q, %v; want failure", got, err)
		}
	}
}

func TestValidProbeTargetRequiresCanonicalDNSAndHTTPSPort(t *testing.T) {
	for _, target := range []string{"example.com:443", "octomus-probe-0.invalid:443", "sub.example.com:443"} {
		if !ValidProbeTarget(target) {
			t.Errorf("valid target %q rejected", target)
		}
	}
	for _, target := range []string{"", "localhost:443", "169.254.169.254:443", "[::1]:443", "example.com:80", "EXAMPLE.COM:443", "example.com.:443", "example.com:0443", "example.com:443\r\nHeader: bad"} {
		if ValidProbeTarget(target) {
			t.Errorf("invalid target %q accepted", target)
		}
	}
}
