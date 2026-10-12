package runner

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

const cleanupBudget = 30 * time.Second

const stderrTailLimit = 2048

// stderrTail is reported only on connect failures and never persisted, so no raw transcript is kept.
type stderrTail struct {
	mu   sync.Mutex
	data []byte
	cut  bool
}

func (t *stderrTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if extra := len(t.data) - stderrTailLimit; extra > 0 {
		t.data = append(t.data[:0], t.data[extra:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *stderrTail) explain(err error) error {
	t.mu.Lock()
	text, cut := string(t.data), t.cut
	t.mu.Unlock()
	text = strings.ToValidUTF8(strings.TrimRightFunc(text, unicode.IsSpace), "\uFFFD")
	if cut {
		text = redact.Fragment(text, redact.TailTwoWordsCut)
	} else {
		text = redact.Text(text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return err
	}
	return fmt.Errorf("%w; stderr: %s", err, text)
}

func discardStdout(lines <-chan lineResult, stdout io.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range lines {
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	return done
}

// exitErr turns a runner child's end into the error its owner joins: nil for a clean exit.
func exitErr(status process.Status, err error) error {
	if err != nil {
		return err
	}
	return status.Err()
}

// connectFailed keeps how a runner that could not connect ended, such as a sandbox that never started, beside the
// connect error, which often saw only its stream go away.
func connectFailed(err, closeErr error) error {
	if closeErr == nil {
		return err
	}
	return fmt.Errorf("%w: %w", err, closeErr)
}

// setupHint names where a runner that cannot start must be fixed.
func setupHint(box sandbox.Backend, action string) string {
	if box.Mode() == sandbox.ModeDocker {
		return "check the sandbox image and the runner login volume"
	}
	return action + " on this host"
}

func killed(err error) bool {
	if errors.Is(err, process.ErrKilled) {
		return true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// joinOwned joins the two owners of one released runner child within cleanupBudget: the child's exit and the end of
// its output stream.
func joinOwned(waitCh <-chan error, readerDone <-chan struct{}, stuck string) error {
	return joinOwnedWithin(cleanupBudget, waitCh, readerDone, stuck)
}

// joinOwnedWithin joins a released child within budget. A child that still holds its process group or its sandbox
// stream when that budget ends did not end in a way Octomus can confirm, so the child's owner reports the sandbox's
// own failure rather than a plain error: it refuses to start replacement work on an unsettled child, and the task is
// recorded as a runner failure instead of an unclassified one.
func joinOwnedWithin(budget time.Duration, waitCh <-chan error, readerDone <-chan struct{}, stuck string) error {
	var errs error
	timer := time.NewTimer(budget)
	defer timer.Stop()
	for waitCh != nil || readerDone != nil {
		select {
		case err := <-waitCh:
			if err != nil && !killed(err) {
				errs = errors.Join(errs, err)
			}
			waitCh = nil
		case <-readerDone:
			readerDone = nil
		case <-timer.C:
			return errors.Join(errs, &sandbox.Error{Err: errors.New(stuck), Started: true})
		}
	}
	return errs
}
