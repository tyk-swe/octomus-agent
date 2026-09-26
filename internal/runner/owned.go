package runner

// Shared cleanup and startup diagnostics for the owned Codex app-server and
// OpenCode server children.
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

	"github.com/tyk-swe/octomus-agent/internal/store"
)

// cleanupBudget bounds how long an owner waits for its killed child and the
// child's stdout reader to finish.
const cleanupBudget = 30 * time.Second

// stderrWaitDelay bounds how long a child's wait waits for its stderr to close
// after the child exits, in case a descendant still holds it.
const stderrWaitDelay = 2 * time.Second

// stderrTailLimit bounds the stderr bytes kept to explain a connect failure.
const stderrTailLimit = 2048

// stderrTail keeps the last bytes a child wrote to stderr so a connect failure
// can say why. It is reported only on connect failures and never persisted
// otherwise, so no raw transcript is kept.
type stderrTail struct {
	mu   sync.Mutex
	data []byte
	cut  bool
}

// Write keeps the last stderrTailLimit bytes and never fails.
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

// explain appends the redacted stderr tail to a connect failure, keeping err
// in the chain. Call it only after the child's wait has been joined, so the
// tail is complete.
func (t *stderrTail) explain(err error) error {
	t.mu.Lock()
	text, cut := string(t.data), t.cut
	t.mu.Unlock()
	text = store.Redact(strings.ToValidUTF8(strings.TrimRightFunc(text, unicode.IsSpace), "\uFFFD"))
	if cut {
		// The cut can split a secret, or part a bearer token from its
		// prefix, so that redaction no longer recognises what is left.
		// Report only whole lines, or for one long line the words after its
		// partial first word and the word after that.
		if _, rest, found := strings.Cut(text, "\n"); found {
			text = rest
		} else {
			text = afterWord(strings.TrimLeftFunc(afterWord(text), unicode.IsSpace))
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return err
	}
	return fmt.Errorf("%w; stderr: %s", err, text)
}

// afterWord drops text up to its first whitespace, or all of it when there is
// none.
func afterWord(text string) string {
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		return text[i:]
	}
	return ""
}

// drained discards lines until the reader closes them and reports that.
func drained(lines <-chan lineResult) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range lines {
		}
	}()
	return done
}

// discardStdout drains a server's stdout for its whole life without keeping
// it: lines until the line reader stops (end of stream, a read error or an
// over-long line), then raw bytes until the pipe ends. Only one reader is ever
// active, and closing the pipe ends the copy. A server whose stdout is no
// longer read would block on its next write once the pipe fills.
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

// killed reports the expected exit of a child its owner killed with SIGKILL.
func killed(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// joinOwned waits, under the cleanup budget, for an owned child's exit and for
// its stdout reader. A SIGKILL exit is expected, as is a clean exit whose
// stderr a descendant held past stderrWaitDelay; any other exit error is
// returned, and stuck is joined in when the budget runs out. Each channel is
// consumed once and then ignored, because a closed channel fires repeatedly.
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
