package broker

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

type Broker struct {
	cfg    Config
	engine *engineapi.Client
	info   wire.BrokerInfo
	// slots admits sandboxes up to the limit. A sandbox gives its slot back only once its removal is confirmed.
	slots   chan struct{}
	leases  *egress.Leases
	started time.Time
	// closing closes when Serve begins to shut down.
	closing chan struct{}
	// refresh admits one request at a time to probe an image the configured tag newly resolves to. Its holder owns
	// failed.
	refresh chan struct{}
	// failed is the last rebuilt image whose probe failed, so requests soon after fail without probing it again.
	failed imageFailure
	// mu guards info and live.
	mu   sync.Mutex
	live map[string]string
}

// newBroker is a broker for cfg that has checked nothing yet.
func newBroker(cfg Config) *Broker {
	b := &Broker{
		cfg:     cfg,
		engine:  engineapi.New(cfg.DockerSocket),
		slots:   make(chan struct{}, cfg.Max),
		started: time.Now(),
		closing: make(chan struct{}),
		refresh: make(chan struct{}, 1),
		live:    map[string]string{},
	}
	if cfg.LeaseDir != "" {
		b.leases = &egress.Leases{Dir: cfg.LeaseDir}
	}
	return b
}

// Info is what the broker serves the control plane about itself.
func (b *Broker) Info() wire.BrokerInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	info := b.info
	info.Live = len(b.live)
	return info
}

// logf reports what the broker could not do on its own, such as a removal it keeps retrying.
func (b *Broker) logf(format string, args ...any) {
	w := b.cfg.Log
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "Octomus sandbox broker: "+format+"\n", args...)
}
