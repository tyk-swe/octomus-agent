package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// Remote runs every untrusted child in a container through the sandbox broker. It never falls back to the host: when
// the broker is unreachable, starting a child fails.
type Remote struct {
	socket string
	mu     sync.Mutex
	info   wire.BrokerInfo
	infoAt time.Time
	slots  chan struct{}
	// killWait is how long a kill waits for the broker's exit report: killReportWait outside tests.
	killWait time.Duration
}

const infoTTL = 5 * time.Second

func NewRemote(socket string) *Remote { return &Remote{socket: socket, killWait: killReportWait} }

func (r *Remote) Mode() Mode { return ModeDocker }

func (r *Remote) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", r.socket)
}

// Info reports the broker's posture, refreshed at most every few seconds.
func (r *Remote) Info(ctx context.Context) (wire.BrokerInfo, error) {
	r.mu.Lock()
	if !r.infoAt.IsZero() && time.Since(r.infoAt) < infoTTL {
		info := r.info
		r.mu.Unlock()
		return info, nil
	}
	r.mu.Unlock()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return r.dial(ctx) },
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sandboxd/v1/info", nil)
	if err != nil {
		return wire.BrokerInfo{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return wire.BrokerInfo{}, r.unavailable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return wire.BrokerInfo{}, brokerError(resp)
	}
	var info wire.BrokerInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return wire.BrokerInfo{}, fmt.Errorf("Sandbox broker answered unreadable info: %w", err)
	}
	if info.Limits.Max < 1 {
		return wire.BrokerInfo{}, errors.New("Sandbox broker reported no sandbox capacity")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.info, r.infoAt = info, time.Now()
	if r.slots == nil || cap(r.slots) != info.Limits.Max {
		r.slots = make(chan struct{}, info.Limits.Max)
	}
	return info, nil
}

// Healthy reports whether the broker answers, so work is refused rather than started without isolation.
func (r *Remote) Healthy(ctx context.Context) error {
	_, err := r.Info(ctx)
	return err
}

// unavailable names the broker socket and the root cause, without the request plumbing around it.
func (r *Remote) unavailable(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		err = op.Err
		var syscallErr *os.SyscallError
		if errors.As(err, &syscallErr) {
			err = syscallErr.Err
		}
	}
	return fmt.Errorf("Sandbox broker is unavailable at %s: %w", r.socket, err)
}

func brokerError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var doc struct {
		Error string `json:"error"`
	}
	message := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &doc) == nil && doc.Error != "" {
		message = doc.Error
	}
	return fmt.Errorf("Sandbox broker refused the request (HTTP %d): %s", resp.StatusCode, message)
}

func (r *Remote) request(spec Spec) wire.Request {
	req := wire.Request{Kind: spec.Kind.String(), Dir: spec.Dir, Env: spec.Env, Stdin: spec.Stdin, FreshHome: spec.FreshHome, Timeout: spec.Timeout}
	switch spec.Kind {
	case KindRunner:
		req.Runner, req.Mode = runnerName(spec.Runner), wire.RunnerModeStdio
	case KindVerify:
		req.Command = spec.Command
	case KindProbe:
		req.Mode, req.Dir = spec.Probe, ""
	}
	return req
}

func (r *Remote) Start(ctx context.Context, spec Spec) (Child, error) {
	return r.start(ctx, spec, r.request(spec))
}

func (r *Remote) start(ctx context.Context, spec Spec, req wire.Request) (*remoteChild, error) {
	if ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	if _, err := r.Info(ctx); err != nil {
		return nil, notStarted(ctx, err)
	}
	if spec.Kind != KindProbe {
		if err := PrepareRoot(spec); err != nil {
			return nil, &SandboxError{fmt.Errorf("Preparing the sandbox root: %w", err)}
		}
	}
	r.mu.Lock()
	slots := r.slots
	r.mu.Unlock()
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		return nil, process.ErrSessionCancelled
	}
	release := sync.OnceFunc(func() { <-slots })
	conn, reader, err := r.open(ctx, req)
	if err != nil {
		release()
		return nil, notStarted(ctx, err)
	}
	// The broker resolves its image tag for each sandbox, so this one may have moved it to an image rebuilt since the
	// cached info: the next Info asks again.
	r.mu.Lock()
	r.infoAt = time.Time{}
	r.mu.Unlock()
	return newRemoteChild(conn, reader, spec.Stderr, req.Stdin, r.killWait, release), nil
}

// notStarted reports a sandbox the broker did not provide as the sandbox's failure, unless the caller cancelled.
func notStarted(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, process.ErrSessionCancelled) {
		return process.ErrSessionCancelled
	}
	return &SandboxError{err}
}

