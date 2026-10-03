package egress

import (
	"net"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
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
