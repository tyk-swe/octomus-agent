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

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
)

// Remote runs every untrusted child in a container through the sandbox broker. It never falls back to the host: when
// the broker is unreachable, starting a child fails.
type Remote struct {
	socket string
	mu     sync.Mutex
	info   BrokerInfo
	infoAt time.Time
	slots  chan struct{}
}

const infoTTL = 5 * time.Second

func NewRemote(socket string) *Remote { return &Remote{socket: socket} }

func (r *Remote) Mode() Mode { return ModeDocker }

func (r *Remote) Socket() string { return r.socket }

func (r *Remote) dial(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", r.socket)
}

// Info reports the broker's posture, refreshed at most every few seconds.
func (r *Remote) Info(ctx context.Context) (BrokerInfo, error) {
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
		return BrokerInfo{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return BrokerInfo{}, r.unavailable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return BrokerInfo{}, brokerError(resp)
	}
	var info BrokerInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return BrokerInfo{}, fmt.Errorf("Sandbox broker answered unreadable info: %w", err)
	}
	if info.Limits.Max < 1 {
		return BrokerInfo{}, errors.New("Sandbox broker reported no sandbox capacity")
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

func runnerName(backend config.Backend) string {
	if backend == config.BackendOpencode {
		return "opencode"
	}
	return "codex"
}

func (r *Remote) request(spec Spec) Request {
	req := Request{Kind: spec.Kind.String(), Dir: spec.Dir, Env: spec.Env, Stdin: spec.Stdin, FreshHome: spec.FreshHome, Timeout: spec.Timeout}
	switch spec.Kind {
	case KindRunner:
		req.Runner, req.Mode = runnerName(spec.Runner), RunnerModeStdio
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

func (r *Remote) start(ctx context.Context, spec Spec, req Request) (*remoteChild, error) {
	if ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	if _, err := r.Info(ctx); err != nil {
		return nil, err
	}
	if spec.Kind != KindProbe {
		if err := PrepareRoot(spec); err != nil {
			return nil, fmt.Errorf("Preparing the sandbox root: %w", err)
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
		return nil, err
	}
	return newRemoteChild(conn, reader, spec.Stderr, req.Stdin, release), nil
}

// open asks the broker for a sandbox and returns the upgraded stream that is its lifeline.
func (r *Remote) open(ctx context.Context, req Request) (net.Conn, *bufio.Reader, error) {
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
	httpReq.Header.Set("Upgrade", UpgradeProtocol)
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
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, nil
}

func (r *Remote) StartOpenCode(ctx context.Context, spec Spec, readinessSeconds uint64) (*OpenCodeServer, error) {
	spec.Stdin = true
	req := r.request(spec)
	req.Mode, req.Readiness = RunnerModeOpenCode, readinessSeconds
	child, err := r.start(ctx, spec, req)
	if err != nil {
		return nil, err
	}
	stdout := child.Stdout()
	_, err = process.Bounded(ctx, readinessSeconds+10, "OpenCode startup timed out", func(wctx context.Context) (struct{}, error) {
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
		_, _ = child.Wait()
		return nil, err
	}
	return &OpenCodeServer{Base: "http://opencode.sandbox", Transport: streamTransport(child), Child: child, Drained: child.done}, nil
}

const (
	handshakeReady  = "OCTOMUS-READY"
	handshakeFailed = "OCTOMUS-FAILED "
	handshakeLimit  = 4096
)

// readHandshake reads the shim's one-line verdict byte by byte, so nothing of the HTTP/2 stream behind it is consumed.
func readHandshake(r io.Reader) error {
	var line []byte
	var one [1]byte
	for len(line) < handshakeLimit {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return errors.New("OpenCode exited before server readiness")
		}
		if one[0] == '\n' {
			text := string(line)
			if text == handshakeReady {
				return nil
			}
			if message, ok := strings.CutPrefix(text, handshakeFailed); ok {
				return errors.New(strings.ToValidUTF8(message, "�"))
			}
			return errors.New("OpenCode sandbox answered an unexpected handshake")
		}
		line = append(line, one[0])
	}
	return errors.New("OpenCode sandbox handshake exceeded its size limit")
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
			return &streamConn{r: child.Stdout(), w: child.stdin}, nil
		},
	}
}

func (r *Remote) RunnerVersion(ctx context.Context, spec Spec, _ uint64) (string, error) {
	info, err := r.Info(ctx)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(info.Runners[runnerName(spec.Runner)])
	if version == "" {
		return "", fmt.Errorf("%s is not installed in the sandbox image %s", spec.Runner.Display(), info.Image)
	}
	return version, nil
}

// remoteChild is one sandbox seen through its broker stream.
type remoteChild struct {
	conn    net.Conn
	out     *FrameWriter
	stdin   *remoteStdin
	stdout  *io.PipeReader
	stderr  *io.PipeReader
	done    chan struct{}
	report  ExitReport
	lost    error
	killed  atomic.Bool
	release func()
}

func newRemoteChild(conn net.Conn, reader *bufio.Reader, sink io.Writer, stdin bool, release func()) *remoteChild {
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	c := &remoteChild{conn: conn, out: NewFrameWriter(conn), stdout: stdoutR, stderr: stderrR,
		done: make(chan struct{}), release: release}
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
			kind, payload, err := ReadFrame(reader)
			if err != nil {
				c.lost = err
				return
			}
			switch kind {
			case FrameStdout:
				if !outBroken {
					_, werr := stdoutW.Write(payload)
					outBroken = werr != nil
				}
			case FrameStderr:
				if !errBroken {
					_, werr := errSink.Write(payload)
					errBroken = werr != nil
				}
			case FrameExit:
				if err := json.Unmarshal(payload, &c.report); err != nil {
					c.lost = err
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

// Wait reports how the sandbox ended. A stream lost without an exit report is a kill when Octomus cut it, and an
// error otherwise.
func (c *remoteChild) Wait() (process.Status, error) {
	<-c.done
	if c.lost != nil {
		if c.killed.Load() {
			return process.ExitStatus(process.Exit{Killed: true}), nil
		}
		return process.Status{}, fmt.Errorf("Sandbox stream was lost: %w", c.lost)
	}
	if c.report.Error != "" && !c.report.Killed {
		return process.Status{}, errors.New(c.report.Error)
	}
	return process.ExitStatus(process.Exit{Code: c.report.Code, OOM: c.report.OOM, Killed: c.report.Killed, Reason: c.report.Error}), nil
}

func (c *remoteChild) signal(name string) {
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = c.out.Frame(FrameSignal, []byte(name))
}

func (c *remoteChild) Terminate() { c.signal(SignalTerminate) }

// killReportWait is how long a kill waits for the broker's exit report, which carries the sandbox's evidence.
const killReportWait = 10 * time.Second

// Kill stops the sandbox and waits briefly for the broker's report of it. Cutting the stream afterwards holds even
// when the kill frame cannot be written: the broker kills and removes a container whose stream closes.
func (c *remoteChild) Kill() {
	if c.killed.Swap(true) {
		return
	}
	select {
	case <-c.done:
	default:
		c.signal(SignalKill)
		timer := time.NewTimer(killReportWait)
		select {
		case <-c.done:
		case <-timer.C:
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
				if _, err := child.out.Data(FrameStdin, data); err != nil {
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
		err = s.child.out.Frame(FrameStdinEOF, nil)
	})
	return err
}

func (s *remoteStdin) stop() {
	s.once.Do(func() { close(s.closed) })
}

// streamConn presents a sandbox's standard streams as the single connection an HTTP/2 client needs.
type streamConn struct {
	r io.Reader
	w *remoteStdin
}

func (c *streamConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *streamConn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n, err := c.w.Write(p[written:])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (c *streamConn) Close() error                     { return nil }
func (c *streamConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "control", Net: "unix"} }
func (c *streamConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "sandbox", Net: "unix"} }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }
