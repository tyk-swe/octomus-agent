package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// probeGateway serves the real egress gateway with one lease of the given kind and returns the proxy address a
// sandbox would be given. Nothing resolves or dials: a target that gets past the allowlist fails at resolution.
func probeGateway(t *testing.T, kind, model string) (string, string) {
	t.Helper()
	leases := t.TempDir()
	token := strings.Repeat("cd", 32)
	data, _ := json.Marshal(sandbox.Lease{Sandbox: "octomus-test-probe", Kind: kind})
	if err := os.WriteFile(filepath.Join(leases, sandbox.LeaseFile(token)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := egress.ParseRules(model)
	if err != nil {
		t.Fatal(err)
	}
	gateway := egress.New(egress.Policy{Model: rules}, leases, io.Discard).WithNetwork(noResolver{},
		func(context.Context, netip.AddrPort) (net.Conn, error) { return nil, errors.New("no network in tests") })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: gateway}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	return listener.Addr().String(), token
}

type noResolver struct{}

func (noResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("no DNS in tests")
}

// The probe sits on the runner network, so its refusals must come from the runner allowlist itself. A lease the
// gateway gives nothing, or a credential it does not accept, refuses everything and proves nothing about that list.
func TestProbeEgressRefusalsComeFromTheRunnerAllowlist(t *testing.T) {
	cases := []struct {
		name, kind, model string
		credential        func(token string) string
		refused           bool
		detail            string
	}{
		{"runner lease", "runner", "api.openai.com", func(token string) string { return "sandbox:" + token }, true,
			"refused example.com:443, 169.254.169.254:80, localhost:4200"},
		{"probe lease the gateway gives nothing", "probe", "api.openai.com", func(token string) string { return "sandbox:" + token },
			false, "not proven refused: example.com:443 (HTTP 403 for another reason: Octomus egress blocked this connection: host is not on the probe allowlist)"},
		{"unknown credential", "runner", "api.openai.com", func(string) string { return "sandbox:" + strings.Repeat("ef", 32) },
			false, "not proven refused: example.com:443 (the gateway refused the probe's credential, HTTP 407)"},
		{"an allowlist that holds the unlisted name", "runner", "example.com", func(token string) string { return "sandbox:" + token },
			false, "not proven refused: example.com:443 (HTTP 502)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			address, token := probeGateway(t, c.kind, c.model)
			refused, detail := sandbox.EgressRefusals("http://" + c.credential(token) + "@" + address)
			if refused != c.refused || !strings.Contains(detail, c.detail) {
				t.Fatalf("egress refusals = %v, %q; want %v with %q", refused, detail, c.refused, c.detail)
			}
		})
	}
}
