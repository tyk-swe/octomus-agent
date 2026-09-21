package process_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/process"
)

// waitUntil polls ready every 10 ms until it holds or timeout elapses.
func waitUntil(timeout time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ready() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processGone reports whether pid no longer runs: the proc entry is gone or it
// is a zombie awaiting its parent's reap.
func processGone(pid string) bool {
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] == "Z"
}

// processReaped reports whether a waited child left no trace at all.
func processReaped(pid string) bool {
	_, err := os.ReadFile("/proc/" + pid + "/stat")
	return errors.Is(err, os.ErrNotExist)
}

// cleanupGroup mirrors FixtureGroup in tests/process_lifecycle.rs: kill the
// recorded group independently of capture's cleanup, even when an assertion
// failed first.
func cleanupGroup(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	})
}

// inheritedPipe ports the four descendant-holds-a-pipe scenarios from
// tests/process_lifecycle.rs: a forked child outlives its leader while holding
// one captured pipe, and capture still finishes with complete output.
func inheritedPipe(t *testing.T, pipe string, exitCode int) {
	t.Helper()
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("requires /proc")
	}
	temp := t.TempDir()
	cleanupGroup(t, filepath.Join(temp, "group.pid"))
	script := `
import os, pathlib, sys, time
pathlib.Path('group.pid').write_text(str(os.getpid()))
ready_read, ready_write = os.pipe()
pid = os.fork()
if pid == 0:
    os.close(ready_read)
    os.close(2 if sys.argv[1] == 'stdout' else 1)
    os.write(ready_write, b'ready')
    os.close(ready_write)
    time.sleep(60)
    os._exit(0)
os.close(ready_write)
assert os.read(ready_read, 5) == b'ready'
os.close(ready_read)
pathlib.Path('child.pid').write_text(str(pid))
sys.stdout.write('o' * 131072 + 'stdout end\n')
sys.stdout.flush()
sys.stderr.write('e' * 131072 + 'stderr end\n')
sys.stderr.flush()
sys.exit(int(sys.argv[2]))
`
	// The descendant outlives this generous deadline unless group cleanup runs.
	type result struct {
		out *process.ProcessOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := process.Capture(context.Background(), "python3",
			[]string{"-c", script, pipe, strconv.Itoa(exitCode)},
			temp, 10, process.CaptureDiagnostic)
		done <- result{out, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("capture exceeded its outer deadline")
	}
	if res.err != nil {
		t.Fatalf("completed leader must not time out on descendant-held pipes: %v", res.err)
	}
	if code, ok := res.out.Status.Code(); !ok || code != exitCode {
		t.Fatalf("status code = %v, %v; want %d", code, ok, exitCode)
	}
	if got := string(res.out.Stdout.Bytes); got != strings.Repeat("o", 131072)+"stdout end\n" {
		t.Fatalf("stdout = %d bytes; want the complete parent output", len(got))
	}
	if got := string(res.out.Stderr.Bytes); got != strings.Repeat("e", 131072)+"stderr end\n" {
		t.Fatalf("stderr = %d bytes; want the complete parent output", len(got))
	}
	if res.out.Stdout.Truncated || res.out.Stderr.Truncated {
		t.Fatal("parent output below the diagnostic limit must not be marked truncated")
	}
	childData, err := os.ReadFile(filepath.Join(temp, "child.pid"))
	if err != nil {
		t.Fatalf("descendant never wrote its pid: %v", err)
	}
	pid := strings.TrimSpace(string(childData))
	if !waitUntil(5*time.Second, func() bool { return processGone(pid) }) {
		t.Fatal("descendant survived leader completion")
	}
	// The leader was successfully started, so it must have been reaped: its
	// proc entry is gone entirely rather than lingering as a zombie.
	leaderData, err := os.ReadFile(filepath.Join(temp, "group.pid"))
	if err != nil {
		t.Fatalf("leader never wrote its pid: %v", err)
	}
	if !processReaped(strings.TrimSpace(string(leaderData))) {
		t.Error("leader's proc entry survived capture; the child was not waited")
	}
}

