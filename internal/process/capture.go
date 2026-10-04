package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/redact"
)

const (
	diagnosticLimit = 262_144
	machineLimit    = 16 * 1024 * 1024
	tailLimit       = 64 * 1024
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

const truncatedMarker = "[diagnostic output truncated]"

func (c Captured) SafePreview() string {
	return strings.Join(redact.Parts(c.previewParts()...), "")
}

func (c Captured) previewParts() []redact.Part {
	parts := []redact.Part{{Text: strings.ToValidUTF8(string(c.Bytes), "�"), CutEnd: c.Truncated}}
	if c.Truncated {
		parts = append(parts, redact.Part{Text: "\n" + truncatedMarker},
			redact.Part{Text: strings.ToValidUTF8(string(c.tail), "�"), CutStart: true, Prefix: "\n"})
	}
	return parts
}

func joinPreview(head, tail string, truncated bool) string {
	if !truncated {
		return head
	}
	if tail == "" {
		return head + "\n" + truncatedMarker
	}
	return head + "\n" + truncatedMarker + "\n" + tail
}

type Output struct {
	Status Status
	Stdout Captured
	Stderr Captured
}

// SafeCapture contains a stream's retained fragments after capture cuts and secret
// redaction. The caller chooses its display separators and bounds only afterwards.
type SafeCapture struct {
	Head, Tail string
	Truncated  bool
}

// SafeCaptures scrubs both streams together, before a caller inserts a stderr
// label or trims whitespace that may separate a bearer prefix from its token.
// It preserves each stream's head and tail for their different display policies.
func (o Output) SafeCaptures() (stdout, stderr SafeCapture) {
	safe, stdoutParts := o.safeParts()
	convert := func(parts []string) SafeCapture {
		p := scrubbedPreview(parts)
		return SafeCapture{Head: p.head, Tail: p.tail, Truncated: p.truncated}
	}
	return convert(safe[:stdoutParts]), convert(safe[stdoutParts+1:])
}

func (o Output) safeParts() ([]string, int) {
	stdoutParts := o.Stdout.previewParts()
	boundary := len(stdoutParts)
	safe := redact.Streams(stdoutParts, o.Stderr.previewParts())
	// A newline-leading secret can put its replacement in the synthetic
	// separator. Keep that evidence in stderr when callers omit the separator.
	if separator := safe[boundary]; separator != "" && separator != "\n" {
		safe[boundary+1] = separator + safe[boundary+1]
		safe[boundary] = ""
	}
	return safe, boundary
}

type outputTooLarge struct {
	Limit int
}

func (e *outputTooLarge) Error() string {
	return fmt.Sprintf("Machine output exceeds %d bytes; complete output was not captured", e.Limit)
}

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
	if len(p) > tailLimit {
		p = p[len(p)-tailLimit:]
	}
	if w.buf == nil {
		w.buf = make([]byte, 0, tailLimit)
	}
	if room := tailLimit - len(w.buf); room > 0 {
		n := min(room, len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
	}
	for len(p) > 0 {
		n := copy(w.buf[w.next:], p)
		p = p[n:]
		w.next = (w.next + n) % tailLimit
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

// CaptureHost starts binary on this host with the Command environment plus env and bounds it like Capture.
func CaptureHost(ctx context.Context, binary string, args []string, cwd string, env []string, seconds uint64, mode CaptureMode) (*Output, error) {
	if ctx.Err() != nil {
		return nil, ErrCancelled
	}
	child, err := StartHost(binary, args, cwd, env, false)
	if err != nil {
		return nil, fmt.Errorf("Could not start %s: %w", binary, err)
	}
	return Capture(ctx, child, child.Stdout(), child.Stderr(), seconds, mode)
}

// Capture bounds an already started child's output and lifetime: a timeout or cancellation terminates it,
// escalates to a kill after a grace period, and a normal exit still kills anything it left running.
func Capture(ctx context.Context, proc Proc, stdout, stderr io.ReadCloser, seconds uint64, mode CaptureMode) (*Output, error) {
	defer stdout.Close()
	defer stderr.Close()
	limit := diagnosticLimit
	if mode == CaptureMachine {
		limit = machineLimit
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
		captured, err := boundedRead(stderr, diagnosticLimit)
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
				case result = <-waitCh:
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
			case result = <-waitCh:
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
			return nil, stopped(fmt.Errorf("Command timed out: %w", ErrDeadlineElapsed), result.err)
		case <-ctx.Done():
			terminate()
			return nil, stopped(ErrCancelled, result.err)
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
	return &Output{
		Status: result.status,
		Stdout: out.captured,
		Stderr: errOut.captured,
	}, nil
}

// stopped keeps why a child that was stopped could not report a clean end, such as a sandbox whose end its broker
// never confirmed, beside why it was stopped: that end is not an ordinary timeout or cancellation.
func stopped(why, ended error) error {
	if ended == nil {
		return why
	}
	return fmt.Errorf("%w: %w", why, ended)
}

const (
	failureTextLimit = 16384 - 1024
	stderrShare      = 4096
	elisionReserve   = 48
)

func ensureSuccess(binary string, output *Output) error {
	if output.Status.Success() {
		return nil
	}
	return errors.New(failureText(binary, output))
}

func failureText(binary string, output *Output) string {
	prefix := fmt.Sprintf("%s exited with %s: ", binary, output.Status)
	budget := failureTextLimit - utf8.RuneCountInString(prefix)
	// Keep both streams intact until all overlapping secret spans are found.
	// The same scrubbed parts feed both the complete and shortened error forms.
	safe, stdoutParts := output.safeParts()
	if joined := strings.Join(safe, ""); utf8.RuneCountInString(joined) <= budget {
		return prefix + joined
	}
	stdout, stderr := scrubbedPreview(safe[:stdoutParts]), scrubbedPreview(safe[stdoutParts+1:])
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
	const marker = "\n" + truncatedMarker + "\n"
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

// RunMachine runs binary on this host with the Command environment plus env and returns its complete stdout for a
// program to parse: a failed exit, truncated or non-UTF-8 output is an error.
func RunMachine(ctx context.Context, binary string, args []string, cwd string, seconds uint64, env []string) (string, error) {
	output, err := CaptureHost(ctx, binary, args, cwd, env, seconds, CaptureMachine)
	if err != nil {
		return "", err
	}
	if err := ensureSuccess(binary, output); err != nil {
		return "", err
	}
	if output.Stdout.Truncated {
		return "", &outputTooLarge{Limit: machineLimit}
	}
	if !utf8.Valid(output.Stdout.Bytes) {
		return "", errors.New("Machine output is not valid UTF-8")
	}
	return string(output.Stdout.Bytes), nil
}

// RunText is RunMachine for output a reader is shown rather than a program parses, as visible renders it instead of
// failing on bytes that are not UTF-8. It returns at most limit bytes of that text (limit is at most the diagnostic
// capture limit), and complete reports whether that was all of it.
func RunText(ctx context.Context, binary string, args []string, cwd string, seconds uint64, env []string, limit int) (text string, complete bool, err error) {
	output, err := CaptureHost(ctx, binary, args, cwd, env, seconds, CaptureDiagnostic)
	if err != nil {
		return "", false, err
	}
	if err := ensureSuccess(binary, output); err != nil {
		return "", false, err
	}
	text, complete = visible(output.Stdout.Bytes, min(limit, diagnosticLimit))
	return text, complete && !output.Stdout.Truncated, nil
}

// visible renders raw output as text in which nothing reads differently than it is: a reader, or a language model,
// sees every line break that is one and no character it cannot see. Tab, newline and CR before newline stay as they
// are. Every other control character (C0, DEL and C1, so NUL, a lone CR, VT, FF and NEL), every format character
// (zero-width, bidirectional and tag characters, U+FEFF among them), U+2028, U+2029, variation selectors and the
// other default-ignorable characters show as ⟦U+XXXX⟧, and each byte that is not UTF-8 as ⟦xNN⟧. A literal ⟦ shows
// as ⟦U+27E6⟧, so every ⟦ in the text opens an escape. It returns at most limit bytes, cut between characters or
// escapes, and complete reports whether that was all of raw.
func visible(raw []byte, limit int) (text string, complete bool) {
	var out strings.Builder
	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		piece := raw[i : i+size]
		switch {
		case ' ' <= r && r < 0x7f, r == '\t', r == '\n', r == '\r' && i+1 < len(raw) && raw[i+1] == '\n':
		case r == utf8.RuneError && size == 1:
			piece = fmt.Appendf(nil, "⟦x%02X⟧", raw[i])
		case r == '⟦' || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point):
			piece = fmt.Appendf(nil, "⟦U+%04X⟧", r)
		}
		if out.Len()+len(piece) > limit {
			return out.String(), false
		}
		out.Write(piece)
		i += size
	}
	return out.String(), true
}

// RunPredicate runs binary on this host with the Command environment plus env and reads its exit as an answer: a
// success is true, one of falseCodes is false, and any other end, a signal or a failure to start is an error.
func RunPredicate(ctx context.Context, binary string, args []string, cwd string, seconds uint64, falseCodes []int, env []string) (bool, error) {
	output, err := CaptureHost(ctx, binary, args, cwd, env, seconds, CaptureDiagnostic)
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
