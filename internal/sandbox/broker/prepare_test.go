package broker

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

func discard([]byte) error { return nil }

func TestCreateWarningsRefuseTheSandbox(t *testing.T) {
	e := newFakeEngine(t)
	warning := "Your kernel does not support swap limit capabilities or the cgroup is not mounted. Memory limited without swap."
	e.create = func(string, *http.Request) (int, []string) { return 0, []string{warning} }
	b := e.broker(t, testConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := b.runSandbox(ctx, plan{kind: sandbox.KindProbe, probe: sandbox.ProbeVersions, timeout: time.Minute}, discard, discard, nil)
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
		_, err := remote.Start(ctx, sandbox.Spec{Kind: sandbox.KindProbe, Probe: sandbox.ProbeVersions})
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