func TestInheritedStdoutZeroExit(t *testing.T)    { inheritedPipe(t, "stdout", 0) }
func TestInheritedStdoutNonzeroExit(t *testing.T) { inheritedPipe(t, "stdout", 23) }
func TestInheritedStderrZeroExit(t *testing.T)    { inheritedPipe(t, "stderr", 0) }
func TestInheritedStderrNonzeroExit(t *testing.T) { inheritedPipe(t, "stderr", 23) }

// TestCancellationKillsTheCommandProcessGroup ports the core.rs case: a shell
// running a background sleeper dies together with that sleeper once the owner
// cancels.
func TestCancellationKillsTheCommandProcessGroup(t *testing.T) {
	temp := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := process.Run(ctx, "bash",
			[]string{"-c", "sleep 30 & echo $! > child.pid; wait"}, temp, 10)
		done <- err
	}()
	pidFile := filepath.Join(temp, "child.pid")
	if !waitUntil(time.Second, func() bool {
		_, err := os.Stat(pidFile)
		return err == nil
	}) {
		t.Fatal("the command did not start its child process")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation must fail the command")
	}
	if !waitUntil(time.Second, func() bool { return processGone(pid) }) {
		t.Fatal("Child process survived cancellation")
	}
}

// TestDeadlineExpirationKillsTheProcessGroup covers the other early-return
// path: when the command's own deadline elapses, its group is terminated and
// the error reports the elapsed deadline.
func TestDeadlineExpirationKillsTheProcessGroup(t *testing.T) {
	temp := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := process.Capture(context.Background(), "bash",
			[]string{"-c", "sleep 30 & echo $! > child.pid; wait"}, temp, 1, process.CaptureDiagnostic)
		done <- err
	}()
	pidFile := filepath.Join(temp, "child.pid")
	if !waitUntil(time.Second, func() bool {
		_, err := os.Stat(pidFile)
		return err == nil
	}) {
		t.Fatal("the command did not start its child process")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Command timed out") {
			t.Fatalf("capture error = %v; want Command timed out", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not return after its deadline")
	}
	if !waitUntil(time.Second, func() bool { return processGone(pid) }) {
		t.Fatal("Child process survived deadline expiration")
	}
}

// TestCleanupLeavesUnrelatedProcessesUntouched pins acceptance criterion 8:
// terminating one owned group must not signal processes outside it.
func TestCleanupLeavesUnrelatedProcessesUntouched(t *testing.T) {
	temp := t.TempDir()
	sleeper := exec.Command("sleep", "30")
	sleeper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := process.Capture(ctx, "sleep", []string{"30"}, temp, 10, process.CaptureDiagnostic)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, process.ErrCancelled) {
		t.Fatalf("capture error = %v; want Operation cancelled", err)
	}
	if err := syscall.Kill(sleeper.Process.Pid, 0); err != nil {
		t.Fatalf("unrelated process was killed by group cleanup: %v", err)
	}
}

// TestStartupFailureLeaksNothing covers spawn failures: the command reports
// cleanly and leaves no goroutines or descriptors behind.
func TestStartupFailureLeaksNothing(t *testing.T) {
	temp := t.TempDir()
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("requires /proc")
	}
	fds := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	// Warm up several times so lazy global state and transient descriptors are
	// counted in the baseline; a leak only ever grows the count.
	for i := 0; i < 3; i++ {
		_, _ = process.Capture(context.Background(), "octomus-no-such-binary", nil, temp, 10, process.CaptureDiagnostic)
		_, _ = process.Run(context.Background(), "true", nil, temp, 10)
	}
	runtime.GC()
	beforeFDs, beforeG := fds(), runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		_, err := process.Capture(context.Background(), "octomus-no-such-binary",
			nil, temp, 10, process.CaptureDiagnostic)
		if err == nil || !strings.Contains(err.Error(), "Could not start octomus-no-such-binary") {
			t.Fatalf("spawn error = %v; want Could not start", err)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := process.Run(context.Background(), "true", nil, temp, 10); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	if got := fds(); got > beforeFDs {
		t.Errorf("fd count grew from %d to %d across failed and successful captures", beforeFDs, got)
	}
	if got := runtime.NumGoroutine(); got > beforeG+2 {
		t.Errorf("goroutine count grew from %d to %d across captures", beforeG, got)
	}
}

