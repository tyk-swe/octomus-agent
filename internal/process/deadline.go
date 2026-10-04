package process

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrDeadlineElapsed = errors.New("deadline has elapsed")

var ErrCancelled = errors.New("Operation cancelled")

var ErrSessionCancelled = errors.New("Session cancelled")

const deadlineGrace = 8 * time.Second

type Deadline[T any] struct {
	Output           T
	Expired          bool
	AlreadyCancelled bool
}

func WithDeadline[T any](ctx context.Context, cancel context.CancelFunc, limit time.Duration, fn func() T) Deadline[T] {
	done := make(chan T, 1)
	go func() { done <- fn() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case out := <-done:
		return Deadline[T]{Output: out}
	case <-timer.C:
	}
	alreadyCancelled := ctx.Err() != nil
	cancel()
	grace := time.NewTimer(deadlineGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
	}
	return Deadline[T]{Expired: true, AlreadyCancelled: alreadyCancelled}
}

// Bounded runs fn until deadline or ctx ends, whichever is first, then cancels fn's context and gives it a grace
// period to return before reporting what ended it.
func Bounded[T any](ctx context.Context, deadline time.Time, what string, fn func(context.Context) (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		v, err := fn(workCtx)
		done <- result{v, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var err error
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
		err = fmt.Errorf("%s: %w", what, ErrDeadlineElapsed)
	case <-ctx.Done():
		err = ErrSessionCancelled
	}
	cancel()
	grace := time.NewTimer(deadlineGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
	}
	var zero T
	return zero, err
}
