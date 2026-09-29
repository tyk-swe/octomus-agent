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

	"github.com/tyk-swe/octomus-agent/internal/redact"
	"golang.org/x/sys/unix"
)

func Command(binary string, cwd string) *exec.Cmd {
	cmd := exec.Command(binary)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case redact.TokenEnv, redact.WebhookEnv, "GIT_TERMINAL_PROMPT":
			continue
		// Repository-locating Git variables would redirect child git away from cmd.Dir; GIT_CONFIG* passes through.
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_COMMON_DIR",
			"GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE", "GIT_NO_REPLACE_OBJECTS",
			"GIT_REPLACE_REF_BASE":
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0")
	return cmd
}

type GroupChild struct {
	Cmd  *exec.Cmd
	pgid int
	once sync.Once
}

func NewGroupChild(cmd *exec.Cmd) *GroupChild {
	return &GroupChild{Cmd: cmd, pgid: cmd.Process.Pid}
}

func (g *GroupChild) Close() {
	g.once.Do(func() {
		_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
		_ = g.Cmd.Process.Kill()
	})
}

const (
	DiagnosticLimit = 262_144
	MachineLimit    = 16 * 1024 * 1024
	TailLimit       = 64 * 1024
)

type CaptureMode int

const (
	CaptureDiagnostic CaptureMode = iota
	CaptureMachine
)

type Captured struct {
	Bytes     []byte
	Truncated bool
	tail      []byte
}

func (c Captured) SafeText() string {
	text := strings.ToValidUTF8(string(c.Bytes), "\uFFFD")
	if !c.Truncated {
		return redact.Secrets(text)
	}
	return redact.Fragment(text, redact.HeadLineCut)
}

func (c Captured) SafeTailText() string {
	if !c.Truncated {
		return ""
	}
	return redact.Fragment(strings.ToValidUTF8(string(c.tail), "\uFFFD"), redact.TailLineCut)
}

const diagnosticTruncatedMarker = "[diagnostic output truncated]"

func (c Captured) SafePreview() string {
	return strings.Join(redact.Parts(c.previewParts()...), "")
}

func (c Captured) previewParts() []redact.Part {
	parts := []redact.Part{{Text: strings.ToValidUTF8(string(c.Bytes), "\uFFFD"), CutEnd: c.Truncated}}
	if c.Truncated {
		parts = append(parts, redact.Part{Text: "\n" + diagnosticTruncatedMarker},
			redact.Part{Text: strings.ToValidUTF8(string(c.tail), "\uFFFD"), CutStart: true, Prefix: "\n"})
	}
	return parts
}

func joinPreview(head, tail string, truncated bool) string {
	if !truncated {
		return head
	}
	if tail == "" {
		return head + "\n" + diagnosticTruncatedMarker
	}
	return head + "\n" + diagnosticTruncatedMarker + "\n" + tail
}

type Status struct {
	state *os.ProcessState
	exit  *Exit
}

// Exit is how a sandboxed child ended as its container runtime reports it: the init process's exit code, whether the
// memory limit killed it, and whether Octomus itself stopped it.
type Exit struct {
	Code   int
	OOM    bool
	Killed bool
	// Reason explains a kill that was not the caller's own, such as a sandbox time limit.
	Reason string
}

// ErrKilled marks a sandboxed child that Octomus stopped on purpose, the counterpart of a host group's SIGKILL.
var ErrKilled = errors.New("stopped by Octomus")

func HostStatus(state *os.ProcessState) Status { return Status{state: state} }

func ExitStatus(exit Exit) Status { return Status{exit: &exit} }

func (s Status) Success() bool {
	if s.exit != nil {
		return s.exit.Code == 0 && !s.exit.OOM && !s.exit.Killed
	}
	return s.state != nil && s.state.Success()
}

func (s Status) OOM() bool { return s.exit != nil && s.exit.OOM }

func (s Status) Code() (int, bool) {
	if s.exit != nil {
		return s.exit.Code, !s.exit.Killed
	}
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok {
		return ws.ExitStatus(), ws.Exited()
	}
	code := s.state.ExitCode()
	return code, code >= 0
}

// Err reports an unsuccessful exit the way exec does for host children, so owners can tell a deliberate kill apart.
func (s Status) Err() error {
	switch {
	case s.Success():
		return nil
	case s.exit != nil && s.exit.Killed:
		return ErrKilled
	case s.exit != nil:
		return errors.New(s.String())
	case s.state == nil:
		return errors.New(s.String())
	}
	return &exec.ExitError{ProcessState: s.state}
}

