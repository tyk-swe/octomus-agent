package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestUnavailableGatewayLeavesPostureUnknown(t *testing.T) {
	cfg := testConfig(t)
	cfg.EgressCollector = filepath.Join(t.TempDir(), "missing-collector.sock")
	b := newBroker(cfg)
	b.info.Egress = true
	b.info.Gateway = &wire.GatewayPosture{InstanceID: model.ID(), PolicyFingerprint: strings.Repeat("a", 64)}
	if got := b.liveInfo(context.Background()); got.Gateway != nil || !got.Egress {
		t.Fatalf("unavailable collector reused stale gateway proof: %+v", got)
	}
}

func TestBrokerInfoReadsGatewayEveryTime(t *testing.T) {
	var current atomic.Pointer[wire.GatewayPosture]
	first := &wire.GatewayPosture{InstanceID: model.ID(), PolicyFingerprint: strings.Repeat("a", 64)}
	second := &wire.GatewayPosture{InstanceID: model.ID(), PolicyFingerprint: strings.Repeat("b", 64)}
	current.Store(first)
	collector := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/posture" {
			t.Errorf("unexpected collector path: %s", r.URL.Path)
		}
		if posture := current.Load(); posture != nil {
			_ = json.NewEncoder(w).Encode(posture)
		} else {
			http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		}
	})
	cfg := testConfig(t)
	cfg.EgressCollector = testutil.UnixHTTPServer(t, collector)
	b := newBroker(cfg)
	b.info.Egress = true
	if got := b.liveInfo(context.Background()); got.Gateway == nil || *got.Gateway != *first {
		t.Fatalf("initial live gateway = %+v", got.Gateway)
	}
	current.Store(second)
	if got := b.liveInfo(context.Background()); got.Gateway == nil || *got.Gateway != *second {
		t.Fatalf("gateway restart/policy change was hidden by cache: %+v", got.Gateway)
	}
	current.Store(nil)
	if got := b.liveInfo(context.Background()); got.Gateway != nil {
		t.Fatalf("unavailable collector retained earlier gateway identity: %+v", got.Gateway)
	}
}
