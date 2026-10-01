package egress

import (
	"fmt"
	"io"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// The broker turns a collected summary into the task record with model.MergeSandbox. A host the summary named must
// keep its name there, or nothing would name it: the gateway logs past its budget only the hosts it folded itself.
func TestTheRecordOfOneSandboxKeepsEveryHostItsSummaryNamed(t *testing.T) {
	g := New(Policy{}, t.TempDir(), io.Discard)
	const sandbox = "octomus-test-runner"
	open := func(host string) bool {
		t.Helper()
		_, _, unnamed := g.countDecision(Decision{Sandbox: sandbox, Host: host, Port: 443, Decision: "allowed"}, "lease")
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
