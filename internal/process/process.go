// Package process owns subprocess execution: every command runs as a process
// group leader so timeouts, cancellation and owner cleanup terminate the whole
// group, never just the direct child. It is the port of src/process.rs.
//
// Diagnostics and machine output are separate contracts: diagnostic captures
// keep a bounded preview for humans while machine captures fail closed on
// truncation or invalid UTF-8.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

// TokenEnv is the operator-token variable removed from every child environment;
// the reference names it api::TOKEN_ENV.
const TokenEnv = "OCTOMUS_TOKEN"

// Command builds an owned command: a new process group (pgid = child pid), the
// service's secret environment removed, Git prompting disabled, and stdin on the
// null device. Callers override Stdin/Stdout/Stderr before Start as needed.
func Command(binary string, cwd string) *exec.Cmd {
	cmd := exec.Command(binary)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case TokenEnv, store.WebhookEnv, "GIT_TERMINAL_PROMPT":
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// GroupChild owns a started process and its recorded process group. Close kills
// the group first and then the leader; the stored group id stays meaningful even
// after Wait reaps the leader, so cleanup also terminates background descendants
// after normal completion.
type GroupChild struct {
	Cmd  *exec.Cmd
	pgid int
	once sync.Once
}

// NewGroupChild takes ownership of a started command built by Command.
func NewGroupChild(cmd *exec.Cmd) *GroupChild {
	return &GroupChild{Cmd: cmd, pgid: cmd.Process.Pid}
}

// Close terminates the recorded process group and the direct child. It is safe
// to call more than once and after the leader has already exited.
func (g *GroupChild) Close() {
	g.once.Do(func() {
		// kill(-pgid) signals the whole group; the worst outcome is ESRCH.
		_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
		_ = g.Cmd.Process.Kill()
	})
}

const (
	// DiagnosticLimit bounds kept stdout/stderr for human-readable evidence.
	DiagnosticLimit = 262_144
	// MachineLimit bounds stdout for machine-readable output. It is distinct
	// from the runner protocol's 16,000,000-byte bound.
	MachineLimit = 16 * 1024 * 1024
)

// CaptureMode selects the stdout bound; stderr is always diagnostic-bounded.
type CaptureMode int

const (
	CaptureDiagnostic CaptureMode = iota
	CaptureMachine
)

// Captured is bounded output: at most limit bytes kept plus a truncation flag
// once anything beyond the limit was observed.
type Captured struct {
	Bytes     []byte
	Truncated bool
}

// Preview renders kept bytes as lossy UTF-8 (invalid sequences become U+FFFD,
// like String::from_utf8_lossy) and flags truncation explicitly.
func (c Captured) Preview() string {
	text := strings.ToValidUTF8(string(c.Bytes), "\uFFFD")
	if c.Truncated {
		return text + "\n[diagnostic output truncated]"
	}
	return text
}

// Status is the end state of a direct child, matching std::process::ExitStatus:
// an exit code when the leader exited normally, or the terminating signal when
// it was killed. Code reports false for signal termination, never a placeholder.
type Status struct {
	state *os.ProcessState
}

// Success reports a clean exit status of zero.
func (s Status) Success() bool { return s.state != nil && s.state.Success() }

// Code returns the process exit code, or false when a signal ended the child.
func (s Status) Code() (int, bool) {
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok {
		return ws.ExitStatus(), ws.Exited()
	}
	code := s.state.ExitCode()
	return code, code >= 0
}

// signalString mirrors the reference table on Linux: a searchable name in
// parentheses for known signals, nothing for unrecognized ones.
func signalString(signal int) string {
	switch syscall.Signal(signal) {
	case syscall.SIGHUP:
		return " (SIGHUP)"
	case syscall.SIGINT:
		return " (SIGINT)"
	case syscall.SIGQUIT:
		return " (SIGQUIT)"
	case syscall.SIGILL:
		return " (SIGILL)"
	case syscall.SIGTRAP:
		return " (SIGTRAP)"
	case syscall.SIGABRT:
		return " (SIGABRT)"
	case syscall.SIGBUS:
		return " (SIGBUS)"
	case syscall.SIGFPE:
		return " (SIGFPE)"
	case syscall.SIGKILL:
		return " (SIGKILL)"
	case syscall.SIGUSR1:
		return " (SIGUSR1)"
	case syscall.SIGSEGV:
		return " (SIGSEGV)"
	case syscall.SIGUSR2:
		return " (SIGUSR2)"
	case syscall.SIGPIPE:
		return " (SIGPIPE)"
	case syscall.SIGALRM:
		return " (SIGALRM)"
	case syscall.SIGTERM:
		return " (SIGTERM)"
	case syscall.SIGSTKFLT:
		return " (SIGSTKFLT)"
	case syscall.SIGCHLD:
		return " (SIGCHLD)"
	case syscall.SIGCONT:
		return " (SIGCONT)"
	case syscall.SIGSTOP:
		return " (SIGSTOP)"
	case syscall.SIGTSTP:
		return " (SIGTSTP)"
	case syscall.SIGTTIN:
		return " (SIGTTIN)"
	case syscall.SIGTTOU:
		return " (SIGTTOU)"
	case syscall.SIGURG:
		return " (SIGURG)"
	case syscall.SIGXCPU:
		return " (SIGXCPU)"
	case syscall.SIGXFSZ:
		return " (SIGXFSZ)"
	case syscall.SIGVTALRM:
		return " (SIGVTALRM)"
	case syscall.SIGPROF:
		return " (SIGPROF)"
	case syscall.SIGWINCH:
		return " (SIGWINCH)"
	case syscall.SIGIO:
		return " (SIGIO)"
	case syscall.SIGPWR:
		return " (SIGPWR)"
	case syscall.SIGSYS:
		return " (SIGSYS)"
	}
	return ""
}

// String renders the status exactly like Rust's ExitStatus Display on Linux.
func (s Status) String() string {
	if s.state == nil {
		return "unrecognised wait status: 0 0x0"
	}
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok {
		switch {
		case ws.Exited():
			return fmt.Sprintf("exit status: %d", ws.ExitStatus())
		case ws.Signaled():
			sig := int(ws.Signal())
			if ws.CoreDump() {
				return fmt.Sprintf("signal: %d%s (core dumped)", sig, signalString(sig))
			}
			return fmt.Sprintf("signal: %d%s", sig, signalString(sig))
		case ws.Stopped():
			sig := int(ws.StopSignal())
			return fmt.Sprintf("stopped (not terminated) by signal: %d%s", sig, signalString(sig))
		case ws.Continued():
			return "continued (WIFCONTINUED)"
		default:
			raw := int(ws)
			return fmt.Sprintf("unrecognised wait status: %d %#x", raw, raw)
		}
	}
	return s.state.String()
}

// ProcessOutput is the leader's status plus its two bounded captures.
type ProcessOutput struct {
	Status Status
	Stdout Captured
	Stderr Captured
}

// OutputTooLarge is the machine-capture failure for output past the ceiling.
type OutputTooLarge struct {
	Limit int
}

func (e *OutputTooLarge) Error() string {
	return fmt.Sprintf("Machine output exceeds %d bytes; complete output was not captured", e.Limit)
}

// errDeadlineElapsed mirrors tokio::time::error::Elapsed's display text.
var errDeadlineElapsed = errors.New("deadline has elapsed")

// IsDeadlineElapsed reports whether err carries a bounded-execution timeout
// marker produced by this package.
func IsDeadlineElapsed(err error) bool { return errors.Is(err, errDeadlineElapsed) }

// ErrCancelled is the capture cancellation result ("Operation cancelled").
var ErrCancelled = errors.New("Operation cancelled")

// ErrSessionCancelled is the bounded() cancellation result ("Session cancelled").
var ErrSessionCancelled = errors.New("Session cancelled")

// boundedRead drains r, keeping at most limit bytes and flagging anything more.
func boundedRead(r io.Reader, limit int) (Captured, error) {
	var kept []byte
	buf := make([]byte, 8192)
	truncated := false
	for {
		n, err := r.Read(buf)
		if n > 0 {
			remaining := limit - len(kept)
			if remaining < 0 {
				remaining = 0
			}
			truncated = truncated || n > remaining
			kept = append(kept, buf[:min(n, remaining)]...)
		}
		if err != nil {
			if err == io.EOF {
				return Captured{kept, truncated}, nil
			}
			return Captured{kept, truncated}, err
		}
	}
}

type readResult struct {
	captured Captured
	err      error
}

// cleanupGrace bounds post-kill joining: reaping a SIGKILLed leader and closing
// the read ends both finish promptly, so readers always join within it.
const cleanupGrace = 30 * time.Second

// Capture runs binary to completion, deadline expiry, or cancellation. The
// leader is always reaped; owned descendants are killed when it finishes, on
// timeout, or on cancellation, so an inheriting child cannot hold the pipes.
func Capture(ctx context.Context, binary string, args []string, cwd string, seconds uint64, mode CaptureMode) (*ProcessOutput, error) {
	if ctx.Err() != nil {
		return nil, ErrCancelled
	}
	cmd := Command(binary, cwd)
	cmd.Args = append(cmd.Args, args...)
	// Owned pipes, not exec.StdoutPipe: Wait must reap the leader without
	// racing the readers, which finish only once every inherited write end is
	// closed.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return nil, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stderrR.Close()
		stdoutW.Close()
		stderrW.Close()
		return nil, fmt.Errorf("Could not start %s: %w", binary, err)
	}
	// The child has its own pipe fds now; the parent's write ends must close or
	// the readers would never see EOF once the group is gone.
	stdoutW.Close()
	stderrW.Close()
	// The read ends are owned until every capture path joins its reader; a
	// deferred close also covers early returns, and closing while a reader is
	// blocked is exactly how terminate's force path unwinds it.
	defer stdoutR.Close()
	defer stderrR.Close()
	child := NewGroupChild(cmd)
	limit := DiagnosticLimit
	if mode == CaptureMachine {
		limit = MachineLimit
	}
	outCh := make(chan readResult, 1)
	errCh := make(chan readResult, 1)
	waitCh := make(chan error, 1)
	go func() {
		captured, err := boundedRead(stdoutR, limit)
		outCh <- readResult{captured, err}
	}()
	go func() {
		captured, err := boundedRead(stderrR, DiagnosticLimit)
		errCh <- readResult{captured, err}
	}()
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	var out, errOut readResult
	haveWait, haveOut, haveErr := false, false, false
	// terminate kills the whole group, then joins the leader wait and the
	// readers for up to cleanupGrace. A descendant that escaped the group
	// (setsid) or a leader stuck in uninterruptible sleep must not hang the
	// caller, so expiry forces the read ends closed and returns: channel sends
	// are buffered, the deferred closes are idempotent, and the goroutines
	// unwind on their own — the same orphan-reaping the reference relies on.
	terminate := func() {
		child.Close()
		deadline := time.NewTimer(cleanupGrace)
		defer deadline.Stop()
		for !haveWait || !haveOut || !haveErr {
			select {
			case <-waitCh:
				haveWait = true
			case <-outCh:
				haveOut = true
			case <-errCh:
				haveErr = true
			case <-deadline.C:
				stdoutR.Close()
				stderrR.Close()
				return
			}
		}
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	for !haveWait || !haveOut || !haveErr {
		select {
		case waitErr = <-waitCh:
			haveWait = true
			// Stop owned descendants as soon as the leader finishes, so their
			// inherited pipes reach EOF. Readers still drain buffered output.
			child.Close()
		case out = <-outCh:
			haveOut = true
		case errOut = <-errCh:
			haveErr = true
		case <-timer.C:
			terminate()
			return nil, fmt.Errorf("Command timed out: %w", errDeadlineElapsed)
		case <-ctx.Done():
			terminate()
			return nil, ErrCancelled
		}
	}
	if cmd.ProcessState == nil {
		// The leader was never reaped: a genuine wait failure, not an exit code.
		return nil, waitErr
	}
	if out.err != nil {
		return nil, out.err
	}
	if errOut.err != nil {
		return nil, errOut.err
	}
	return &ProcessOutput{
		Status: Status{cmd.ProcessState},
		Stdout: out.captured,
		Stderr: errOut.captured,
	}, nil
}

func ensureSuccess(binary string, output *ProcessOutput) error {
	if !output.Status.Success() {
		return fmt.Errorf("%s exited with %s: %s", binary, output.Status,
			store.Redact(output.Stdout.Preview()+"\n"+output.Stderr.Preview()))
	}
	return nil
}

// DiagnosticText renders human-readable evidence: bounded stdout, with bounded
// stderr appended when present.
func DiagnosticText(binary string, output *ProcessOutput) (string, error) {
	if err := ensureSuccess(binary, output); err != nil {
		return "", err
	}
	text := strings.TrimSpace(output.Stdout.Preview())
	if stderr := strings.TrimSpace(output.Stderr.Preview()); stderr != "" {
		text += "\n[stderr]\n" + stderr
	}
	return text, nil
}

func checked(ctx context.Context, binary string, args []string, cwd string, seconds uint64, mode CaptureMode) (string, error) {
	output, err := Capture(ctx, binary, args, cwd, seconds, mode)
	if err != nil {
		return "", err
	}
	if mode == CaptureDiagnostic {
		return DiagnosticText(binary, output)
	}
	if err := ensureSuccess(binary, output); err != nil {
		return "", err
	}
	if output.Stdout.Truncated {
		return "", &OutputTooLarge{Limit: MachineLimit}
	}
	if !utf8.Valid(output.Stdout.Bytes) {
		return "", errors.New("Machine output is not valid UTF-8")
	}
	return string(output.Stdout.Bytes), nil
}

// Run executes a command for human-readable evidence, keeping bounded stdout
// and stderr on success.
func Run(ctx context.Context, binary string, args []string, cwd string, seconds uint64) (string, error) {
	return checked(ctx, binary, args, cwd, seconds, CaptureDiagnostic)
}

// RunMachine executes a command whose stdout is machine output: fail closed on
// any command failure, truncation past the machine ceiling, or invalid UTF-8.
func RunMachine(ctx context.Context, binary string, args []string, cwd string, seconds uint64) (string, error) {
	return checked(ctx, binary, args, cwd, seconds, CaptureMachine)
}

// ShellCheck runs one operator-configured verification command through
// `bash -o pipefail -c` — the single place a shell is ever constructed — so a
// pipeline fails when any stage fails. The caller inspects the diagnostic
// capture itself: a nonzero status is evidence, not an error.
func ShellCheck(ctx context.Context, command string, cwd string, seconds uint64) (*ProcessOutput, error) {
	return Capture(ctx, "bash", []string{"-o", "pipefail", "-c", command}, cwd, seconds, CaptureDiagnostic)
}

// RunPredicate executes a command whose exit status is itself the answer.
// Success means true, falseCodes are the documented "predicate is false"
// statuses, and every other failure — spawn, timeout, signal, an unexpected
// code — still fails closed rather than reading as a false predicate.
func RunPredicate(ctx context.Context, binary string, args []string, cwd string, seconds uint64, falseCodes []int) (bool, error) {
	output, err := Capture(ctx, binary, args, cwd, seconds, CaptureMachine)
	if err != nil {
		return false, err
	}
	if output.Status.Success() {
		return true, nil
	}
	if code, ok := output.Status.Code(); ok && slices.Contains(falseCodes, code) {
		return false, nil
	}
	if err := ensureSuccess(binary, output); err != nil {
		return false, err
	}
	return false, nil
}

// deadlineGrace is the bounded window for a cancelled future to unwind before
// the caller reports expiry.
const deadlineGrace = 8 * time.Second

// Deadline reports how fn finished relative to the limit: Done carries fn's
// output, Expired reports whether ctx was already cancelled before the expiry
// cancellation fired.
type Deadline[T any] struct {
	Output           T
	Expired          bool
	AlreadyCancelled bool
}

// WithDeadline runs fn under limit. On expiry it cancels ctx and then waits a
// bounded grace period: the cancelled context prevents new turns and
// publication commands while owners stop their own detached process groups.
// AlreadyCancelled separates an operator cancellation from a genuine deadline.
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

// Bounded runs fn until it completes, seconds elapse, or ctx fires. `what` is
// the complete timeout message so callers keep their existing wording.
func Bounded[T any](ctx context.Context, seconds uint64, what string, fn func(context.Context) (T, error)) (T, error) {
	return BoundedAt(ctx, time.Now().Add(time.Duration(seconds)*time.Second), what, fn)
}

// BoundedAt is Bounded against an absolute deadline. On timeout or cancellation,
// it cancels the callback context and waits up to deadlineGrace for cleanup.
// Callbacks must observe cancellation; Go cannot forcibly stop a goroutine.
func BoundedAt[T any](ctx context.Context, deadline time.Time, what string, fn func(context.Context) (T, error)) (T, error) {
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
		err = fmt.Errorf("%s: %w", what, errDeadlineElapsed)
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
