package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// prepared is a created container with its attach stream already registered, not yet started.
type prepared struct {
	b      *Broker
	id     string
	name   string
	image  string
	lease  string
	attach *engineapi.Attached
	// release gives the sandbox's admission slot back, once its removal is confirmed.
	release func()
	once    sync.Once
	removed error
}

const (
	// createTimeout matches how long the control plane waits for a sandbox to open.
	createTimeout = 2 * time.Minute
	attachTimeout = time.Minute
)

// prepare creates a sandbox's container and attaches to it. ctx is the request's; the create alone runs under base,
// the broker's lifetime, because a client that gives up mid-create must not leave behind a container the daemon
// still finishes: once created, it has an ID and is removed like any other. prepare owns release from here on.
func (b *Broker) prepare(ctx, base context.Context, p plan, release func()) (*prepared, error) {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := fmt.Sprintf("octomus-%s-%s-%s", b.cfg.Instance, p.kind, hex.EncodeToString(suffix[:]))
	var extraEnv []string
	lease := ""
	if b.leases != nil && (p.kind != wire.KindProbe || p.probe == wire.ProbeContainment) {
		// The containment probe proves what the gateway refuses a runner sandbox, so it holds a runner's lease.
		kind := p.kind
		if kind == wire.KindProbe {
			kind = wire.KindRunner
		}
		token, err := b.leases.Grant(name, kind)
		if err != nil {
			release()
			return nil, fmt.Errorf("Granting the sandbox egress lease: %w", err)
		}
		lease, extraEnv = token, egress.ProxyEnv(b.cfg.EgressProxy, token)
	}
	spec := b.cfg.container(p, extraEnv)
	// A sandbox runs the image ID its tag resolved to, so its evidence names exactly what ran.
	spec.Image = p.image
	if spec.Image == "" {
		spec.Image = b.cfg.Image
	}
	createCtx, cancel := context.WithTimeout(base, createTimeout)
	id, warnings, err := b.engine.ContainerCreate(createCtx, name, spec)
	cancel()
	if err != nil {
		if lease != "" {
			b.leases.Revoke(lease)
		}
		// A create cut short can still finish in the daemon; removing by name finds the container if it did.
		removeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := b.engine.ContainerRemove(removeCtx, name); err != nil {
			b.logf("Removing sandbox %s after its create failed did not succeed; the next sweep removes it: %v", name, err)
		}
		cancel()
		release()
		return nil, fmt.Errorf("Creating the sandbox: %w", err)
	}
	b.mu.Lock()
	b.live[id] = p.rel
	b.mu.Unlock()
	s := &prepared{b: b, id: id, name: name, image: spec.Image, lease: lease, release: release}
	if len(warnings) > 0 {
		// Docker drops a limit the host cannot enforce and only warns; every limit in the spec is part of the boundary.
		s.discard()
		return nil, fmt.Errorf("Docker Engine would not enforce the sandbox spec: %s", strings.Join(warnings, " "))
	}
	if err := ctx.Err(); err != nil {
		s.discard()
		return nil, fmt.Errorf("Creating the sandbox: %w", err)
	}
	attachCtx, cancel := context.WithTimeout(ctx, attachTimeout)
	attach, err := b.engine.ContainerAttach(attachCtx, id, p.stdin)
	cancel()
	if err != nil {
		s.discard()
		return nil, fmt.Errorf("Attaching to the sandbox: %w", err)
	}
	s.attach = attach
	return s, nil
}

type control struct {
	stdin  []byte
	eof    bool
	signal string
}

// output is where a sandbox's stdout and stderr go. interrupt, when set, unblocks a sink stuck writing to a client
// that stopped reading; the sinks fail from then on.
type output struct {
	stdout, stderr func([]byte) error
	interrupt      func()
}

const streamClosed = "Sandbox stream closed"

// ending is how a sandbox's run ended.
type ending struct {
	// result is the exit the daemon reported, if it did.
	result *engineapi.WaitResult
	// killed is set when a SIGKILL the daemon delivered, or the removal by force, stopped the sandbox.
	killed bool
	reason string
	// cut is set when the control stream closed, leaving nobody to report to.
	cut bool
	// err is a failure before the sandbox could report an exit.
	err error
	// deadline bounds the teardown.
	deadline time.Time
}

