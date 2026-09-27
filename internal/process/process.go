// Package process owns subprocess execution: every command runs as a process
// group leader so timeouts, cancellation and owner cleanup terminate the whole
// group, never just the direct child.
//
// Diagnostics and machine output are separate contracts: diagnostic captures
// keep a bounded preview for humans, the beginning of each stream and its real
// end, while machine captures fail closed on truncation or invalid UTF-8.
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

// Command builds an owned command: a new process group (pgid = child pid), the
// service's secret environment and Git's repository-locating variables
// removed, Git prompting disabled, and stdin on the null device. Callers
// override Stdin/Stdout/Stderr before Start as needed.
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
		// Repository-locating Git variables (as exported into Git hooks) would
		// redirect every child git away from cmd.Dir; Git itself clears them
		// before entering another repository. The GIT_CONFIG* channels are the
		// operator's deliberate configuration and pass through.
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

// GroupChild owns a started process and its recorded process group. Close kills
// the group first and then the leader, so background descendants die even after
// the leader completed normally. The group id stays reserved only while some
// member is alive: once the whole group has exited, the kernel may reuse it.
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
	// TailLimit bounds the rolling window that keeps the real end of a stream
	// past its limit, where commands usually state their result. The window is
	// allocated only once a stream passes its limit.
	TailLimit = 64 * 1024
)

// CaptureMode selects the stdout bound; stderr is always diagnostic-bounded.
type CaptureMode int

const (
	CaptureDiagnostic CaptureMode = iota
	CaptureMachine
)

// Captured is bounded output: at most limit bytes kept plus a truncation flag
// once anything beyond the limit was observed, and then the stream's last
// bytes, at most TailLimit of them (TailText).
type Captured struct {
	Bytes     []byte
	Truncated bool
	// tail is the rolling window's content, oldest byte first: the last bytes
	// written after Bytes. It is empty unless Truncated.
	tail []byte
}

// Text renders kept bytes as lossy UTF-8 (invalid sequences become U+FFFD).
// A truncated capture stops wherever the limit fell, possibly inside a secret
// that redaction can then no longer recognise, so its partial last line is
// dropped: the text is cut back to its last newline, or to its last
// whitespace when the kept bytes hold no newline, or to nothing. The first
// words or lines of a multi-word or multi-line environment secret the limit
// cut are dropped with it (redact.TrimCutSecretEnd).
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