// TestChildEnvironmentIsScrubbed pins acceptance criterion 6 plus the Git
// terminal-prompt guard.
func TestChildEnvironmentIsScrubbed(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("OCTOMUS_TOKEN", "test-token-value-that-must-not-leak")
	t.Setenv("OCTOMUS_NOTIFICATION_WEBHOOK_URL", "https://example.invalid/hook")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	out, err := process.Run(context.Background(), "bash", []string{"-c",
		`printf 't=%s w=%s g=%s' "${OCTOMUS_TOKEN-unset}" "${OCTOMUS_NOTIFICATION_WEBHOOK_URL-unset}" "$GIT_TERMINAL_PROMPT"`},
		temp, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out != "t=unset w=unset g=0" {
		t.Fatalf("child environment = %q; want secrets removed and GIT_TERMINAL_PROMPT=0", out)
	}
}

// TestMachineCaptureNeverCorruptsSuccessfulJSON ports the hardening.rs case:
// large valid output parses, oversized or non-UTF-8 output fails explicitly,
// and diagnostic capture truncates at the documented limit.
func TestMachineCaptureNeverCorruptsSuccessfulJSON(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	text, err := process.RunMachine(ctx, "python3",
		[]string{"-c", "import json; print(json.dumps({'body': 'x' * 402790}))"},
		tmp, 10)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("machine output corrupted: %v", err)
	}
	_, err = process.RunMachine(ctx, "python3",
		[]string{"-c", "import sys; sys.stdout.write('x' * (16 * 1024 * 1024 + 1))"},
		tmp, 10)
	var tooLarge *process.OutputTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("oversized output error = %v; want OutputTooLarge", err)
	}
	if _, err := process.RunMachine(ctx, "python3",
		[]string{"-c", "import sys; sys.stdout.buffer.write(bytes([255]))"},
		tmp, 10); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 error = %v; want a UTF-8 failure", err)
	}
	out, err := process.Capture(ctx, "python3",
		[]string{"-c", "print('x' * 300000)"}, tmp, 10, process.CaptureDiagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Status.Success() || !out.Stdout.Truncated {
		t.Fatal("diagnostic capture must succeed with truncation flagged")
	}
	if len(out.Stdout.Bytes) != process.DiagnosticLimit {
		t.Fatalf("kept %d bytes; want exactly the %d-byte diagnostic limit",
			len(out.Stdout.Bytes), process.DiagnosticLimit)
	}
}

// TestMachineCaptureFailsClosedOnAnyCommandFailure ports the hardening.rs
// case: nonzero status and signal termination are failures regardless of what
// was written to stdout.
func TestMachineCaptureFailsClosedOnAnyCommandFailure(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	// Empty stdout plus exit 1 is a failed command, not a successful empty result.
	if _, err := process.RunMachine(ctx, "python3",
		[]string{"-c", "import sys; sys.exit(1)"}, tmp, 10); err == nil {
		t.Fatal("exit 1 with empty stdout must fail")
	}
	// Well-formed JSON on stdout does not rescue a nonzero status.
	if _, err := process.RunMachine(ctx, "python3",
		[]string{"-c", "import json, sys; print(json.dumps({'ok': True})); sys.exit(7)"},
		tmp, 10); err == nil {
		t.Fatal("exit 7 after valid JSON must fail")
	}
	// Signal termination cannot read as a successful capture either.
	_, err := process.RunMachine(ctx, "python3",
		[]string{"-c", "import os, signal; os.kill(os.getpid(), signal.SIGKILL)"},
		tmp, 10)
	if err == nil {
		t.Fatal("SIGKILL termination must fail")
	}
	if !strings.Contains(err.Error(), "signal: 9 (SIGKILL)") {
		t.Fatalf("signal error = %v; want the terminated status", err)
	}
}