// execute starts a prepared container and pumps it until it exits, the time limit passes or the control source
// ends, then tears it down within teardownBudget. It always removes the container, or leaves it to a reaper; a
// report with an Error and no kill means the broker cannot vouch for how the sandbox ended.
func (s *prepared) execute(ctx context.Context, timeout time.Duration, out output, controls <-chan control) (wire.ExitReport, error) {
	b := s.b
	input := make(chan control)
	stopInput, inputDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(inputDone)
		for {
			select {
			case <-stopInput:
				return
			case msg := <-input:
				if msg.eof {
					_ = s.attach.CloseStdin()
				} else if _, err := s.attach.Conn.Write(msg.stdin); err != nil {
					_ = s.attach.CloseStdin()
				}
			}
		}
	}()
	attached := make(chan error, 1)
	go func() {
		attached <- engineapi.Demux(s.attach.Reader, out.stdout, out.stderr)
	}()
	end := s.run(ctx, timeout, controls, input)
	if end.deadline.IsZero() {
		end.deadline = time.Now().Add(teardownBudget)
	}
	// within bounds one teardown step by what the budget leaves once the removal's share is kept back.
	within := func(step time.Duration) time.Duration {
		return max(0, min(step, time.Until(end.deadline)-removeReserve))
	}
	close(stopInput)
	// Draining closes the attach stream, which also ends a blocked stdin write.
	truncated, outErr := s.drain(end, attached, out, within)
	<-inputDone
	var report wire.ExitReport
	var failure error
	switch {
	case end.cut:
		report = wire.ExitReport{Killed: true, Error: streamClosed}
	case end.err != nil:
		failure = end.err
	default:
		report = wire.ExitReport{Killed: end.killed, Error: end.reason}
		oom := false
		if end.result != nil {
			report.Code = end.result.StatusCode
			oom = s.oomKilled(within(5 * time.Second))
		}
		// Docker marks a container OOM-killed when the kernel killed any process in it. The evidence keeps that; the
		// status says the memory limit ended the command only when the command failed.
		report.OOM = oom && report.Code != 0
		report.Sandbox = b.evidence(s.name, s.image, oom)
		switch {
		case end.killed:
		case truncated:
			report.Error = "Sandbox output was cut short after the sandbox ended"
		case outErr != nil:
			report.Error = "Sandbox output failed: " + outErr.Error()
		}
	}
	removeCtx, cancel := context.WithDeadline(context.Background(), end.deadline)
	removed := s.remove(removeCtx)
	cancel()
	switch {
	case removed == nil || end.cut:
	case failure != nil:
		failure = fmt.Errorf("%w; removing the sandbox also failed: %v", failure, removed)
	default:
		report.Killed, report.Error = false, "Removing the sandbox failed: "+removed.Error()
	}
	return report, failure
}