// TailText renders the real end of a truncated capture, the rolling window
// of its last bytes, as lossy UTF-8; it is empty for a complete capture. The
// window starts wherever the stream then was, possibly inside a secret that
// redaction can then no longer recognise, so its partial first line is
// dropped, or, when the window holds no newline, its partial first word, or
// everything when there is no whitespace either. Redaction also recognises
// some tokens only after context that may have been dropped: a bearer token
// after "Bearer" and whitespace, an API key after a terminal escape sequence
// that holds spaces, and the later words or lines of a multi-word or
// multi-line environment secret after its first ones. So, repeatedly, the
// kept text loses its first word while what was dropped just before it could
// end a bearer prefix, loses such a key and the rest of its sequence, and
// loses the later part of such an environment secret
// (redact.TrimCutSecretStart) with the rest of the word it ends in, which may
// belong to a token redaction recognises only with that part.
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
	for {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		// A word to drop ends at the first whitespace from `from` on.
		from := -1
		if key := cutEscapeKey.FindStringIndex(rest); key != nil {
			from = key[1]
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

// cutEscapeKey matches kept text that starts with an API key redaction
// recognises only after a terminal escape sequence whose last byte is a
// letter: the rest of such a sequence, whose intermediate bytes may be
// spaces, then the key (redact's key pattern).
var cutEscapeKey = regexp.MustCompile(`(?i)^[\x20-\x2f]*[a-z]sk-[a-z0-9_-]{10}`)

// mayEndBearerPrefix reports whether text, dropped just before the kept text
// and with trailing whitespace removed, could end the "Bearer" that redaction
// needs before a token: it is empty (the window began in the whitespace after
// the prefix), all of it is the end of the prefix (the window began inside
// it), or it ends with the whole prefix.
func mayEndBearerPrefix(text string) bool {
	const prefix = "bearer"
	n := min(len(text), len(prefix))
	return (n == len(text) || n == len(prefix)) && strings.EqualFold(text[len(text)-n:], prefix[len(prefix)-n:])
}

// diagnosticTruncatedMarker stands where a truncated capture dropped output.
const diagnosticTruncatedMarker = "[diagnostic output truncated]"

// Preview renders Text and flags truncation explicitly: a truncated capture
// is followed by a marker line, then by its real end (TailText) when that
// holds anything.
func (c Captured) Preview() string {
	return joinPreview(c.Text(), c.TailText(), c.Truncated)
}

// joinPreview renders a stream's head, and for a truncated capture the marker
// and its tail, as Preview does.
func joinPreview(head, tail string, truncated bool) string {
	if !truncated {
		return head
	}
	if tail == "" {
		return head + "\n" + diagnosticTruncatedMarker
	}
	return head + "\n" + diagnosticTruncatedMarker + "\n" + tail
}

// Status is the end state of a direct child: an exit code when the leader
// exited normally, or the terminating signal when it was killed. Code reports
// false for signal termination, never a placeholder.
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

// signalString returns the searchable signal name in parentheses for known
// signals, nothing for unrecognized ones.
func signalString(signal int) string {
	if name := unix.SignalName(syscall.Signal(signal)); name != "" {
		return " (" + name + ")"
	}
	return ""
}

// String renders the Linux exit status used in process diagnostics.
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

// errDeadlineElapsed is the stable bounded-execution timeout text.
var errDeadlineElapsed = errors.New("deadline has elapsed")

// IsDeadlineElapsed reports whether err carries a bounded-execution timeout
// marker produced by this package.
func IsDeadlineElapsed(err error) bool { return errors.Is(err, errDeadlineElapsed) }

// ErrCancelled is the capture cancellation result ("Operation cancelled").
var ErrCancelled = errors.New("Operation cancelled")

// ErrSessionCancelled is the bounded() cancellation result ("Session cancelled").
var ErrSessionCancelled = errors.New("Session cancelled")

// boundedRead drains r, keeping at most limit bytes and flagging anything
// more, whose last TailLimit bytes it keeps in a rolling window.
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

// tailWindow keeps the last TailLimit bytes written to it in a ring buffer
// allocated on the first write.
type tailWindow struct {
	buf  []byte
	next int // the oldest byte's position once buf is full
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

// bytes returns the kept bytes oldest first, rotating the ring in place.
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

// cleanupGrace bounds post-kill joining: reaping a SIGKILLed leader and closing
// the read ends both finish promptly, so readers always join within it.
const cleanupGrace = 30 * time.Second

// terminateGrace bounds how long a signalled leader may run its own cleanup
// before its group is killed.
const terminateGrace = 2 * time.Second

// Capture runs binary to completion, deadline expiry, or cancellation. The
// leader is always reaped; owned descendants are killed when it finishes, on
// timeout, or on cancellation, so an inheriting child cannot hold the pipes.
// On timeout or cancellation a still-running leader's group is sent SIGTERM
// first; the group is killed once the leader exits or terminateGrace elapses.
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
	// unwind on their own and orphaned processes are reaped.
	terminate := func() {
		// A still-running leader's group first gets SIGTERM so Git and similar
		// tools can remove their lock files; whatever remains is killed after
		// terminateGrace. A reaped leader's group gets no SIGTERM: Close already
		// killed it, and its id may since have been reused.
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

const (
	// failureTextLimit bounds a failure message, in characters. The store cuts
	// recorded messages at 16,384 characters, and callers usually wrap a
	// failure in context first ("Open pull request inventory failed: ", a
	// blocked reason), so the message leaves room for that context: recording
	// a wrapped failure then never cuts the stderr tail that states the cause.
	failureTextLimit = 16384 - 1024
	// stderrShare is the part of an over-long failure message stderr may always
	// claim, however much stdout there was: stderr usually carries the cause
	// (an HTTP error, a "fatal:" line) while stdout carries bulk output.
	stderrShare = 4096
	// elisionReserve is room for elideMiddle's marker at any omitted count a
	// capture can produce.
	elisionReserve = 48
)

func ensureSuccess(binary string, output *ProcessOutput) error {
	if output.Status.Success() {
		return nil
	}
	return errors.New(failureText(binary, output))
}

// failureText renders a failed command for operators: its exit status, then
// scrubbed stdout and stderr, each as its Preview. Output that fits
// failureTextLimit is kept whole, as `<stdout>\n<stderr>`. Longer output keeps
// both ends of each stream around an explicit marker, in
// `<stdout>\n[stderr]\n<stderr>` form (no section when stderr is empty);
// stderr may always use up to stderrShare of the bound however long stdout
// is. Preview drops the partial lines a capture limit or its tail window cut,
// and secrets are scrubbed from the remaining text before anything is cut
// here, so no cut exposes part of a secret.
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

// failurePreview is one stream's Preview with secrets scrubbed from its head
// and its tail separately, so that eliding it can keep the beginning of one
// and the end of the other.
type failurePreview struct {
	head, tail string
	truncated  bool
}

func scrubPreview(c Captured) failurePreview {
	return failurePreview{head: redact.Secrets(c.Text()), tail: redact.Secrets(c.TailText()), truncated: c.Truncated}
}

func (p failurePreview) text() string { return joinPreview(p.head, p.tail, p.truncated) }

func (p failurePreview) runes() int { return utf8.RuneCountInString(p.text()) }

// elide shortens the preview to at most limit characters, keeping both of its
// ends; limit must leave room for its markers, as elideMiddle's does. A
// capture whose real end is kept always shows the truncation marker between
// its head and its tail: whichever of them fits in half the room stays whole
// while the other keeps both of its ends around an omission count
// (elideMiddle); otherwise the beginning of the head and the end of the tail
// remain around the truncation marker alone.
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

// elideMiddle shortens text to at most limit characters, keeping its beginning
// and end around a marker that states how many characters were omitted. limit
// must leave room for the marker (elisionReserve).
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

// runeOffset returns the byte offset at which the nth character of s starts,
// or len(s) when s has no more than n characters.
func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

// RunMachine executes a command whose stdout is machine output: fail closed on
// any command failure, truncation past the machine ceiling, or invalid UTF-8.
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
// code — still fails closed rather than reading as a false predicate. Output
// is kept only as diagnostic evidence for such a failure.
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

// deadlineGrace is the bounded window for a cancelled future to unwind before
// the caller reports expiry.
const deadlineGrace = 8 * time.Second

// Deadline reports how fn finished relative to the limit. Output carries fn's
// result when it returned within the limit. Expired reports that the limit
// elapsed first (Output is then the zero value). AlreadyCancelled reports that
// ctx was already cancelled when the limit elapsed, separating an operator
// cancellation from a genuine deadline.
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