// open asks the broker for a sandbox and returns the upgraded stream that is its lifeline.
func (r *Remote) open(ctx context.Context, req wire.Request) (net.Conn, *bufio.Reader, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	conn, err := r.dial(ctx)
	if err != nil {
		return nil, nil, r.unavailable(err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, "http://sandboxd/v1/sandboxes", bytes.NewReader(body))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Connection", "Upgrade")
	httpReq.Header.Set("Upgrade", wire.UpgradeProtocol)
	// Creating a container can take a while on a busy host; the stream itself has no deadline once it is up.
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := httpReq.Write(conn); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("Sandbox request failed: %w", err)
	}
	reader := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(reader, httpReq)
	if err != nil {
		conn.Close()
		if ctx.Err() != nil {
			return nil, nil, process.ErrSessionCancelled
		}
		return nil, nil, fmt.Errorf("Sandbox request failed: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		return nil, nil, brokerError(resp)
	}
	if !stop() {
		// A cancellation as the stream came up has closed it; the broker removes a sandbox whose stream closes.
		conn.Close()
		return nil, nil, process.ErrSessionCancelled
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, nil
}

func (r *Remote) StartOpenCode(ctx context.Context, spec Spec, readinessSeconds uint64) (*OpenCodeServer, error) {
	spec.Stdin = true
	req := r.request(spec)
	req.Mode, req.Readiness = wire.RunnerModeOpenCode, readinessSeconds
	child, err := r.start(ctx, spec, req)
	if err != nil {
		return nil, err
	}
	stdout := child.Stdout()
	_, err = process.Bounded(ctx, readinessSeconds+10, openCodeStartupTimeout, func(wctx context.Context) (struct{}, error) {
		stop := context.AfterFunc(wctx, func() {
			stdout.Close()
			child.Kill()
		})
		defer stop()
		return struct{}{}, readHandshake(stdout)
	})
	if err != nil {
		stdout.Close()
		child.Kill()
		if _, werr := child.Wait(); werr != nil {
			// The sandbox's own failure, such as a container that never started, is why no handshake came.
			err = fmt.Errorf("%w: %w", err, werr)
		}
		return nil, err
	}
	return &OpenCodeServer{Base: "http://opencode.sandbox", Transport: streamTransport(child), Child: child, Drained: child.done}, nil
}

// streamTransport speaks HTTP/2 without TLS over the sandbox's standard streams. It dials exactly once: the stream is
// the only way into the sandbox, and a lost stream is a lost server.
func streamTransport(child *remoteChild) http.RoundTripper {
	var used atomic.Bool
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{
		Proxy:     nil,
		Protocols: protocols,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if used.Swap(true) {
				return nil, errors.New("OpenCode sandbox stream is closed")
			}
			return &pipeConn{r: child.Stdout(), w: child.stdin, local: "control", remote: "sandbox"}, nil
		},
	}
}

func (r *Remote) RunnerVersion(ctx context.Context, spec Spec, _ uint64) (string, error) {
	info, err := r.Info(ctx)
	if err != nil {
		return "", err
	}
	name := runnerName(spec.Runner)
	version := strings.TrimSpace(info.Runners[name])
	switch {
	case version != "":
		return version, nil
	case info.RunnerErrors[name] != "":
		return "", fmt.Errorf("%s --version failed in the sandbox image %s: %s", spec.Runner.Display(), info.Image, info.RunnerErrors[name])
	}
	return "", fmt.Errorf("%s is not installed in the sandbox image %s", spec.Runner.Display(), info.Image)
}

// remoteChild is one sandbox seen through its broker stream.
type remoteChild struct {
	conn   net.Conn
	out    *wire.FrameWriter
	stdin  *remoteStdin
	stdout *io.PipeReader
	stderr *io.PipeReader
	done   chan struct{}
	report wire.ExitReport
	// lost is why the stream ended without a readable exit report, decided when it ended.
	lost   error
	killed atomic.Bool
	// cut is set when a kill closed the stream because its wait for the broker's report ran out.
	cut     atomic.Bool
	wait    time.Duration
	release func()
}

func newRemoteChild(conn net.Conn, reader *bufio.Reader, sink io.Writer, stdin bool, wait time.Duration, release func()) *remoteChild {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	c := &remoteChild{conn: conn, out: wire.NewFrameWriter(conn), stdout: stdoutR, stderr: stderrR,
		done: make(chan struct{}), wait: wait, release: release}
	if stdin {
		c.stdin = newRemoteStdin(c)
	}
	var errSink io.Writer = stderrW
	if sink != nil {
		errSink = sink
		stderrW.Close()
	}
	go func() {
		defer close(c.done)
		defer c.release()
		defer stdoutW.Close()
		defer stderrW.Close()
		outBroken, errBroken := false, false
		for {
			kind, payload, err := wire.ReadFrame(reader)
			if err != nil {
				c.lose(err)
				return
			}
			switch kind {
			case wire.FrameStdout:
				if !outBroken {
					_, werr := stdoutW.Write(payload)
					outBroken = werr != nil
				}
			case wire.FrameStderr:
				if !errBroken {
					_, werr := errSink.Write(payload)
					errBroken = werr != nil
				}
			case wire.FrameExit:
				if err := json.Unmarshal(payload, &c.report); err != nil {
					c.lose(err)
				}
				return
			}
		}
	}()
	return c
}

func (c *remoteChild) Stdin() DeadlineWriter {
	if c.stdin == nil {
		return nil
	}
	return c.stdin
}

func (c *remoteChild) Stdout() io.ReadCloser { return c.stdout }

// Evidence is the broker's record of this sandbox once it has ended.
func (c *remoteChild) Evidence() *model.SandboxRecord {
	select {
	case <-c.done:
		return c.report.Sandbox
	default:
		return nil
	}
}
func (c *remoteChild) Stderr() io.ReadCloser { return c.stderr }

// lose records why the stream ended without a readable exit report. Only that report confirms the container is gone,
// so a stream Octomus cut after a kill is no more a clean end than one the broker dropped.
func (c *remoteChild) lose(err error) {
	if c.cut.Load() {
		err = fmt.Errorf("Sandbox end is unconfirmed: the broker did not report it within %s of the kill", c.wait)
	} else {
		err = fmt.Errorf("Sandbox stream was lost: %w", err)
	}
	c.lost = &SandboxError{err}
}

// TimeLimitReason is the broker's report of a sandbox it killed at its time limit. Running too long is the program's
// own result, as a timeout is; any other error an exit report carries means the broker failed the sandbox.
const TimeLimitReason = "Sandbox time limit reached"

// Wait reports how the sandbox ended. A lost stream, an unconfirmed kill and every error the broker reports but its
// time limit, such as a sandbox it could not start or remove, are the sandbox's failures, never the program's result,
// even when the broker had killed it.
func (c *remoteChild) Wait() (process.Status, error) {
	<-c.done
	if c.lost != nil {
		return process.Status{}, c.lost
	}
	if c.report.Error != "" && !(c.report.Killed && c.report.Error == TimeLimitReason) {
		return process.Status{}, &SandboxError{errors.New(c.report.Error)}
	}
	return process.ExitStatus(process.Exit{Code: c.report.Code, OOM: c.report.OOM, Killed: c.report.Killed, Reason: c.report.Error}), nil
}

func (c *remoteChild) signal(name string) {
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = c.out.Frame(wire.FrameSignal, []byte(name))
}

func (c *remoteChild) Terminate() { c.signal(wire.SignalTerminate) }

// killReportWait is how long a kill waits for the broker's exit report, which confirms the container is gone and
// carries the sandbox's evidence. The broker bounds its teardown, removal included, well inside it.
const killReportWait = 60 * time.Second

// Kill stops the sandbox and waits for the broker's report of it. Its output is discarded from then on, so a reader
// that stopped cannot hold the report back. Cutting the stream afterwards holds even when the kill frame cannot be
// written: the broker kills and removes a container whose stream closes, but without its report Wait cannot call the
// end clean.
func (c *remoteChild) Kill() {
	if c.killed.Swap(true) {
		return
	}
	select {
	case <-c.done:
	default:
		c.stdout.Close()
		c.stderr.Close()
		c.signal(wire.SignalKill)
		timer := time.NewTimer(c.wait)
		select {
		case <-c.done:
		case <-timer.C:
			c.cut.Store(true)
		}
		timer.Stop()
	}
	c.conn.Close()
	if c.stdin != nil {
		c.stdin.stop()
	}
}

// remoteStdin hands whole writes to one writer goroutine, so a caller's write deadline abandons a write before it
// starts and can never cut a frame in half.
type remoteStdin struct {
	child    *remoteChild
	queue    chan []byte
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	deadline time.Time
	failed   atomic.Pointer[error]
}

func newRemoteStdin(child *remoteChild) *remoteStdin {
	s := &remoteStdin{child: child, queue: make(chan []byte), closed: make(chan struct{})}
	go func() {
		for {
			select {
			case data := <-s.queue:
				if _, err := child.out.Data(wire.FrameStdin, data); err != nil {
					s.failed.Store(&err)
				}
			case <-s.closed:
				return
			}
		}
	}()
	return s
}

func (s *remoteStdin) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.deadline = t
	s.mu.Unlock()
	return nil
}

func (s *remoteStdin) Write(p []byte) (int, error) {
	if err := s.failed.Load(); err != nil {
		return 0, *err
	}
	s.mu.Lock()
	deadline := s.deadline
	s.mu.Unlock()
	var expired <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		expired = timer.C
	}
	data := bytes.Clone(p)
	select {
	case s.queue <- data:
		return len(p), nil
	case <-expired:
		return 0, os.ErrDeadlineExceeded
	case <-s.closed:
		return 0, io.ErrClosedPipe
	case <-s.child.done:
		return 0, io.ErrClosedPipe
	}
}

// Close ends the sandbox's stdin once everything accepted so far has been framed.
func (s *remoteStdin) Close() error {
	var err error
	s.once.Do(func() {
		select {
		case s.queue <- nil:
		case <-s.child.done:
		}
		close(s.closed)
		err = s.child.out.Frame(wire.FrameStdinEOF, nil)
	})
	return err
}

func (s *remoteStdin) stop() {
	s.once.Do(func() { close(s.closed) })
}