// TestPredicateCommandsInterpretOnlyDocumentedFalseStatuses ports the
// hardening.rs case: exit 0 is true, a documented false status is false, and
// every other outcome — unexpected status, signal, spawn failure — is an error.
func TestPredicateCommandsInterpretOnlyDocumentedFalseStatuses(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	if ok, err := process.RunPredicate(ctx, "python3",
		[]string{"-c", "print('true')"}, tmp, 10, []int{1}); err != nil || !ok {
		t.Fatalf("exit 0 predicate = %v, %v; want true", ok, err)
	}
	if ok, err := process.RunPredicate(ctx, "python3",
		[]string{"-c", "import sys; sys.exit(1)"}, tmp, 10, []int{1}); err != nil || ok {
		t.Fatalf("documented false status = %v, %v; want false", ok, err)
	}
	// An unexpected status is a command failure, never a false predicate.
	if _, err := process.RunPredicate(ctx, "python3",
		[]string{"-c", "import sys; sys.exit(2)"}, tmp, 10, []int{1}); err == nil {
		t.Fatal("undocumented status must be an error")
	}
	// Signal termination has no code, so it cannot match the false list.
	if _, err := process.RunPredicate(ctx, "python3",
		[]string{"-c", "import os, signal; os.kill(os.getpid(), signal.SIGKILL)"},
		tmp, 10, []int{1, 9}); err == nil {
		t.Fatal("signal termination must be an error, not a false predicate")
	}
	if _, err := process.RunPredicate(ctx, "octomus-no-such-binary",
		nil, tmp, 10, []int{1}); err == nil {
		t.Fatal("spawn failure must be an error, not a false predicate")
	}
	if _, err := process.RunPredicate(ctx, "sleep", []string{"30"},
		tmp, 1, []int{1}); err == nil {
		t.Fatal("timeout must be an error, not a false predicate")
	}
}