func signalString(signal int) string {
	if name := unix.SignalName(syscall.Signal(signal)); name != "" {
		return " (" + name + ")"
	}
	return ""
}

func (s Status) String() string {
	if s.exit != nil {
		switch {
		case s.exit.OOM:
			return fmt.Sprintf("exit status: %d (sandbox memory limit exceeded)", s.exit.Code)
		case s.exit.Killed && s.exit.Reason != "":
			return s.exit.Reason
		case s.exit.Killed:
			return "stopped by Octomus"
		}
		return fmt.Sprintf("exit status: %d", s.exit.Code)
	}
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

type ProcessOutput struct {
	Status Status
	Stdout Captured
	Stderr Captured
}

type OutputTooLarge struct {
	Limit int
}

func (e *OutputTooLarge) Error() string {
	return fmt.Sprintf("Machine output exceeds %d bytes; complete output was not captured", e.Limit)
}

var errDeadlineElapsed = errors.New("deadline has elapsed")

func IsDeadlineElapsed(err error) bool { return errors.Is(err, errDeadlineElapsed) }

var ErrCancelled = errors.New("Operation cancelled")

var ErrSessionCancelled = errors.New("Session cancelled")

func boundedRead(r io.Reader, limit int) (Captured, error) {
	var kept []byte
	var tail tailWindow
	buf := make([]byte, 8192)
	truncated := false
	for {
		n, err := r.Read(buf)
		if n > 0 {
			take := min(n, limit-len(kept))
			kept = append(kept, buf[:take]...)
			if take < n {
				truncated = true
				tail.write(buf[take:n])
			}
		}
		if err != nil {
			captured := Captured{Bytes: kept, Truncated: truncated, tail: tail.bytes()}
			if err == io.EOF {
				return captured, nil
			}
			return captured, err
		}
	}
}

type tailWindow struct {
	buf  []byte
	next int
}

func (w *tailWindow) write(p []byte) {
	if len(p) > TailLimit {
		p = p[len(p)-TailLimit:]
	}
	if w.buf == nil {
		w.buf = make([]byte, 0, TailLimit)
	}
	if room := TailLimit - len(w.buf); room > 0 {
		n := min(room, len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
	}
	for len(p) > 0 {
		n := copy(w.buf[w.next:], p)
		p = p[n:]
		w.next = (w.next + n) % TailLimit
	}
}

func (w *tailWindow) bytes() []byte {
	slices.Reverse(w.buf[:w.next])
	slices.Reverse(w.buf[w.next:])
	slices.Reverse(w.buf)
	w.next = 0
	return w.buf
}

type readResult struct {
	captured Captured
	err      error
}

const cleanupGrace = 30 * time.Second

const terminateGrace = 2 * time.Second

// Proc is a started child whose lifetime a capture owns: a host process group or a sandbox.
type Proc interface {
	Wait() (Status, error)
	Terminate()
	Kill()
}

// HostChild is a command running in its own process group on this host, with pipes for its standard streams.
type HostChild struct {
	cmd    *exec.Cmd
	group  *GroupChild
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	once   sync.Once
	done   chan struct{}
	status Status
	err    error
}

// StartHost starts binary in its own process group with the Command environment plus extra, piping stdout and stderr,
// and stdin only when requested.
func StartHost(binary string, args []string, cwd string, extra []string, stdin bool) (*HostChild, error) {
	cmd := Command(binary, cwd)
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(cmd.Env, extra...)
	var parentEnds, childEnds []*os.File
	closeAll := func() {
		for _, f := range append(parentEnds, childEnds...) {
			f.Close()
		}
	}
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			closeAll()
		}
		return r, w, err
	}
	child := &HostChild{cmd: cmd, done: make(chan struct{})}
	if stdin {
		r, w, err := pipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdin, child.stdin = r, w
		parentEnds, childEnds = append(parentEnds, w), append(childEnds, r)
	}
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, child.stdout = stdoutW, stdoutR
	parentEnds, childEnds = append(parentEnds, stdoutR), append(childEnds, stdoutW)
	stderrR, stderrW, err := pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr, child.stderr = stderrW, stderrR
	parentEnds, childEnds = append(parentEnds, stderrR), append(childEnds, stderrW)
	if err := cmd.Start(); err != nil {
		closeAll()
		return nil, err
	}
	// The child holds its own pipe fds now; the parent's copies must close or readers never see EOF.
	for _, f := range childEnds {
		f.Close()
	}
	child.group = NewGroupChild(cmd)
	return child, nil
}

