package process_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

const (
	captureSecretEnv  = "CAPTURE_TEST_API_KEY"
	captureSecret     = "s3cr3tValue-0123456789"
	capturePhraseEnv  = "CAPTURE_TEST_PASSWORD"
	capturePhrase     = "correct horse battery staple"
	captureLinesEnv   = "CAPTURE_TEST_SECRET"
	captureLines      = "first-line-of-key\nsecond-line-of-key\nthird-line"
	captureBearerEnv  = "CAPTURE_TEST_BEARER_SECRET"
	captureBearer     = "opaque value then tokenbearer"
	captureMarkEnv    = "CAPTURE_TEST_MARK_PASSWORD"
	captureMark       = "ends with a mark !"
	captureSplitEnv   = "CAPTURE_TEST_SPLIT_SECRET"
	captureSplit      = "ghp_abcdefghijklmnop\nsensitive-suffix"
	captureReverseEnv = "CAPTURE_TEST_REVERSE_SECRET"
	captureReverse    = "sensitive-prefix\nghp_abcdefghijklmnop"
)

func TestMain(m *testing.M) {
	for name, value := range map[string]string{captureSecretEnv: captureSecret, capturePhraseEnv: capturePhrase, captureLinesEnv: captureLines, captureBearerEnv: captureBearer, captureMarkEnv: captureMark, captureSplitEnv: captureSplit, captureReverseEnv: captureReverse} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

func waitForPid(t *testing.T, path string) string {
	t.Helper()
	var pid string
	if !testutil.WaitUntil(5*time.Second, func() bool {
		data, err := os.ReadFile(path)
		if err != nil || !strings.HasSuffix(string(data), "\n") {
			return false
		}
		pid = strings.TrimSpace(string(data))
		_, err = strconv.Atoi(pid)
		return err == nil
	}) {
		t.Fatalf("the command never wrote %s", filepath.Base(path))
	}
	return pid
}

func processReaped(pid string) bool {
	_, err := os.ReadFile("/proc/" + pid + "/stat")
	return errors.Is(err, os.ErrNotExist)
}

func cleanupGroup(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	})
}

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
	if !testutil.WaitUntil(5*time.Second, func() bool { return testutil.ProcessGone(pid) }) {
		t.Fatal("descendant survived leader completion")
	}
	leaderData, err := os.ReadFile(filepath.Join(temp, "group.pid"))
	if err != nil {
		t.Fatalf("leader never wrote its pid: %v", err)
	}
	if !processReaped(strings.TrimSpace(string(leaderData))) {
		t.Error("leader's proc entry survived capture; the child was not waited")
	}
}

func TestInheritedStdoutZeroExit(t *testing.T) {
	t.Parallel()
	inheritedPipe(t, "stdout", 0)
}

func TestCancellationKillsTheCommandProcessGroup(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := process.RunMachine(ctx, "bash",
			[]string{"-c", "sleep 30 & echo $! > child.pid; wait"}, temp, 10)
		done <- err
	}()
	pid := waitForPid(t, filepath.Join(temp, "child.pid"))
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancellation must fail the command")
	}
	if !testutil.WaitUntil(time.Second, func() bool { return testutil.ProcessGone(pid) }) {
		t.Fatal("Child process survived cancellation")
	}
}

func TestDeadlineExpirationKillsTheProcessGroup(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := process.Capture(context.Background(), "bash",
			[]string{"-c", "sleep 30 & echo $! > child.pid; wait"}, temp, 1, process.CaptureDiagnostic)
		done <- err
	}()
	pid := waitForPid(t, filepath.Join(temp, "child.pid"))
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Command timed out") {
			t.Fatalf("capture error = %v; want Command timed out", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not return after its deadline")
	}
	if !testutil.WaitUntil(time.Second, func() bool { return testutil.ProcessGone(pid) }) {
		t.Fatal("Child process survived deadline expiration")
	}
}

func TestChildEnvironmentIsScrubbed(t *testing.T) {
	temp := t.TempDir()
	t.Setenv(redact.TokenEnv, "test-token-value-that-must-not-leak")
	t.Setenv(redact.WebhookEnv, "https://example.invalid/hook")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	script := fmt.Sprintf(`printf 't=%%s w=%%s g=%%s' "${%s-unset}" "${%s-unset}" "$GIT_TERMINAL_PROMPT"`,
		redact.TokenEnv, redact.WebhookEnv)
	out, err := process.RunMachine(context.Background(), "bash", []string{"-c", script}, temp, 10)
	if err != nil {
		t.Fatal(err)
	}
	if out != "t=unset w=unset g=0" {
		t.Fatalf("child environment = %q; want secrets removed and GIT_TERMINAL_PROMPT=0", out)
	}
}

func TestMachineCaptureFailsClosedOnAnyCommandFailure(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	ctx := context.Background()
	if _, err := process.RunMachine(ctx, "python3",
		[]string{"-c", "import sys; sys.exit(1)"}, tmp, 10); err == nil {
		t.Fatal("exit 1 with empty stdout must fail")
	}
	if _, err := process.RunMachine(ctx, "python3",
		[]string{"-c", "import json, sys; print(json.dumps({'ok': True})); sys.exit(7)"},
		tmp, 10); err == nil {
		t.Fatal("exit 7 after valid JSON must fail")
	}
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

func TestPredicateCommandsInterpretOnlyDocumentedFalseStatuses(t *testing.T) {
	t.Parallel()
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
	if _, err := process.RunPredicate(ctx, "python3",
		[]string{"-c", "import sys; sys.exit(2)"}, tmp, 10, []int{1}); err == nil {
		t.Fatal("undocumented status must be an error")
	}
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

func TestFailureTextNeverShowsASecretTheCaptureLimitCut(t *testing.T) {
	t.Parallel()
	const kept = "KEPT-LINE\n"
	for _, secret := range []struct {
		name  string
		print string
		cut   int
		leak  string
	}{
		{name: "environment secret", print: `"$` + captureSecretEnv + `"`, cut: 10, leak: captureSecret[:10]},
		{name: "URL credential", print: `'https://bot:s3cr3tpassword0123@github.com/x'`, cut: 22, leak: "s3cr3tpass"},
	} {
		lines := fmt.Sprintf(`echo KEPT-LINE; head -c %d /dev/zero | tr '\0' A; printf '%%s\n' %s`,
			process.DiagnosticLimit-len(kept)-secret.cut, secret.print)
		for _, stream := range []struct {
			name string
			run  func(script string) error
		}{
			{name: "predicate stdout", run: func(script string) error {
				_, err := process.RunPredicate(context.Background(), "bash", []string{"-c", script}, t.TempDir(), 10, []int{1})
				return err
			}},
			{name: "machine stderr", run: func(script string) error {
				_, err := process.RunMachine(context.Background(), "bash", []string{"-c", "{ " + script + "; } >&2"}, t.TempDir(), 10)
				return err
			}},
		} {
			t.Run(secret.name+" on "+stream.name, func(t *testing.T) {
				err := stream.run(lines + "; exit 3")
				if err == nil {
					t.Fatal("exit 3 must fail")
				}
				text := err.Error()
				if strings.Contains(text, secret.leak) {
					t.Fatalf("failure text shows the cut secret prefix %q: ...%q", secret.leak, text[max(len(text)-80, 0):])
				}
				if !strings.Contains(text, "KEPT-LINE\n[diagnostic output truncated]") {
					t.Fatalf("failure text lost the complete line or the truncation flag: ...%q", text[max(len(text)-80, 0):])
				}
			})
		}
	}
}