// TestDiagnosticTextAndStatusFormat pins the human-readable evidence contract:
// bounded stdout, an appended [stderr] section, and Rust ExitStatus wording.
func TestDiagnosticTextAndStatusFormat(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	out, err := process.Capture(ctx, "bash",
		[]string{"-c", "printf 'o'; printf 'e' >&2"}, tmp, 10, process.CaptureDiagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if text, err := process.DiagnosticText("bash", out); err != nil || text != "o\n[stderr]\ne" {
		t.Fatalf("diagnostic text = %q, %v; want stdout plus a stderr section", text, err)
	}
	out, err = process.Capture(ctx, "bash",
		[]string{"-c", "printf 'o'; printf 'e' >&2; exit 3"}, tmp, 10, process.CaptureDiagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.DiagnosticText("bash", out); err == nil ||
		!strings.HasPrefix(err.Error(), "bash exited with exit status: 3: o") {
		t.Fatalf("failure text = %v; want the Rust status wording", err)
	}
	out, err = process.Capture(ctx, "python3",
		[]string{"-c", "import os, signal; os.kill(os.getpid(), signal.SIGKILL)"},
		tmp, 10, process.CaptureDiagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Status.String(); got != "signal: 9 (SIGKILL)" {
		t.Fatalf("status = %q; want signal: 9 (SIGKILL)", got)
	}
}

// TestShellCheckRetainsBashPipefail pins the one place a shell remains:
// operator-configured verification runs through `bash -o pipefail -c`, so a
// failing pipeline member fails the check even when the last stage succeeds.
func TestShellCheckRetainsBashPipefail(t *testing.T) {
	tmp := t.TempDir()
	ctx := context.Background()
	failed, err := process.ShellCheck(ctx, "echo ok | false", tmp, 10)
	if err != nil || failed.Status.Success() {
		t.Fatalf("pipefail must fail a pipeline whose middle stage fails: %v, %v", failed, err)
	}
	passed, err := process.ShellCheck(ctx, "echo ok | tr o O", tmp, 10)
	if err != nil || !passed.Status.Success() {
		t.Fatalf("passing check = %v, %v", passed, err)
	}
	if text := strings.TrimSpace(passed.Stdout.Preview()); text != "Ok" {
		t.Fatalf("check output = %q; want the filtered output", text)
	}
}

// TestWithDeadline ports the deadline helper: expiry cancels the context and
// distinguishes a genuine deadline from an already-cancelled session.
func TestWithDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fresh := process.WithDeadline(ctx, cancel, 50*time.Millisecond, func() string {
		<-ctx.Done()
		return "unwound"
	})
	if !fresh.Expired || fresh.AlreadyCancelled {
		t.Fatalf("deadline = %+v; want expired and not already cancelled", fresh)
	}
	done := process.WithDeadline(context.Background(), context.CancelFunc(func() {}),
		time.Minute, func() string { return "finished" })
	if done.Expired || done.Output != "finished" {
		t.Fatalf("completed work = %+v; want Done with the output", done)
	}
	// When the context is already cancelled but the work still outlives the
	// limit, expiry reports already_cancelled rather than a genuine deadline.
	cancelledCtx, cancelCancelled := context.WithCancel(context.Background())
	cancelCancelled()
	cancelled := process.WithDeadline(cancelledCtx, cancelCancelled,
		50*time.Millisecond, func() string {
			time.Sleep(200 * time.Millisecond)
			return "unwound"
		})
	if !cancelled.Expired || !cancelled.AlreadyCancelled {
		t.Fatalf("cancelled deadline = %+v; want expired and already cancelled", cancelled)
	}
}

// TestBoundedKeepsCallersWording covers the bounded helper: caller-supplied
// timeout text, session cancellation distinct from expiry, and pass-through
// results.
func TestBoundedKeepsCallersWording(t *testing.T) {
	ctx := context.Background()
	cleaned := make(chan struct{})
	_, err := process.BoundedAt(ctx, time.Now().Add(20*time.Millisecond), "Check timed out", func(workCtx context.Context) (int, error) {
		<-workCtx.Done()
		time.Sleep(20 * time.Millisecond) // cleanup must finish before BoundedAt returns
		close(cleaned)
		return 0, workCtx.Err()
	})
	if err == nil || err.Error() != "Check timed out: deadline has elapsed" {
		t.Fatalf("timeout = %v; want the caller's wording plus the elapsed cause", err)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("timed-out callback was not cancelled and joined")
	}
	cancelling, cancelNow := context.WithCancel(ctx)
	defer cancelNow()
	cleaned = make(chan struct{})
	_, err = process.Bounded(cancelling, 60, "Check timed out", func(workCtx context.Context) (int, error) {
		cancelNow()
		<-workCtx.Done()
		time.Sleep(20 * time.Millisecond)
		close(cleaned)
		return 0, workCtx.Err()
	})
	if !errors.Is(err, process.ErrSessionCancelled) {
		t.Fatalf("cancellation = %v; want Session cancelled", err)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("cancelled callback was not joined")
	}
	value, err := process.Bounded(ctx, 60, "Check timed out", func(context.Context) (int, error) {
		return 42, nil
	})
	if err != nil || value != 42 {
		t.Fatalf("completed bounded = %v, %v; want the inner result", value, err)
	}
}