func (h *HostChild) Stdin() *os.File                { return h.stdin }
func (h *HostChild) Stdout() io.ReadCloser          { return h.stdout }
func (h *HostChild) Stderr() io.ReadCloser          { return h.stderr }
func (h *HostChild) Pid() int                       { return h.cmd.Process.Pid }
func (h *HostChild) Terminate()                     { _ = syscall.Kill(-h.group.pgid, syscall.SIGTERM) }
func (h *HostChild) Kill()                          { h.group.Close() }
func (h *HostChild) Group() *GroupChild             { return h.group }
func (h *HostChild) ProcessState() *os.ProcessState { return h.cmd.ProcessState }

// Wait reaps the child once; later calls return the same result.
func (h *HostChild) Wait() (Status, error) {
	h.once.Do(func() {
		defer close(h.done)
		err := h.cmd.Wait()
		if h.cmd.ProcessState == nil {
			h.err = err
			return
		}
		h.status = HostStatus(h.cmd.ProcessState)
	})
	<-h.done
	return h.status, h.err
}

func Capture(ctx context.Context, binary string, args []string, cwd string, seconds uint64, mode CaptureMode) (*ProcessOutput, error) {
	return CaptureEnv(ctx, binary, args, cwd, seconds, mode, nil)
}

// CaptureEnv is Capture with extra environment values appended to the Command environment.
func CaptureEnv(ctx context.Context, binary string, args []string, cwd string, seconds uint64, mode CaptureMode, env []string) (*ProcessOutput, error) {
	if ctx.Err() != nil {
		return nil, ErrCancelled
	}
	child, err := StartHost(binary, args, cwd, env, false)
	if err != nil {
		return nil, fmt.Errorf("Could not start %s: %w", binary, err)
	}
	return CaptureStarted(ctx, child, child.Stdout(), child.Stderr(), seconds, mode)
}

