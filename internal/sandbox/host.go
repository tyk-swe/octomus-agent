package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// Host runs untrusted children directly on this host with the service user's permissions. It isolates nothing; it
// exists for the explicit --sandbox off mode on a dedicated VM and for tests.
type Host struct{}

func (Host) Mode() Mode { return ModeOff }

const stderrWaitDelay = 2 * time.Second

func (Host) Start(ctx context.Context, spec Spec) (Child, error) {
	if ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	binary, args := spec.Binary, wire.RunnerArgs(runnerName(spec.Runner))
	switch spec.Kind {
	case KindRunner:
		if args == nil {
			return nil, errors.New("Invalid backend")
		}
	case KindVerify:
		binary, args = wire.VerifyProgram(spec.Command)
	default:
		return nil, fmt.Errorf("The host backend cannot run %s sandboxes", spec.Kind)
	}
	started, err := process.StartHost(binary, args, spec.Dir, spec.Env, spec.Stdin)
	if err != nil {
		if spec.Kind == KindVerify {
			return nil, fmt.Errorf("Could not start %s: %w", binary, err)
		}
		return nil, &StartError{err}
	}
	child := &hostChild{HostChild: started, copied: make(chan struct{})}
	if spec.Stderr == nil {
		close(child.copied)
		return child, nil
	}
	go func() {
		defer close(child.copied)
		defer started.Stderr().Close()
		_, _ = io.Copy(spec.Stderr, started.Stderr())
	}()
	return child, nil
}

type hostChild struct {
	*process.HostChild
	copied chan struct{}
}

func (h *hostChild) Stdin() DeadlineWriter {
	if f := h.HostChild.Stdin(); f != nil {
		return f
	}
	return nil
}

// Wait also bounds how long an escaped grandchild may hold a copied stderr open, as exec's WaitDelay does.
func (h *hostChild) Wait() (process.Status, error) {
	status, err := h.HostChild.Wait()
	select {
	case <-h.copied:
	case <-time.After(stderrWaitDelay):
		h.HostChild.Stderr().Close()
	}
	return status, err
}

func (h Host) StartOpenCode(ctx context.Context, spec Spec, readinessSeconds uint64) (*OpenCodeServer, error) {
	child, err := h.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	stdout := child.Stdout()
	lines, ready := watchReadyLine(stdout)
	base, err := process.Bounded(ctx, readinessSeconds, openCodeStartupTimeout, func(wctx context.Context) (string, error) {
		select {
		case result := <-ready:
			return result.base, result.err
		case <-wctx.Done():
			return "", wctx.Err()
		}
	})
	if err != nil {
		child.Kill()
		stdout.Close()
		_, _ = child.Wait()
		return nil, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, lines)
	}()
	return &OpenCodeServer{Base: base, Transport: LoopbackTransport(), Child: child, Drained: drained}, nil
}

func (Host) RunnerVersion(ctx context.Context, spec Spec, seconds uint64) (string, error) {
	return process.RunMachine(ctx, spec.Binary, []string{"--version"}, spec.Dir, seconds)
}
