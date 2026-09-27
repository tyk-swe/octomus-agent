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

	"github.com/tyk-swe/octomus-agent/internal/redact"
)

const cleanupBudget = 30 * time.Second

const stderrWaitDelay = 2 * time.Second

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

func drained(lines <-chan lineResult) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range lines {
		}
	}()
	return done
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

func killed(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func joinOwned(waitCh <-chan error, readerDone <-chan struct{}, stuck string) error {
	var errs error
	timer := time.NewTimer(cleanupBudget)
	defer timer.Stop()
	for waitCh != nil || readerDone != nil {
		select {
		case err := <-waitCh:
			if err != nil && !killed(err) && !errors.Is(err, exec.ErrWaitDelay) {
				errs = errors.Join(errs, err)
			}
			waitCh = nil
		case <-readerDone:
			readerDone = nil
		case <-timer.C:
			return errors.Join(errs, errors.New(stuck))
		}
	}
	return errs
}
