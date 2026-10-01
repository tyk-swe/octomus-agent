package egress

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// The broker turns a collected summary into the task record with model.MergeSandbox. A host the summary named must
// keep its name there, or nothing would name it: the gateway logs past its budget only the hosts it folded itself.
func TestTheRecordOfOneSandboxKeepsEveryHostItsSummaryNamed(t *testing.T) {
	g := New(Policy{}, t.TempDir(), io.Discard)
	const sandbox = "octomus-test-runner"
	open := func(host string) bool {
		t.Helper()
		_, _, unnamed := g.countDecision(Decision{Sandbox: sandbox, Host: host, Port: 443, Decision: "allowed"}, "lease", nil)
		return unnamed
	}
	// The last host named sorts after "other", and the host after it is folded.
	for i := range summaryHostLimit - 1 {
		if open(fmt.Sprintf("a%02d.cdn.example", i)) {
			t.Fatalf("host %d within the limit was reported folded", i)
		}
	}
	if open("z.cdn.example") {
		t.Fatal("the last host within the limit was reported folded")
	}
	if !open("a63.cdn.example") {
		t.Fatal("the first host past the limit was not reported folded")
	}
	summary := g.Collect(sandbox)
	if len(summary.Allowed) != summaryHostLimit+1 || summary.Allowed["other"].Count != 1 {
		t.Fatalf("summary = %d hosts, other %+v; want every host but the last named", len(summary.Allowed), summary.Allowed["other"])
	}
	// What the broker records for one sandbox.
	run := &model.SandboxRecord{Runs: 1, Egress: model.SandboxEgress{Allowed: map[string]uint64{}}}
	for host, count := range summary.Allowed {
		run.Egress.Allowed[host] = uint64(count.Count)
	}
	record := model.MergeSandbox(nil, run)
	for host, count := range summary.Allowed {
		if record.Egress.Allowed[host] != uint64(count.Count) {
			t.Fatalf("record counts %s %d times, the summary %d; want every named host kept (record %v)",
				host, record.Egress.Allowed[host], count.Count, record.Egress.Allowed)
		}
	}
	if len(record.Egress.Allowed) != len(summary.Allowed) {
		t.Fatalf("record = %d hosts; want the summary's %d", len(record.Egress.Allowed), len(summary.Allowed))
	}
}

// These handlers use injected resolution/dialing and an in-memory HTTP recorder: collection must be honest
// even before a CONNECT has produced an HTTP response or an allowed tunnel.
func TestCollectorDistinguishesPendingAndResolvedConnects(t *testing.T) {
	for _, stage := range []string{"DNS", "dial"} {
		for _, collectPending := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pending=%v", stage, collectPending), func(t *testing.T) {
				const name = "octomus-test-runner"
				rules, err := ParseRules("api.openai.com")
				if err != nil {
					t.Fatal(err)
				}
				log := &testutil.SyncBuffer{}
				g := New(Policy{Model: rules}, t.TempDir(), log)
				token, err := g.leases.Grant(name, "runner")
				if err != nil {
					t.Fatal(err)
				}
				started, release := make(chan struct{}), make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				wait := func(ctx context.Context) error {
					close(started)
					select {
					case <-release:
						return errors.New("fixture connection failure")
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				g.WithNetwork(resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
					if stage == "DNS" {
						return nil, wait(ctx)
					}
					return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
				}), func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
					return nil, wait(ctx)
				})
				request := func(target string) *http.Request {
					r := httptest.NewRequest(http.MethodConnect, target, nil)
					r.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(ProxyUser+":"+token)))
					return r
				}
				// Known decisions survive collection alongside the pending request.
				g.ServeHTTP(httptest.NewRecorder(), request("unlisted.example:443"))
				response, done := httptest.NewRecorder(), make(chan struct{})
				go func() {
					defer close(done)
					g.ServeHTTP(response, request("api.openai.com:443"))
				}()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("CONNECT did not reach the injected network step")
				}
				finish := func() {
					t.Helper()
					unblock()
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Fatal("CONNECT did not finish")
					}
					if response.Code != http.StatusBadGateway {
						t.Fatalf("CONNECT status = %d; want 502", response.Code)
					}
				}
				if !collectPending {
					finish()
				}
				summary := g.Collect(name)
				// The collector's additive JSON field survives the gateway-to-broker wire boundary.
				data, err := json.Marshal(summary)
				if err != nil {
					t.Fatal(err)
				}
				var decoded Summary
				if err := json.Unmarshal(data, &decoded); err != nil || decoded.Incomplete != collectPending {
					t.Fatalf("summary wire round-trip = %+v, %v", decoded, err)
				}
				if summary.Incomplete != collectPending || summary.Denied["unlisted.example:443"].Count != 1 {
					t.Fatalf("collected = %+v; want incomplete %v and the known refusal", summary, collectPending)
				}
				wantFailures := 1
				if collectPending {
					wantFailures = 0
					// Repeating collection before resolution must not erase the known incompleteness.
					if again := g.Collect(name); !again.Incomplete || len(again.Denied) != 0 {
						t.Fatalf("repeated pending collection = %+v", again)
					}
					finish()
				}
				if summary.Failed["api.openai.com:443"].Count != wantFailures {
					t.Fatalf("collected failures = %+v; want %d", summary.Failed, wantFailures)
				}
				if again := g.Collect(name); again.Incomplete != collectPending || len(again.Allowed)+len(again.Denied)+len(again.Failed) != 0 {
					t.Fatalf("late outcome recreated evidence or cleared incompleteness: %+v", again)
				}
				if len(g.stats) != 0 || len(g.open) != 0 {
					t.Fatalf("finished request retained usage: stats=%v, open=%v", g.stats, g.open)
				}
				if !strings.Contains(log.String(), `"decision":"failed"`) {
					t.Fatalf("eventual failure is missing from the log: %s", log.String())
				}
				// A stale authenticated request after collection cannot make an unrecorded network attempt.
				late := httptest.NewRecorder()
				g.ServeHTTP(late, request("api.openai.com:443"))
				if late.Code != http.StatusForbidden || len(g.stats) != 0 {
					t.Fatalf("post-collection CONNECT = %d, usage %v", late.Code, g.stats)
				}
			})
		}
	}
}

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func TestCollectorRetainsPendingUsageThroughLeaseSweep(t *testing.T) {
	g := New(Policy{}, t.TempDir(), io.Discard)
	clock := newFakeClock()
	clock.install(g)
	const name = "octomus-test-runner"
	token, err := g.leases.Grant(name, "runner")
	if err != nil {
		t.Fatal(err)
	}
	pending := g.beginDecision(name, leaseFile(token))
	g.leases.Revoke(token)
	g.sweep()
	clock.advance(revokedGrace)
	g.sweep()
	if summary := g.Collect(name); !summary.Incomplete {
		t.Fatalf("sweep lost a pending request: %+v", summary)
	}
	g.abandonDecision(pending)
	clock.advance(collectedFor)
	g.sweep()
	if len(g.stats)+len(g.collected) != 0 {
		t.Fatalf("finished evidence outlived its retention: stats=%v, collected=%v", g.stats, g.collected)
	}
}

