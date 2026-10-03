package broker

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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
