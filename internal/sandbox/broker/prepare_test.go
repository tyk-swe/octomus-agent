package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

func discard([]byte) error { return nil }

func TestCreateWarningsRefuseTheSandbox(t *testing.T) {
	e := newFakeEngine(t)
	warning := "Your kernel does not support swap limit capabilities or the cgroup is not mounted. Memory limited without swap."
	e.create = func(string, *http.Request) (int, []string) { return 0, []string{warning} }
	b := e.broker(t, testConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := b.runSandbox(ctx, plan{kind: wire.KindProbe, probe: wire.ProbeVersions, timeout: time.Minute}, discard, discard, nil)
	if err == nil || !strings.Contains(err.Error(), "swap limit") {
		t.Fatalf("create with a dropped limit = %v; want a refusal naming the warning", err)
	}
	created := e.Created()
	if len(created) != 1 || created[0].Running() || len(e.Remaining()) != 0 {
		t.Fatalf("a sandbox whose limits Docker dropped was started or left behind (remaining %v)", e.Remaining())
	}
}

func TestClientGivingUpDuringCreateLeavesNoContainer(t *testing.T) {
	e := newFakeEngine(t)
	creating, finish := make(chan struct{}), make(chan struct{})
	// The daemon finishes a create it has started even when the broker stops waiting for it.
	e.create = func(string, *http.Request) (int, []string) {
		close(creating)
		<-finish
		return 0, nil
	}
	b := e.broker(t, testConfig(t))
	remote := serve(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() {
		_, err := remote.Start(ctx, sandbox.Spec{Kind: sandbox.KindProbe, Probe: wire.ProbeVersions})
		started <- err
	}()
	select {
	case <-creating:
	case <-time.After(5 * time.Second):
		t.Fatal("create did not start")
	}
	cancel()
	if err := <-started; err == nil {
		t.Fatal("a cancelled start succeeded")
	}
	time.Sleep(200 * time.Millisecond)
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for len(e.Remaining()) != 0 || len(e.Created()) != 1 || b.Info().Live != 0 || len(b.slots) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after a client gave up during create: created %d, remaining %v, live %d, slots %d",
				len(e.Created()), e.Remaining(), b.Info().Live, len(b.slots))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContainmentProbeHoldsARunnerLease(t *testing.T) {
	e := newFakeEngine(t)
	cfg := testConfig(t)
	cfg.LeaseDir, cfg.EgressProxy = t.TempDir(), "egress:3128"
	kinds := make(chan string, 1)
	e.run = func(c *fakeContainer) {
		kind := "no lease"
		for _, entry := range c.Spec.Env {
			if strings.HasPrefix(entry, "HTTPS_PROXY=http://"+egress.ProxyUser+":") {
				// The sandbox's lease is the only one granted.
				var lease egress.Lease
				files, _ := filepath.Glob(filepath.Join(cfg.LeaseDir, "*.json"))
				if len(files) == 1 {
					if data, err := os.ReadFile(files[0]); err == nil && json.Unmarshal(data, &lease) == nil {
						kind = lease.Kind
					}
				}
			}
		}
		kinds <- kind
		c.End(0)
	}
	b := e.broker(t, cfg)
	b.leases = &egress.Leases{Dir: cfg.LeaseDir}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.runSandbox(ctx, plan{kind: wire.KindProbe, probe: wire.ProbeContainment, timeout: time.Minute}, discard, discard, nil); err != nil {
		t.Fatal(err)
	}
	// The probe proves what the gateway refuses a runner sandbox; a lease of its own kind matches no allowlist.
	if kind := <-kinds; kind != wire.KindRunner {
		t.Fatalf("containment probe lease kind = %q; want runner", kind)
	}
}
