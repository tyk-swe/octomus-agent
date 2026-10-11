package broker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// Teardown bounds. Once a sandbox ends, or a kill is asked for, the broker drains its output, reads its state and
// evidence, and confirms its removal within teardownBudget, so the control plane's wait for the report holds.
const (
	teardownBudget = wire.TeardownBudget
	// removeReserve is the part of the budget kept for the removal.
	removeReserve = 15 * time.Second
	// stopWait is how long a killed sandbox has to report its exit before the removal stops it by force.
	stopWait = 15 * time.Second
	// killWait bounds one kill request. Docker answers a SIGKILL only once the container has stopped, so a busy
	// daemon can deliver a kill whose answer comes later than that.
	killWait = 5 * time.Second
	// drainWait is how long a sandbox's output has to end once the sandbox has.
	drainWait = 10 * time.Second
	// joinWait is how long the output pump has to finish once its source is closed.
	joinWait = 5 * time.Second
	// reapDelay is the first pause before a removal that failed is tried again.
	reapDelay = 5 * time.Second
)

// sweep removes sandboxes this instance left behind. Only containers carrying this broker's instance label are ever
// touched; everything else on the host is not the broker's to manage. One failure does not stop the rest, and every
// egress lease is cleared even then: no sandbox an earlier broker started keeps its way out.
func (b *Broker) sweep(ctx context.Context) error {
	var errs []error
	containers, err := b.engine.containerList(ctx, map[string]string{instanceLabel: b.cfg.Instance})
	if err != nil {
		errs = append(errs, fmt.Errorf("Listing leftover sandboxes: %w", err))
	}
	for _, container := range containers {
		if container.Labels[instanceLabel] != b.cfg.Instance {
			continue
		}
		if err := b.engine.containerRemove(ctx, container.ID); err != nil {
			errs = append(errs, fmt.Errorf("Removing leftover sandbox %s: %w", container.ID, err))
			continue
		}
		b.mu.Lock()
		var owners []*prepared
		for owned := range b.live {
			if owned.id == container.ID || slices.Contains(container.Names, "/"+owned.name) {
				owners = append(owners, owned)
			}
		}
		b.mu.Unlock()
		for _, owned := range owners {
			owned.forget()
		}
	}
	if b.leases != nil {
		if err := b.leases.Clear(); err != nil {
			errs = append(errs, fmt.Errorf("Clearing egress leases: %w", err))
		}
	}
	return errors.Join(errs...)
}

// cleanupOutcome records independent authorization and container cleanup. A nil result confirms that resource is
// gone. A reaper owns every remaining failure; only the container result determines admission capacity.
type cleanupOutcome struct {
	lease, container error
	id               string
}

func (c cleanupOutcome) err() error {
	var errs []error
	if c.lease != nil {
		errs = append(errs, fmt.Errorf("Revoking the sandbox egress lease: %w", c.lease))
	}
	if c.container != nil {
		errs = append(errs, fmt.Errorf("Removing the sandbox: %w", c.container))
	}
	return errors.Join(errs...)
}

// remove closes the attach stream, tries lease revocation and removes the container, retrying removal until ctx
// ends. Failed revocation never prevents removal. The reaper owns either outstanding resource, even when confirmed
// container removal has already freed the slot.
func (s *prepared) remove(ctx context.Context) error {
	s.once.Do(func() {
		if s.attach != nil {
			s.attach.close()
		}
		if s.lease != "" {
			s.cleanup.lease = s.b.leases.Revoke(s.lease)
			if s.cleanup.lease != nil {
				s.b.logf("Revoking the egress lease of sandbox %s failed; the broker retries it: %v", s.name, s.cleanup.lease)
			}
		}
		s.cleanup.id = s.id
		for delay := 250 * time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
			s.cleanup.container = s.removeContainer(ctx, &s.cleanup.id)
			if s.cleanup.container == nil || ctx.Err() != nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(delay):
			}
		}
		if s.cleanup.container != nil {
			s.b.logf("Removing sandbox %s failed; it keeps its slot while the broker retries: %v", s.name, s.cleanup.container)
		} else {
			s.forget()
		}
		if s.cleanup.err() != nil {
			s.b.reap(s, s.cleanup)
		}
	})
	return s.cleanup.err()
}

// discard removes a sandbox that never ran.
func (s *prepared) discard() error {
	ctx, cancel := context.WithTimeout(context.Background(), teardownBudget)
	defer cancel()
	return s.remove(ctx)
}

func (s *prepared) forget() {
	s.b.mu.Lock()
	delete(s.b.live, s)
	s.b.mu.Unlock()
	s.release()
}

// removeContainer first observes an unanswered create by its reserved name. Not-found while that daemon operation
// is unresolved cannot confirm absence: it may complete later. Once observed, keep the ID so a later 404 confirms
// removal rather than reverting to uncertainty. Time alone never releases that admission slot.
func (s *prepared) removeContainer(ctx context.Context, id *string) error {
	if *id == "" {
		container, err := s.b.engine.containerInspect(ctx, s.name)
		if err != nil {
			return fmt.Errorf("Creation of sandbox %s is unconfirmed: %w", s.name, err)
		}
		if container.ID == "" {
			return errors.New("Docker Engine reported an unconfirmed sandbox without an ID")
		}
		*id = container.ID
	}
	return s.b.engine.containerRemove(ctx, *id)
}

// reap retries each unconfirmed cleanup independently. It holds capacity until container removal is confirmed;
// pending lease revocation can outlive that slot. Shutdown hands both resources to the final sweep.
func (b *Broker) reap(s *prepared, pending cleanupOutcome) {
	go func() {
		for delay := reapDelay; ; delay = min(2*delay, 5*time.Minute) {
			select {
			case <-time.After(delay):
			case <-b.closing:
				return
			}
			if pending.lease != nil {
				pending.lease = b.leases.Revoke(s.lease)
				if pending.lease == nil {
					b.logf("Revoked the egress lease of sandbox %s after earlier failures", s.name)
				}
			}
			if pending.container != nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				pending.container = s.removeContainer(ctx, &pending.id)
				cancel()
				if pending.container == nil {
					b.logf("Removed sandbox %s after earlier failures", s.name)
					s.forget()
				}
			}
			if pending.err() == nil {
				return
			}
		}
	}()
}
