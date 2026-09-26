package runner

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/process"
)

// startOwned starts an owned command and delivers its wait result.
func startOwned(t *testing.T, args ...string) (*process.GroupChild, chan error) {
	t.Helper()
	cmd := process.Command(args[0], t.TempDir())
	cmd.Args = append(cmd.Args, args[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := process.NewGroupChild(cmd)
	t.Cleanup(child.Close)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	return child, waitCh
}

func waitResult(t *testing.T, waitCh chan error) error {
	t.Helper()
	select {
	case err := <-waitCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the owned child was not reaped")
		return nil
	}
}

// Only a SIGKILL exit is the expected end of a killed child; other signals,
// exit codes and look-alike texts are not.
func TestKilledClassifiesOnlySIGKILL(t *testing.T) {
	child, waitCh := startOwned(t, "sleep", "5")
	child.Close()
	if err := waitResult(t, waitCh); !killed(err) {
		t.Fatalf("a SIGKILL exit must be expected: %v", err)
	}
	_, waitCh = startOwned(t, "sh", "-c", "exit 3")
	if err := waitResult(t, waitCh); err == nil || killed(err) {
		t.Fatalf("an exit status is not a kill: %v", err)
	}
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err == nil || killed(err) {
		t.Fatalf("another signal is not a kill: %v", err)
	}
	if killed(nil) || killed(errors.New("signal: killed")) {
		t.Fatal("only a wait status can report a kill")
	}
}

// joinOwned waits for both the child and its reader, hides the expected kill
// and reports any other exit.
func TestJoinOwnedReportsUnexpectedExits(t *testing.T) {
	child, waitCh := startOwned(t, "sleep", "5")
	reader := make(chan struct{})
	joined := make(chan error, 1)
	go func() { joined <- joinOwned(waitCh, reader, "stuck") }()
	child.Close()
	select {
	case err := <-joined:
		t.Fatalf("join returned before the reader finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(reader)
	select {
	case err := <-joined:
		if err != nil {
			t.Fatalf("a killed child must join cleanly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("join did not finish")
	}

	_, waitCh = startOwned(t, "sh", "-c", "exit 3")
	closed := make(chan struct{})
	close(closed)
	if err := joinOwned(waitCh, closed, "stuck"); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("an unexpected exit must be reported: %v", err)
	}
}
