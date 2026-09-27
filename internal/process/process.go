package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
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

func (c Captured) Text() string {
	text := strings.ToValidUTF8(string(c.Bytes), "\uFFFD")
	if !c.Truncated {
		return text
	}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		return redact.TrimCutSecretEnd(text[:i])
	}
	if i := strings.LastIndexFunc(text, unicode.IsSpace); i >= 0 {
		return redact.TrimCutSecretEnd(text[:i])
	}
	return ""
}

func (c Captured) TailText() string {
	if !c.Truncated {
		return ""
	}
	text := strings.ToValidUTF8(string(c.tail), "\uFFFD")
	dropped, rest, found := strings.Cut(text, "\n")
	if !found {
		i := strings.IndexFunc(text, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		dropped, rest = text[:i], text[i:]
	}
	run := 0
	for {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		at := len(text) - len(rest)
		if at >= run {
			run = at + len(rest) - len(strings.TrimLeft(rest, escapeIntermediates))
		}
		from := -1
		if key := cutEscapeKey.FindStringIndex(text[run:]); key != nil {
			from = run - at + key[1]
		} else if mayEndBearerPrefix(strings.TrimRightFunc(dropped, unicode.IsSpace)) {
			from = 0
		} else if trimmed := redact.TrimCutSecretStart(rest); len(trimmed) < len(rest) {
			from = len(rest) - len(trimmed)
		}
		if from < 0 {
			return rest
		}
		i := strings.IndexFunc(rest[from:], unicode.IsSpace)
		if i < 0 {
			return ""
		}
		dropped, rest = rest[:from+i], rest[from+i:]
	}
}

const escapeIntermediates = " !\"#$%&'()*+,-./"

var cutEscapeKey = regexp.MustCompile(`(?i)^[a-z]sk-[a-z0-9_-]{10}`)

func mayEndBearerPrefix(text string) bool {
	const prefix = "bearer"
	n := min(len(text), len(prefix))
	return (n == len(text) || n == len(prefix)) && strings.EqualFold(text[len(text)-n:], prefix[len(prefix)-n:])
}

const diagnosticTruncatedMarker = "[diagnostic output truncated]"

func (c Captured) Preview() string {
	return joinPreview(c.Text(), c.TailText(), c.Truncated)
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
}

func (s Status) Success() bool { return s.state != nil && s.state.Success() }

func (s Status) Code() (int, bool) {
	if ws, ok := s.state.Sys().(syscall.WaitStatus); ok {
		return ws.ExitStatus(), ws.Exited()
	}
	code := s.state.ExitCode()
	return code, code >= 0
}

func signalString(signal int) string {
	if name := unix.SignalName(syscall.Signal(signal)); name != "" {
		return " (" + name + ")"
	}
	return ""
}

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

func Capture(ctx context.Context, binary string, args []string, cwd string, seconds uint64, mode CaptureMode) (*ProcessOutput, error) {
	if ctx.Err() != nil {
		return nil, ErrCancelled
	}
	cmd := Command(binary, cwd)
	cmd.Args = append(cmd.Args, args...)
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
	// The child holds its own pipe fds now; the parent's write ends must close or readers never see EOF.
	stdoutW.Close()
	stderrW.Close()
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
	terminate := func() {
		if !haveWait {
			_ = syscall.Kill(-child.pgid, syscall.SIGTERM)
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
	if joined := redact.Secrets(output.Stdout.Preview() + "\n" + output.Stderr.Preview()); utf8.RuneCountInString(joined) <= budget {
		return prefix + joined
	}
	stdout, stderr := scrubPreview(output.Stdout), scrubPreview(output.Stderr)
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

func scrubPreview(c Captured) failurePreview {
	return failurePreview{head: redact.Secrets(c.Text()), tail: redact.Secrets(c.TailText()), truncated: c.Truncated}
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
	output, err := Capture(ctx, binary, args, cwd, seconds, CaptureDiagnostic)
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