func TestCollectorMarksAbandonedDecisionIncomplete(t *testing.T) {
	g := New(Policy{}, t.TempDir(), io.Discard)
	const name = "octomus-test-runner"
	pending := g.beginDecision(name, "lease")
	g.abandonDecision(pending)
	if summary := g.Collect(name); !summary.Incomplete {
		t.Fatalf("request without an outcome looked complete: %+v", summary)
	}
}

func TestCollectorKeepsRecordedOpenTunnelComplete(t *testing.T) {
	g := New(Policy{}, t.TempDir(), io.Discard)
	const name = "octomus-test-runner"
	pending := g.beginDecision(name, "lease")
	if !g.reserve(name) {
		t.Fatal("could not reserve tunnel")
	}
	defer g.releaseTunnel(name)
	entry, key, _ := g.countDecision(Decision{Sandbox: name, Host: "api.openai.com", Port: 443, Decision: "allowed"}, "lease", pending)
	if summary := g.Collect(name); summary.Incomplete || summary.Allowed["api.openai.com:443"].Count != 1 {
		t.Fatalf("already-recorded open tunnel = %+v; want complete", summary)
	}
	g.addTunnelBytes(name, entry, key, 12)
	if len(g.stats) != 0 {
		t.Fatalf("closing bytes recreated collected evidence: %v", g.stats)
	}
}

func TestCollectorSettlesDecisionAtomically(t *testing.T) {
	g := New(Policy{}, t.TempDir(), io.Discard)
	for i := range 100 {
		name := fmt.Sprintf("octomus-test-%d", i)
		pending := g.beginDecision(name, "lease")
		start, counted, collected := make(chan struct{}), make(chan struct{}), make(chan Summary, 1)
		go func() {
			<-start
			g.countDecision(Decision{Sandbox: name, Host: "api.openai.com", Port: 443, Decision: "failed"}, "lease", pending)
			close(counted)
		}()
		go func() {
			<-start
			collected <- g.Collect(name)
		}()
		close(start)
		summary := <-collected
		<-counted
		if !summary.Incomplete && summary.Failed["api.openai.com:443"].Count != 1 {
			t.Fatalf("collection raced past a decision without marking it incomplete: %+v", summary)
		}
		if len(g.stats) != 0 {
			t.Fatalf("late decision recreated collected usage: %v", g.stats)
		}
	}
}
