package engine

import (
	"context"
	"sync"
)

// planningOrder keeps asynchronous preflight completion from reordering the
// fleet's rotating planning admissions. Checks run concurrently; only their
// final capacity claims wait for earlier checks to finish or fail.
type planningOrder struct {
	mu      sync.Mutex
	pending []*planningTurn
}

type planningTurn struct {
	order    *planningOrder
	ready    chan struct{}
	finished bool
	notified bool
}

func (o *planningOrder) enqueue() *planningTurn {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	turn := &planningTurn{order: o, ready: make(chan struct{})}
	o.pending = append(o.pending, turn)
	if len(o.pending) == 1 {
		turn.notified = true
		close(turn.ready)
	}
	return turn
}

func (t *planningTurn) wait(ctx context.Context) error {
	if t == nil {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ready:
		return ctx.Err()
	}
}

func (t *planningTurn) release() {
	if t == nil {
		return
	}
	o := t.order
	o.mu.Lock()
	defer o.mu.Unlock()
	t.finished = true
	for len(o.pending) > 0 && o.pending[0].finished {
		o.pending[0] = nil
		o.pending = o.pending[1:]
	}
	if len(o.pending) > 0 && !o.pending[0].notified {
		o.pending[0].notified = true
		close(o.pending[0].ready)
	}
}