// run starts the container and serves its controls until it ends. Only a SIGKILL the daemon delivered marks it
// killed, and the first one names the reason: a kill that finds it already exited leaves its own exit status.
func (s *prepared) run(ctx context.Context, timeout time.Duration, controls <-chan control, input chan<- control) (end ending) {
	b := s.b
	if err := b.engine.ContainerStart(ctx, s.id); err != nil {
		end.err = fmt.Errorf("Starting the sandbox: %w", err)
		return end
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	// Waiting for not-running after start also observes an exit that happened before the wait request arrived.
	results, errs := b.engine.ContainerWait(waitCtx, s.id)
	limit := time.NewTimer(timeout)
	defer limit.Stop()
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	var stopped <-chan time.Time
	asked := ""
	kill := func(reason string) {
		if end.deadline.IsZero() {
			end.deadline, stopped, asked = time.Now().Add(teardownBudget), time.After(stopWait), reason
		}
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := b.engine.ContainerKill(killCtx, s.id, "SIGKILL")
		cancel()
		switch {
		case err == nil:
			if !end.killed {
				end.killed, end.reason = true, reason
			}
		case engineapi.IsConflict(err) || engineapi.IsNotFound(err):
			// It is no longer running: it ended on its own, and the wait reports how.
		default:
			b.logf("Killing sandbox %s failed; retrying: %v", s.name, err)
			retry.Reset(time.Second)
		}
	}
	var pendingInput []control
	pendingBytes := 0
	for {
		var sendInput chan<- control
		var nextInput control
		if len(pendingInput) > 0 {
			sendInput, nextInput = input, pendingInput[0]
		}
		select {
		case sendInput <- nextInput:
			pendingBytes -= len(nextInput.stdin)
			pendingInput[0] = control{}
			pendingInput = pendingInput[1:]
		case result := <-results:
			end.result = &result
			return end
		case err := <-errs:
			end.err = fmt.Errorf("Waiting for the sandbox: %w", err)
			return end
		case <-limit.C:
			kill("Sandbox time limit reached")
		case <-retry.C:
			kill(asked)
		case <-stopped:
			b.logf("Sandbox %s did not report its exit after a kill; removing it by force", s.name)
			if !end.killed {
				end.killed, end.reason = true, asked
			}
			return end
		case <-ctx.Done():
			end.cut = true
			return end
		case msg, ok := <-controls:
			switch {
			case !ok:
				end.cut = true
				return end
			case msg.stdin != nil || msg.eof:
				// Keep input ordered and bounded without delaying cancellation, signals or the time limit.
				if len(pendingInput) >= 1024 || pendingBytes+len(msg.stdin) > 32<<20 {
					end.err = errors.New("Sandbox stdin backlog exceeded")
					return end
				}
				pendingInput = append(pendingInput, msg)
				pendingBytes += len(msg.stdin)
			case msg.signal == wire.SignalTerminate:
				termCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = b.engine.ContainerKill(termCtx, s.id, "SIGTERM")
				cancel()
			case msg.signal == wire.SignalKill:
				kill("")
			}
		}
	}
}

// drain joins the output pump. A sandbox that exited has drainWait for the rest of its output; then, or at once when
// it did not exit, its attach stream is closed. A sink still blocked on a client that stopped reading is interrupted,
// so no output can follow the exit report. It reports whether output was cut off, else the pump's own error.
func (s *prepared) drain(end ending, attached <-chan error, out output, within func(time.Duration) time.Duration) (bool, error) {
	join := func(wait time.Duration) (bool, error) {
		select {
		case err := <-attached:
			return true, err
		default:
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case err := <-attached:
			return true, err
		case <-timer.C:
			return false, nil
		}
	}
	if end.result != nil {
		if done, err := join(within(drainWait)); done {
			s.attach.Close()
			return false, err
		}
	}
	s.attach.Close()
	if done, _ := join(within(joinWait)); !done {
		if out.interrupt != nil {
			out.interrupt()
		}
		<-attached
	}
	return true, nil
}

// oomKilled reports whether the kernel killed any process in the sandbox for memory.
func (s *prepared) oomKilled(wait time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	state, err := s.b.engine.ContainerInspect(ctx, s.id)
	if err != nil {
		s.b.logf("Reading the state of sandbox %s failed; a memory-limit kill would go unrecorded: %v", s.name, err)
		return false
	}
	return state.State.OOMKilled
}

// evidence records what one finished sandbox ran and, when the gateway is configured, where it reached out.
func (b *Broker) evidence(name, image string, oom bool) *model.SandboxRecord {
	record := &model.SandboxRecord{ImageID: image, Runtime: b.cfg.Runtime, Runs: 1, OOM: oom,
		Egress: model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{}}}
	if b.cfg.EgressCollector == "" {
		return record
	}
	// The gateway counts accepted tunnels immediately, even when upstream connections are still closing.
	summary, err := egress.FetchSummary(context.Background(), b.cfg.EgressCollector, name)
	if err != nil {
		b.logf("Reading the egress record of sandbox %s failed; its record shows no connections: %v", name, err)
		return record
	}
	for host, count := range summary.Allowed {
		record.Egress.Allowed[host] = uint64(count.Count)
	}
	for host, count := range summary.Denied {
		record.Egress.Denied[host] = uint64(count.Count)
	}
	record.Egress.Failed = map[string]uint64{}
	for host, count := range summary.Failed {
		record.Egress.Failed[host] = uint64(count.Count)
	}
	return model.MergeSandbox(nil, record)
}

// runSandbox runs a plan to completion without a control-plane stream, for the broker's own probes.
func (b *Broker) runSandbox(ctx context.Context, p plan, stdout, stderr func([]byte) error, controls <-chan control) (wire.ExitReport, error) {
	prepared, err := b.prepare(ctx, ctx, p, func() {})
	if err != nil {
		return wire.ExitReport{}, err
	}
	if controls == nil {
		controls = make(chan control)
	}
	return prepared.execute(ctx, p.timeout, output{stdout: stdout, stderr: stderr}, controls)
}
