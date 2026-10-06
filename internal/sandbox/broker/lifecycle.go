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
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// prepared is a created container with its attach stream already registered, not yet started.
type prepared struct {
	b     *Broker
	id    string
	name  string
	image string
	lease string
	// granted is when the egress lease was granted: a gateway that started later never saw all of the sandbox.
	granted time.Time
	attach  *attachStream
	// release gives the sandbox's admission slot back, once its removal is confirmed.
	release func()
	once    sync.Once
	removed error
}

const (
	// Each daemon preparation step is bounded separately; the control plane waits under its caller's context.
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
	probeTarget := ""
	if b.leases != nil && p.kind == wire.KindProbe && p.probe == wire.ProbeContainment {
		var err error
		probeTarget, err = egress.FetchProbeTarget(ctx, b.cfg.EgressCollector)
		if err != nil {
			release()
			return nil, fmt.Errorf("Choosing the containment probe's unlisted egress target: %w", err)
		}
	}
	lease, granted := "", time.Time{}
	if b.leases != nil && (p.kind != wire.KindProbe || p.probe == wire.ProbeContainment) {
		// The containment probe proves what the gateway refuses a runner sandbox, so it holds a runner's lease.
		kind := p.kind
		if kind == wire.KindProbe {
			kind = wire.KindRunner
		}
		granted = time.Now()
		token, err := b.leases.Grant(name, kind)
		if err != nil {
			release()
			return nil, fmt.Errorf("Granting the sandbox egress lease: %w", err)
		}
		lease, extraEnv = token, egress.ProxyEnv(b.cfg.EgressProxy, token)
	}
	if probeTarget != "" {
		extraEnv = append(extraEnv, wire.ProbeTargetEnv+"="+probeTarget)
	}
	spec := b.cfg.container(p, extraEnv)
	// A sandbox runs the image ID its tag resolved to, so its evidence names exactly what ran.
	spec.Image = p.image
	if spec.Image == "" {
		spec.Image = b.cfg.Image
	}
	createCtx, cancel := context.WithTimeout(base, createTimeout)
	id, warnings, err := b.engine.containerCreate(createCtx, name, spec)
	cancel()
	if err != nil {
		if lease != "" {
			b.leases.Revoke(lease)
		}
		// A create cut short can still finish in the daemon; removing by name finds the container if it already has.
		removeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := b.engine.containerRemove(removeCtx, name); err != nil {
			b.logf("Removing sandbox %s after its create failed did not succeed; the next sweep removes it: %v", name, err)
		}
		cancel()
		var refused *dockerError
		if !errors.As(err, &refused) {
			// The daemon never answered, so the create may still be running, and it cannot be found until it ends.
			b.removeLate(name)
		}
		release()
		return nil, fmt.Errorf("Creating the sandbox: %w", err)
	}
	b.mu.Lock()
	b.live[id] = struct{}{}
	b.mu.Unlock()
	s := &prepared{b: b, id: id, name: name, image: spec.Image, lease: lease, granted: granted, release: release}
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
	attach, err := b.engine.containerAttach(attachCtx, id, p.stdin)
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

// sigkillStatus is the exit status Docker reports for a container a SIGKILL stopped.
const sigkillStatus = 128 + 9

// ending is how a sandbox's run ended.
type ending struct {
	// result is the exit the daemon reported, if it did.
	result *waitResult
	// killed is set when a SIGKILL the daemon delivered, or the removal by force, stopped the sandbox.
	killed bool
	reason string
	// cut is set when the control stream closed, leaving nobody to report to.
	cut bool
	// err is a failure before the sandbox could report an exit.
	err error
	// started is set once the daemon started the container, so its program may have run.
	started bool
	// deadline bounds the teardown.
	deadline time.Time
}

// execute starts a prepared container and pumps it until it exits, the time limit passes or the control source
// ends, then tears it down within teardownBudget. It always removes the container, or leaves it to a reaper; a
// report with an Error and no kill means the broker cannot vouch for how the sandbox ended. With a failure, the report
// still carries the evidence of a sandbox that had started.
func (s *prepared) execute(ctx context.Context, timeout time.Duration, out output, controls <-chan control) (wire.ExitReport, error) {
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
					_ = s.attach.closeStdin()
				} else if _, err := s.attach.conn.Write(msg.stdin); err != nil {
					_ = s.attach.closeStdin()
				}
			}
		}
	}()
	attached := make(chan error, 1)
	go func() {
		attached <- demux(s.attach.reader, out.stdout, out.stderr)
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
		if end.started {
			// The program ran before the sandbox failed, so what it did is still on record.
			report.Sandbox = s.evidence(s.oomKilled(within(5 * time.Second)))
		}
	default:
		report = wire.ExitReport{Killed: end.killed, Error: end.reason}
		// A sandbox that never reported its exit after a kill was removed by force without its state being read, so
		// nothing says whether the memory limit killed a process in it.
		oom, known := false, false
		if end.result != nil {
			report.Code = end.result.StatusCode
			oom, known = s.oomKilled(within(5 * time.Second))
		}
		// Docker marks a container OOM-killed when the kernel killed any process in it. The evidence keeps that; the
		// status says the memory limit ended the command only when the command failed.
		report.OOM = oom && report.Code != 0
		report.Sandbox = s.evidence(oom, known)
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
// killed, and the first one names the reason: a kill that finds it already exited leaves its own exit status. A kill
// whose answer never came counts as delivered when the sandbox then ends with a SIGKILL's status.
func (s *prepared) run(ctx context.Context, timeout time.Duration, controls <-chan control, input chan<- control) (end ending) {
	b := s.b
	if err := b.engine.containerStart(ctx, s.id); err != nil {
		end.err = fmt.Errorf("Starting the sandbox: %w", err)
		return end
	}
	end.started = true
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	// Waiting for not-running after start also observes an exit that happened before the wait request arrived.
	results, errs := b.engine.containerWait(waitCtx, s.id)
	limit := time.NewTimer(timeout)
	defer limit.Stop()
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	var stopped <-chan time.Time
	asked := ""
	// unanswered is set when a SIGKILL request failed without a refusal: the daemon may have delivered it.
	unanswered := false
	kill := func(reason string) {
		if end.deadline.IsZero() {
			end.deadline, stopped, asked = time.Now().Add(teardownBudget), time.After(stopWait), reason
		}
		killCtx, cancel := context.WithTimeout(context.Background(), killWait)
		err := b.engine.containerKill(killCtx, s.id, "SIGKILL")
		cancel()
		switch {
		case err == nil:
			if !end.killed {
				end.killed, end.reason = true, reason
			}
		case conflict(err) || notFound(err):
			// It is no longer running: it ended on its own, or by an unanswered kill, and the wait reports how.
		default:
			unanswered = true
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
			if result.Error != nil {
				// The daemon could not report the exit; an unknown exit is a failure, never success.
				end.err = fmt.Errorf("Waiting for the sandbox: %s", result.Error.Message)
				return end
			}
			end.result = &result
			// Docker answers a SIGKILL only once the container has stopped, so a busy daemon can deliver one after the
			// request gave up: a SIGKILL's exit status after it was sent is that kill's.
			if unanswered && !end.killed && result.StatusCode == sigkillStatus {
				end.killed, end.reason = true, asked
			}
			return end
		case err := <-errs:
			end.err = fmt.Errorf("Waiting for the sandbox: %w", err)
			return end
		case <-limit.C:
			kill(wire.TimeLimitReason)
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
				_ = b.engine.containerKill(termCtx, s.id, "SIGTERM")
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
			s.attach.close()
			return false, err
		}
	}
	s.attach.close()
	if done, _ := join(within(joinWait)); !done {
		if out.interrupt != nil {
			out.interrupt()
		}
		<-attached
	}
	return true, nil
}

// oomKilled reports whether the kernel killed any process in the sandbox for memory, and whether the daemon said.
func (s *prepared) oomKilled(wait time.Duration) (killed, known bool) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	state, err := s.b.engine.containerInspect(ctx, s.id)
	if err != nil {
		s.b.logf("Reading the state of sandbox %s failed; its record is marked incomplete: %v", s.name, err)
		return false, false
	}
	return state.State.OOMKilled, true
}

