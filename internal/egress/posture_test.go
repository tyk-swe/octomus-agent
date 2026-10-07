package egress

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

func TestGatewayPostureIdentityAndPolicy(t *testing.T) {
	rules, _ := ParseRules("api.openai.com,auth.openai.com")
	first := New(Policy{Model: rules}, t.TempDir(), io.Discard)
	reordered, _ := ParseRules("auth.openai.com,api.openai.com,api.openai.com")
	second := New(Policy{Model: reordered}, t.TempDir(), io.Discard)
	changed := New(Policy{Build: reordered}, t.TempDir(), io.Discard)
	if !validGatewayPosture(first.posture) || !validGatewayPosture(second.posture) {
		t.Fatal("gateway did not create a valid boot identity and policy digest")
	}
	if first.posture.InstanceID == second.posture.InstanceID {
		t.Fatal("a restarted gateway retained the previous boot identity")
	}
	if first.posture.PolicyFingerprint != second.posture.PolicyFingerprint {
		t.Fatal("rule order or duplicates changed the effective policy fingerprint")
	}
	if first.posture.PolicyFingerprint == changed.posture.PolicyFingerprint {
		t.Fatal("moving model hosts into verification's allowlist kept the policy fingerprint")
	}
	rules[0].Host = "unlisted.example"
	if !first.policy.allows(wire.KindRunner, "api.openai.com", 443) || first.policy.allows(wire.KindRunner, "unlisted.example", 443) {
		t.Fatal("caller mutation changed the policy without a new gateway identity")
	}
}

func TestCollectorReportsLiveGatewayPosture(t *testing.T) {
	start := func(g *Gateway) string {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- g.ServeCollector(ctx, listener) }()
		t.Cleanup(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
		return "http://" + listener.Addr().String() + posturePath
	}
	client := &http.Client{Timeout: 3 * time.Second}
	var previous wire.GatewayPosture
	for _, rules := range []string{"api.openai.com", "api.openai.com,unlisted.example"} {
		model, _ := ParseRules(rules)
		g := New(Policy{Model: model}, t.TempDir(), io.Discard)
		resp, err := client.Get(start(g))
		if err != nil {
			t.Fatal(err)
		}
		var got wire.GatewayPosture
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || got != g.posture {
			t.Fatalf("collector posture = %+v, %v (HTTP %d)", got, err, resp.StatusCode)
		}
		if previous.InstanceID != "" && (got.InstanceID == previous.InstanceID || got.PolicyFingerprint == previous.PolicyFingerprint) {
			t.Fatal("a restarted gateway's changed policy was not visible to the collector")
		}
		previous = got
	}
}

func TestGatewayPostureRefusesMissingOrMalformedIdentity(t *testing.T) {
	valid := New(Policy{}, t.TempDir(), io.Discard).posture
	for _, invalid := range []wire.GatewayPosture{
		{}, {InstanceID: valid.InstanceID}, {PolicyFingerprint: valid.PolicyFingerprint},
		{InstanceID: "not-a-uuid", PolicyFingerprint: valid.PolicyFingerprint},
		{InstanceID: valid.InstanceID, PolicyFingerprint: strings.Repeat("g", 64)},
		{InstanceID: valid.InstanceID, PolicyFingerprint: strings.Repeat("a", 63)},
	} {
		if validGatewayPosture(invalid) {
			t.Fatalf("accepted unknown gateway posture: %+v", invalid)
		}
	}
}
