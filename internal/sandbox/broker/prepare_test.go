package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
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

func TestCreateThatFinishesAfterItTimedOutIsRemoved(t *testing.T) {
	tune(t, &createTimeout, 200*time.Millisecond)
	e := newFakeEngine(t)
	// The daemon stalls on the create, which then completes after the broker gave up. Docker reserves the name at
	// once but finds the container by it only once the create completes, as the fake engine does.
	e.create = func(string, *http.Request) (int, []string) {
		time.Sleep(time.Second)
		return 0, nil
	}
	b := e.broker(t, testConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.runSandbox(ctx, probePlan(time.Minute), discard, discard, nil); err == nil || !strings.Contains(err.Error(), "Creating the sandbox") {
		t.Fatalf("create past its time limit = %v; want it failed", err)
	}
	waitUntil(t, "the container whose create finished late was removed", func() bool {
		return len(e.Created()) == 1 && len(e.Remaining()) == 0
	})
}

func TestContainmentProbeHoldsARunnerLease(t *testing.T) {
	e := newFakeEngine(t)
	cfg := testConfig(t)
	cfg.LeaseDir, cfg.EgressProxy = t.TempDir(), "egress:3128"
	listener, socket := testutil.ListenUnix(t, "collector.sock")
	cfg.EgressCollector = socket
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rules, err := egress.ParseRules("example.com")
	if err != nil {
		t.Fatal(err)
	}
	gateway := egress.New(egress.Policy{Model: rules}, cfg.LeaseDir, io.Discard)
	go func() { _ = gateway.ServeCollector(ctx, listener) }()
	kinds := make(chan string, 1)
	e.run = func(c *fakeContainer) {
		if !slices.Contains(c.Spec.Env, egress.ProbeTargetEnv+"=octomus-probe-0.invalid:443") {
			t.Error("containment probe did not receive the gateway's policy-selected target")
		}
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
	if _, err := b.runSandbox(ctx, plan{kind: wire.KindProbe, probe: wire.ProbeContainment, timeout: time.Minute}, discard, discard, nil); err != nil {
		t.Fatal(err)
	}
	// The probe proves what the gateway refuses a runner sandbox; a lease of its own kind matches no allowlist.
	if kind := <-kinds; kind != wire.KindRunner {
		t.Fatalf("containment probe lease kind = %q; want runner", kind)
	}
}

func TestContainmentProbeCannotStartWithoutGatewayPolicy(t *testing.T) {
	for _, socket := range []string{"", filepath.Join(t.TempDir(), "missing.sock")} {
		t.Run(socket, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.LeaseDir, cfg.EgressProxy, cfg.EgressCollector = t.TempDir(), "egress:3128", socket
			// No engine is installed: preparation must fail before it can attempt any Docker operation.
			b := &Broker{cfg: cfg, leases: &egress.Leases{Dir: cfg.LeaseDir}}
			released := 0
			_, err := b.prepare(context.Background(), context.Background(),
				plan{kind: wire.KindProbe, probe: wire.ProbeContainment}, func() { released++ })
			if err == nil || !strings.Contains(err.Error(), "unlisted egress target") || released != 1 {
				t.Fatalf("prepare = %v, released %d; want failed before create", err, released)
			}
			files, _ := filepath.Glob(filepath.Join(cfg.LeaseDir, "*.json"))
			if len(files) != 0 {
				t.Fatalf("failed target lookup left %d egress leases", len(files))
			}
		})
	}
}