// evidence records what one finished sandbox ran and, when it held an egress lease, where it reached out. What the
// broker could not read leaves the record marked incomplete rather than reading as nothing.
func (s *prepared) evidence(oom, oomKnown bool) *model.SandboxRecord {
	b := s.b
	record := &model.SandboxRecord{ImageID: s.image, Runtime: b.cfg.Runtime, Runs: 1, OOM: oom, Incomplete: !oomKnown,
		Egress: model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{}}}
	if s.lease == "" {
		return record
	}
	if b.cfg.EgressCollector == "" {
		b.logf("Reading the egress record of sandbox %s failed; its record is marked incomplete: egress collector is not configured", s.name)
		record.Incomplete = true
		return record
	}
	// The gateway counts accepted tunnels immediately, even when upstream connections are still closing; an
	// unresolved request (for example, DNS still in flight) makes the summary incomplete.
	summary, err := egress.FetchSummary(context.Background(), b.cfg.EgressCollector, s.name)
	if err != nil {
		b.logf("Reading the egress record of sandbox %s failed; its record is marked incomplete: %v", s.name, err)
		record.Incomplete = true
		return record
	}
	return s.egressEvidence(record, summary)
}

// egressEvidence preserves known host counts even when the collector could not resolve every request.
func (s *prepared) egressEvidence(record *model.SandboxRecord, summary egress.Summary) *model.SandboxRecord {
	b := s.b
	if summary.GatewayStarted.IsZero() || summary.GatewayStarted.After(s.granted) {
		// A gateway that started after the lease lost whatever the sandbox did before then.
		b.logf("The egress gateway restarted while sandbox %s ran; its record is marked incomplete", s.name)
		record.Incomplete = true
	}
	if summary.Incomplete {
		b.logf("The egress record of sandbox %s has unresolved requests; its record is marked incomplete", s.name)
		record.Incomplete = true
	}
	record.Egress.Allowed = hostCounts(summary.Allowed)
	record.Egress.Denied = hostCounts(summary.Denied)
	record.Egress.Failed = hostCounts(summary.Failed)
	return model.MergeSandbox(nil, record)
}

// hostCounts is the record's view of the collector's per-host request counts.
func hostCounts(counts map[string]egress.HostCount) map[string]uint64 {
	out := make(map[string]uint64, len(counts))
	for host, count := range counts {
		out[host] = uint64(count.Count)
	}
	return out
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
