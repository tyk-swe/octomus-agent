package egress

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestLeasesGrantWhatTheGatewayLooksUpUntilRevoked(t *testing.T) {
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

func TestLeasesClearKeepsOnlyOtherFiles(t *testing.T) {
	leases := Leases{Dir: t.TempDir()}
	if _, err := leases.Grant("octomus-test-runner", "runner"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{tempPrefix + "123", "collector.sock"} {
		if err := os.WriteFile(filepath.Join(leases.Dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := leases.Clear(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(leases.Dir)
	if len(entries) != 1 || entries[0].Name() != "collector.sock" {
		t.Fatalf("after clear: %v", entries)
	}
}

func TestFetchSummaryReadsTheCollector(t *testing.T) {
	g := New(Policy{}, t.TempDir(), io.Discard)
	g.started = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	g.countDecision(Decision{Sandbox: "octomus-test-runner", Host: "api.openai.com", Port: 443, Decision: "allowed", BytesUp: 7}, "lease")
	g.countDecision(Decision{Sandbox: "octomus-test-runner", Host: "example.com", Port: 443, Decision: "denied"}, "lease")
	g.countDecision(Decision{Sandbox: "octomus-test-runner", Host: "example.org", Port: 443, Decision: "failed"}, "lease")
	listener, socket := testutil.ListenUnix(t, "collector.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = g.ServeCollector(ctx, listener) }()

	summary, err := FetchSummary(ctx, socket, "octomus-test-runner")
	want := Summary{
		Allowed: map[string]HostCount{"api.openai.com:443": {Count: 1, Bytes: 7}},
		Denied:  map[string]HostCount{"example.com:443": {Count: 1}},
		Failed:  map[string]HostCount{"example.org:443": {Count: 1}},
		// The broker tells a sandbox that made no connection from one a restarted gateway never saw by it.
		GatewayStarted: g.started,
	}
	if err != nil || !reflect.DeepEqual(summary, want) {
		t.Fatalf("summary = %+v, %v; want %+v", summary, err, want)
	}
	empty := emptySummary()
	empty.GatewayStarted = g.started
	if again, err := FetchSummary(ctx, socket, "octomus-test-runner"); err != nil || !reflect.DeepEqual(again, empty) {
		t.Fatalf("second collection = %+v, %v; want it empty", again, err)
	}
	if _, err := FetchSummary(ctx, socket, ""); err == nil || err.Error() != "400 Bad Request" {
		t.Fatalf("collection without a sandbox = %v; want the collector's status", err)
	}
}
