package runner

// Shared cleanup for the owned Codex app-server and OpenCode server children.
import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// cleanupBudget bounds how long an owner waits for its killed child and the
// child's stdout reader to finish.
const cleanupBudget = 30 * time.Second

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
// its stdout reader. A SIGKILL exit is expected; any other exit error is
// returned, and stuck is joined in when the budget runs out. Each channel is
// consumed once and then ignored, because a closed channel fires repeatedly.
func joinOwned(waitCh <-chan error, readerDone <-chan struct{}, stuck string) error {
	var errs error
	timer := time.NewTimer(cleanupBudget)
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
			return errors.Join(errs, errors.New(stuck))
		}
	}
	return errs
}
