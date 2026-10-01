package broker

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Teardown bounds. Once a sandbox ends, or a kill is asked for, the broker drains its output, reads its state and
// evidence, and confirms its removal within teardownBudget, so the control plane's wait for the report holds.
var (
	teardownBudget = 45 * time.Second
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
	containers, err := b.engine.ContainerList(ctx, map[string]string{instanceLabel: b.cfg.Instance})
	if err != nil {
		errs = append(errs, fmt.Errorf("Listing leftover sandboxes: %w", err))
	}
	for _, container := range containers {
		if container.Labels[instanceLabel] != b.cfg.Instance {
			continue
		}
		if err := b.engine.ContainerRemove(ctx, container.ID); err != nil {
			errs = append(errs, fmt.Errorf("Removing leftover sandbox %s: %w", container.ID, err))
		}
	}
	if b.leases != nil {
		if err := b.leases.Clear(); err != nil {
			errs = append(errs, fmt.Errorf("Clearing egress leases: %w", err))
		}
	}
	return errors.Join(errs...)
}

// remove closes the attach stream, revokes the egress lease and removes the container, retrying until ctx ends. Only
// a confirmed removal frees the sandbox's live entry and slot; otherwise a reaper keeps both while it keeps trying,
// so the cap on running sandboxes holds.
func (s *prepared) remove(ctx context.Context) error {
	s.once.Do(func() {
		if s.attach != nil {
			s.attach.Close()
		}
		// Egress is cut at once; that needs no confirmation.
		if s.lease != "" {
			s.b.leases.Revoke(s.lease)
		}
		if s.removed = s.b.removeContainer(ctx, s.id); s.removed != nil {
			s.b.logf("Removing sandbox %s failed; it keeps its slot while the broker retries: %v", s.name, s.removed)
			s.b.reap(s)
			return
		}
		s.forget()
	})
	return s.removed
}

// discard removes a sandbox that never ran.
func (s *prepared) discard() {
	ctx, cancel := context.WithTimeout(context.Background(), teardownBudget)
	defer cancel()
	_ = s.remove(ctx)
}

func (s *prepared) forget() {
	s.b.mu.Lock()
	delete(s.b.live, s.id)
	s.b.mu.Unlock()
	s.release()
}

// removeContainer force-removes a container, retrying until the daemon confirms it is gone or ctx ends.
func (b *Broker) removeContainer(ctx context.Context, id string) error {
	for delay := 250 * time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
		err := b.engine.ContainerRemove(ctx, id)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
}

// reap keeps removing a sandbox whose teardown could not confirm its removal. Shutdown hands it to the final sweep.
func (b *Broker) reap(s *prepared) {
	go func() {
		for delay := reapDelay; ; delay = min(2*delay, 5*time.Minute) {
			select {
			case <-time.After(delay):
			case <-b.closing:
				s.forget()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := b.engine.ContainerRemove(ctx, s.id)
			cancel()
			if err == nil {
				b.logf("Removed sandbox %s after earlier failures", s.name)
				s.forget()
				return
			}
		}
	}()
}
