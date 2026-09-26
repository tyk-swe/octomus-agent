package runner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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

	// A clean exit whose stderr a descendant still holds ends the wait with
	// ErrWaitDelay, which is not a failure.
	cmd := process.Command("sh", t.TempDir())
	cmd.Args = append(cmd.Args, "-c", "sleep 5 & exit 0")
	cmd.Stderr = &stderrTail{}
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(process.NewGroupChild(cmd).Close)
	held := make(chan error, 1)
	go func() { held <- cmd.Wait() }()
	err := waitResult(t, held)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("the fixture must end its wait with ErrWaitDelay: %v", err)
	}
	held <- err
	if err := joinOwned(held, closed, "stuck"); err != nil {
		t.Fatalf("a held stderr after a clean exit must join cleanly: %v", err)
	}
}

// A server's stdout stays drained after an over-long line ends the line
// reader, and the drain ends at end of stream or when the owner closes the
// pipe.
func TestDiscardStdoutSurvivesOverlongLine(t *testing.T) {
	for _, end := range []string{"writer-closed", "reader-closed"} {
		t.Run(end, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			done := make(chan struct{})
			defer close(done)
			drain := discardStdout(lineReader(r, 16_384, done), r)
			written := make(chan error, 1)
			go func() {
				if _, err := w.Write([]byte(strings.Repeat("z", 20_000) + "\n")); err != nil {
					written <- err
					return
				}
				_, err := w.Write(bytes.Repeat([]byte("log line\n"), 1<<17))
				written <- err
			}()
			select {
			case err := <-written:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stdout stopped draining after an over-long line")
			}
			if end == "writer-closed" {
				w.Close()
			} else {
				r.Close()
			}
			select {
			case <-drain:
			case <-time.After(5 * time.Second):
				t.Fatal("the drain did not end")
			}
		})
	}
}

// The stderr tail keeps a bounded end of the stream, reports only whole lines
// or words once it was cut, redacts secrets and leaves a silent failure as is.
func TestStderrTailExplainsConnectFailures(t *testing.T) {
	cause := errors.New("connect failed")
	if err := (&stderrTail{}).explain(cause); err != cause {
		t.Fatalf("no stderr must keep the error: %v", err)
	}
	tail := &stderrTail{}
	fmt.Fprint(tail, "  \n\t ")
	if err := tail.explain(cause); err != cause {
		t.Fatalf("blank stderr must keep the error: %v", err)
	}

	tail = &stderrTail{}
	fmt.Fprint(tail, "warming up\nconfig invalid: token ghp_fixtureStartupSecret0001\n")
	err := tail.explain(cause)
	if !errors.Is(err, cause) || err.Error() != "connect failed; stderr: warming up\nconfig invalid: token [redacted]" {
		t.Fatalf("explained error: %q", err)
	}

	// A secret cut at the start of the kept tail is dropped with its line.
	tail = &stderrTail{}
	fmt.Fprint(tail, "token ghp_fixtureStartupSecret0001\n")
	kept := "StartupSecret0001\n"
	rest := strings.Repeat("y", stderrTailLimit-len(kept)-len("\nlast line")) + "\nlast line"
	fmt.Fprint(tail, rest)
	if len(tail.data) != stderrTailLimit || !strings.HasPrefix(string(tail.data), kept) {
		t.Fatalf("fixture math: %d %q", len(tail.data), string(tail.data[:20]))
	}
	if err := tail.explain(cause); err.Error() != "connect failed; stderr: "+rest {
		t.Fatalf("cut tail: %q", err)
	}

	// One long line, terminated or not, keeps only the whole words after its
	// partial first word and the word after that.
	for _, end := range []string{"", "\n"} {
		tail = &stderrTail{}
		fmt.Fprint(tail, "ghp_fixtureStartupSecret0001 "+strings.Repeat("word ", stderrTailLimit/5)+end)
		err = tail.explain(cause)
		if strings.Contains(err.Error(), "Secret") || !strings.HasSuffix(err.Error(), "; stderr: "+strings.TrimSpace(strings.Repeat("word ", (stderrTailLimit-2)/5-1))) {
			t.Fatalf("long line: %q", err)
		}
	}

	// A cut run with no line or word boundary is not reported at all.
	tail = &stderrTail{}
	fmt.Fprint(tail, strings.Repeat("z", 3*stderrTailLimit))
	if err := tail.explain(cause); err != cause {
		t.Fatalf("an unbroken cut run must not be reported: %v", err)
	}
}

// Wherever the cut falls in a bearer header on one long line, including
// inside or just after "Bearer", no part of the token is reported.
func TestStderrTailCutKeepsBearerTokensRedacted(t *testing.T) {
	cause := errors.New("connect failed")
	header := "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
	for at := 1; at < len(header); at++ {
		kept := header[at:]
		filler := stderrTailLimit - len(kept)
		kept += strings.Repeat(" word", filler/5) + strings.Repeat("s", filler%5)
		tail := &stderrTail{}
		fmt.Fprint(tail, header[:at]+kept)
		if len(tail.data) != stderrTailLimit || string(tail.data) != kept {
			t.Fatalf("fixture math at %d: %d", at, len(tail.data))
		}
		err := tail.explain(cause)
		if !errors.Is(err, cause) {
			t.Fatalf("cut at %d lost the cause: %v", at, err)
		}
		_, reported, _ := strings.Cut(err.Error(), "; stderr: ")
		for _, word := range strings.Fields(reported) {
			if word != "[redacted]" && !strings.HasPrefix(word, "word") {
				t.Fatalf("cut at %d reported %q: %q", at, word, err)
			}
		}
	}
}
