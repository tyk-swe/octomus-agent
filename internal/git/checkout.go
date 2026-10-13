package git

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

// The trusted checkout is one directory that independent engine workers share: cycle planning, task
// initialization, baseline verification and the merge worker all fetch into it, and each of those
// clones from it too. Git locks a single command's own ref updates, but not two of these commands:
// concurrent fetches race each other's ref and maintenance locks, and a clone that is copying the
// checkout's objects fails when a fetch's automatic maintenance removes a lock file mid-copy
// ("failed to copy file ... objects/maintenance.lock: No such file or directory").
//
// A checkoutGate therefore serializes the checkout's writers — fetch, and the fork-head replacement
// that deletes and re-creates its refs — against each other and against its readers, a clone copying
// those objects. Readers keep running together: they only read. The gate is keyed by checkout
// directory, so independent workspaces, clones of an owned clone and other repositories keep their
// concurrency, and a waiting acquisition is cancellable, so a cancelled cycle or task stops waiting
// for the fetch it queued behind instead of holding its worker open until that fetch finishes.
type checkoutGate struct {
	mu       sync.Mutex
	readers  int
	writing  bool
	waiting  int
	released chan struct{}
}

var checkoutGates = struct {
	mu    sync.Mutex
	gates map[string]*checkoutGate
}{gates: map[string]*checkoutGate{}}

func gateFor(repository string) *checkoutGate {
	key := filepath.Clean(repository)
	checkoutGates.mu.Lock()
	defer checkoutGates.mu.Unlock()
	gate := checkoutGates.gates[key]
	if gate == nil {
		gate = &checkoutGate{released: make(chan struct{})}
		checkoutGates.gates[key] = gate
	}
	return gate
}

// withCheckout runs fn with the trusted checkout held for writing (a mutation) or reading (a clone of
// its objects). It returns the waiting error when ctx ends first; fn is then never called.
func withCheckout(ctx context.Context, c config.Config, write bool, fn func() error) error {
	release, err := gateFor(c.Repository).acquire(ctx, write)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}

func (g *checkoutGate) acquire(ctx context.Context, write bool) (func(), error) {
	g.mu.Lock()
	for !g.available(write) {
		wait := g.released
		if write {
			g.waiting++
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			if write {
				g.waiting--
				if g.waiting == 0 {
					close(g.released)
					g.released = make(chan struct{})
				}
			}
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-wait:
		}
		g.mu.Lock()
		if write {
			g.waiting--
		}
	}
	if err := ctx.Err(); err != nil {
		// Cancellation can race a release, causing the wait to receive the
		// release instead. Do not admit the cancelled work, and wake readers
		// that may still be waiting for this last writer to leave the queue.
		if write && g.waiting == 0 {
			close(g.released)
			g.released = make(chan struct{})
		}
		g.mu.Unlock()
		return nil, err
	}
	if write {
		g.writing = true
	} else {
		g.readers++
	}
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		if write {
			g.writing = false
		} else {
			g.readers--
		}
		close(g.released)
		g.released = make(chan struct{})
		g.mu.Unlock()
	}, nil
}

// available reports whether the checkout may admit the requested mode now. A reader gives way to a
// writer that is already waiting, so a steady stream of clones cannot starve the fetch that every
// later admission depends on.
func (g *checkoutGate) available(write bool) bool {
	if write {
		return !g.writing && g.readers == 0
	}
	return !g.writing && g.waiting == 0
}