// CaptureStarted bounds an already started child's output and lifetime: a timeout or cancellation terminates it,
// escalates to a kill after a grace period, and a normal exit still kills anything it left running.
func CaptureStarted(ctx context.Context, proc Proc, stdout, stderr io.ReadCloser, seconds uint64, mode CaptureMode) (*ProcessOutput, error) {
	defer stdout.Close()
	defer stderr.Close()
	limit := DiagnosticLimit
	if mode == CaptureMachine {
		limit = MachineLimit
	}
	outCh := make(chan readResult, 1)
	errCh := make(chan readResult, 1)
	type waited struct {
		status Status
		err    error
	}
	waitCh := make(chan waited, 1)
	go func() {
		captured, err := boundedRead(stdout, limit)
		outCh <- readResult{captured, err}
	}()
	go func() {
		captured, err := boundedRead(stderr, DiagnosticLimit)
		errCh <- readResult{captured, err}
	}()
	go func() {
		status, err := proc.Wait()
		waitCh <- waited{status, err}
	}()
	var result waited
	var out, errOut readResult
	haveWait, haveOut, haveErr := false, false, false
	terminate := func() {
		if !haveWait {
			proc.Terminate()
			grace := time.NewTimer(terminateGrace)
		term:
			for !haveWait {
				select {
				case <-waitCh:
					haveWait = true
				case <-outCh:
					haveOut = true
				case <-errCh:
					haveErr = true
				case <-grace.C:
					break term
				}
			}
			grace.Stop()
		}
		proc.Kill()
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
				stdout.Close()
				stderr.Close()
				return
			}
		}
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	for !haveWait || !haveOut || !haveErr {
		select {
		case result = <-waitCh:
			haveWait = true
			proc.Kill()
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
	if result.err != nil {
		return nil, result.err
	}
	if out.err != nil {
		return nil, out.err
	}
	if errOut.err != nil {
		return nil, errOut.err
	}
	return &ProcessOutput{
		Status: result.status,
		Stdout: out.captured,
		Stderr: errOut.captured,
	}, nil
}

const (
	failureTextLimit = 16384 - 1024
	stderrShare      = 4096
	elisionReserve   = 48
)

func ensureSuccess(binary string, output *ProcessOutput) error {
	if output.Status.Success() {
		return nil
	}
	return errors.New(failureText(binary, output))
}

func failureText(binary string, output *ProcessOutput) string {
	prefix := fmt.Sprintf("%s exited with %s: ", binary, output.Status)
	budget := failureTextLimit - utf8.RuneCountInString(prefix)
	stdoutParts := output.Stdout.previewParts()
	parts := append(stdoutParts, redact.Part{Text: "\n"})
	parts = append(parts, output.Stderr.previewParts()...)
	// Keep both streams intact until all overlapping secret spans are found.
	// The same scrubbed parts feed both the complete and shortened error forms.
	safe := redact.Parts(parts...)
	if joined := strings.Join(safe, ""); utf8.RuneCountInString(joined) <= budget {
		return prefix + joined
	}
	stdout, stderr := scrubbedPreview(safe[:len(stdoutParts)]), scrubbedPreview(safe[len(stdoutParts)+1:])
	if stderr.text() == "" {
		return prefix + stdout.elide(budget)
	}
	const separator = "\n[stderr]\n"
	budget -= utf8.RuneCountInString(separator)
	stderrText := stderr.elide(min(stderr.runes(), max(stderrShare, budget-stdout.runes())))
	return prefix + stdout.elide(budget-utf8.RuneCountInString(stderrText)) + separator + stderrText
}

type failurePreview struct {
	head, tail string
	truncated  bool
}

func scrubbedPreview(parts []string) failurePreview {
	preview := failurePreview{head: parts[0], truncated: len(parts) == 3}
	if preview.truncated {
		preview.tail = strings.TrimPrefix(parts[2], "\n")
	}
	return preview
}

func (p failurePreview) text() string { return joinPreview(p.head, p.tail, p.truncated) }

func (p failurePreview) runes() int { return utf8.RuneCountInString(p.text()) }

func (p failurePreview) elide(limit int) string {
	text := p.text()
	if p.tail == "" || utf8.RuneCountInString(text) <= limit {
		return elideMiddle(text, limit)
	}
	const marker = "\n" + diagnosticTruncatedMarker + "\n"
	keep := max(limit-utf8.RuneCountInString(marker), 0)
	headRunes, tailRunes := utf8.RuneCountInString(p.head), utf8.RuneCountInString(p.tail)
	switch {
	case headRunes <= keep/2:
		return p.head + marker + elideMiddle(p.tail, keep-headRunes)
	case tailRunes <= keep-keep/2:
		return elideMiddle(p.head, keep-tailRunes) + marker + p.tail
	}
	return p.head[:runeOffset(p.head, keep/2)] + marker + p.tail[runeOffset(p.tail, tailRunes-(keep-keep/2)):]
}

func elideMiddle(text string, limit int) string {
	total := utf8.RuneCountInString(text)
	if total <= limit {
		return text
	}
	keep := max(limit-elisionReserve, 0)
	head := keep / 2
	tail := keep - head
	return text[:runeOffset(text, head)] +
		fmt.Sprintf("\n[... %d characters omitted ...]\n", total-keep) +
		text[runeOffset(text, total-tail):]
}

func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

func RunMachine(ctx context.Context, binary string, args []string, cwd string, seconds uint64) (string, error) {
	output, err := Capture(ctx, binary, args, cwd, seconds, CaptureMachine)
	return machineResult(binary, output, err)
}

// RunMachineEnv is RunMachine with extra environment values appended to the Command environment.
func RunMachineEnv(ctx context.Context, binary string, args []string, cwd string, seconds uint64, env []string) (string, error) {
	output, err := CaptureEnv(ctx, binary, args, cwd, seconds, CaptureMachine, env)
	return machineResult(binary, output, err)
}

func machineResult(binary string, output *ProcessOutput, err error) (string, error) {
	if err != nil {
		return "", err
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

func ShellCheck(ctx context.Context, command string, cwd string, seconds uint64) (*ProcessOutput, error) {
	return Capture(ctx, "bash", []string{"-o", "pipefail", "-c", command}, cwd, seconds, CaptureDiagnostic)
}

func RunPredicate(ctx context.Context, binary string, args []string, cwd string, seconds uint64, falseCodes []int) (bool, error) {
	return RunPredicateEnv(ctx, binary, args, cwd, seconds, falseCodes, nil)
}

// RunPredicateEnv is RunPredicate with extra environment values appended to the Command environment.
func RunPredicateEnv(ctx context.Context, binary string, args []string, cwd string, seconds uint64, falseCodes []int, env []string) (bool, error) {
	output, err := CaptureEnv(ctx, binary, args, cwd, seconds, CaptureDiagnostic, env)
	if err != nil {
		return false, err
	}
	if output.Status.Success() {
		return true, nil
	}
	if code, ok := output.Status.Code(); ok && slices.Contains(falseCodes, code) {
		return false, nil
	}
	return false, ensureSuccess(binary, output)
}

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

func Bounded[T any](ctx context.Context, seconds uint64, what string, fn func(context.Context) (T, error)) (T, error) {
	return BoundedAt(ctx, time.Now().Add(time.Duration(seconds)*time.Second), what, fn)
}

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
