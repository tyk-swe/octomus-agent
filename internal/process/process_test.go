package process_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

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
func TestInheritedStdoutNonzeroExit(t *testing.T) {
	t.Parallel()
	inheritedPipe(t, "stdout", 23)
}
func TestInheritedStderrZeroExit(t *testing.T) {
	t.Parallel()
	inheritedPipe(t, "stderr", 0)
}
func TestInheritedStderrNonzeroExit(t *testing.T) {
	t.Parallel()
	inheritedPipe(t, "stderr", 23)
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

func TestStoppedGroupsCanCleanUp(t *testing.T) {
	t.Parallel()
	script := "trap 'touch cleaned; exit 1' TERM; touch started; sleep 30 & wait"
	for _, tc := range []struct {
		name    string
		seconds uint64
		cancel  bool
		want    string
	}{
		{name: "cancellation", seconds: 30, cancel: true, want: "Operation cancelled"},
		{name: "deadline", seconds: 3, want: "Command timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			temp := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := process.Capture(ctx, "bash", []string{"-c", script}, temp, tc.seconds, process.CaptureDiagnostic)
				done <- err
			}()
			if !testutil.WaitUntil(5*time.Second, func() bool {
				_, err := os.Stat(filepath.Join(temp, "started"))
				return err == nil
			}) {
				t.Fatal("the command did not start")
			}
			if tc.cancel {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("capture error = %v; want %s", err, tc.want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("capture did not return")
			}
			if _, err := os.Stat(filepath.Join(temp, "cleaned")); err != nil {
				t.Fatalf("the command's TERM cleanup never ran: %v", err)
			}
		})
	}
}

func TestTermIgnoringGroupIsStillKilled(t *testing.T) {
	t.Parallel()
	temp := t.TempDir()
	cleanupGroup(t, filepath.Join(temp, "leader.pid"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := process.Capture(ctx, "bash", []string{"-c",
			"trap '' TERM; echo $$ > leader.pid; sleep 30 & echo $! > child.pid; wait"},
			temp, 30, process.CaptureDiagnostic)
		done <- err
	}()
	leader := waitForPid(t, filepath.Join(temp, "leader.pid"))
	child := waitForPid(t, filepath.Join(temp, "child.pid"))
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, process.ErrCancelled) {
			t.Fatalf("capture error = %v; want Operation cancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a TERM-ignoring group must still be killed after the grace")
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("cancellation took %v; want the short grace then a kill", elapsed)
	}
	for _, pid := range []string{leader, child} {
		if !testutil.WaitUntil(time.Second, func() bool { return testutil.ProcessGone(pid) }) {
			t.Fatalf("process %s survived cancellation", pid)
		}
	}
}

func TestCleanupLeavesUnrelatedProcessesUntouched(t *testing.T) {
	t.Parallel()
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
	for i := 0; i < 3; i++ {
		_, _ = process.Capture(context.Background(), "octomus-no-such-binary", nil, temp, 10, process.CaptureDiagnostic)
		_, _ = process.RunMachine(context.Background(), "true", nil, temp, 10)
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
		if _, err := process.RunMachine(context.Background(), "true", nil, temp, 10); err != nil {
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

func TestChildEnvironmentDropsGitRepositoryLocation(t *testing.T) {
	temp := t.TempDir()
	located := []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_COMMON_DIR",
		"GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE", "GIT_NO_REPLACE_OBJECTS",
		"GIT_REPLACE_REF_BASE",
	}
	for _, key := range located {
		t.Setenv(key, filepath.Join(temp, "decoy", strings.ToLower(key)))
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Kept Operator")
	script := `for key in "$@"; do printf '%s=%s ' "$key" "${!key-unset}"; done`
	out, err := process.RunMachine(context.Background(), "bash",
		append([]string{"-c", script, "env"}, append(located, "GIT_CONFIG_COUNT")...), temp, 10)
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, key := range located {
		want.WriteString(key + "=unset ")
	}
	want.WriteString("GIT_CONFIG_COUNT=1 ")
	if out != want.String() {
		t.Fatalf("child environment = %q; want %q", out, want.String())
	}
	if _, err := process.RunMachine(context.Background(), "git", []string{"init", "--quiet"}, temp, 10); err != nil {
		t.Fatalf("git init under exported GIT_DIR: %v", err)
	}
	if _, err := os.Stat(filepath.Join(temp, ".git", "HEAD")); err != nil {
		t.Fatalf("git init did not create the repository in its working directory: %v", err)
	}
	name, err := process.RunMachine(context.Background(), "git", []string{"config", "user.name"}, temp, 10)
	if err != nil || strings.TrimSpace(name) != "Kept Operator" {
		t.Fatalf("git config user.name = %q, %v; want the GIT_CONFIG_* value", name, err)
	}
}

func TestMachineCaptureNeverCorruptsSuccessfulJSON(t *testing.T) {
	t.Parallel()
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

func TestSignalStatusFormat(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	ctx := context.Background()
	out, err := process.Capture(ctx, "python3",
		[]string{"-c", "import os, signal; os.kill(os.getpid(), signal.SIGKILL)"},
		tmp, 10, process.CaptureDiagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Status.String(); got != "signal: 9 (SIGKILL)" {
		t.Fatalf("status = %q; want signal: 9 (SIGKILL)", got)
	}
}

func TestCapturedSafeTextDropsThePartialLineACaptureCut(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		bytes     string
		truncated bool
		want      string
	}{
		{name: "complete capture", bytes: "line one\npartial https://bot:s3cr3t", want: "line one\npartial https://bot:s3cr3t"},
		{name: "cut inside a line", bytes: "line one\nline two\nkey s3cr3tVal", truncated: true, want: "line one\nline two"},
		{name: "cut after a newline", bytes: "line one\n", truncated: true, want: "line one"},
		{name: "one line with words", bytes: "word one\u2003https://bot:s3cr3tpass", truncated: true, want: "word one"},
		{name: "one unbroken token", bytes: "https://bot:s3cr3tpass", truncated: true, want: ""},
		{name: "cut inside a character", bytes: "done\nnext \xe2\x82", truncated: true, want: "done"},
		{name: "cut inside a passphrase", bytes: "PASS=correct horse batt", truncated: true, want: "PASS="},
		{name: "cut inside a multi-line key", bytes: "done\nKEY=first-line-of-key\nsecond-line-of-key\nthi", truncated: true, want: "done\nKEY="},
		{name: "complete passphrase", bytes: "PASS=correct horse battery staple", want: "PASS=[redacted]"},
		{name: "invalid bytes kept whole", bytes: "bad \xff", want: "bad \uFFFD"},
	} {
		t.Run(test.name, func(t *testing.T) {
			captured := process.Captured{Bytes: []byte(test.bytes), Truncated: test.truncated}
			if got := captured.SafeText(); got != test.want {
				t.Fatalf("SafeText() = %q; want %q", got, test.want)
			}
			want := test.want
			if test.truncated {
				want += "\n[diagnostic output truncated]"
			}
			if got := captured.SafePreview(); got != want {
				t.Fatalf("SafePreview() = %q; want %q", got, want)
			}
		})
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

func TestFailureTextNeverShowsTheFirstWordsOrLinesOfACutSecret(t *testing.T) {
	t.Parallel()
	const assignment = "KEY="
	for _, secret := range []struct {
		name string
		env  string
		cut  int
		leak string
	}{
		{name: "passphrase", env: capturePhraseEnv, cut: len("correct horse batt"), leak: "correct"},
		{name: "multi-line key", env: captureLinesEnv, cut: len("first-line-of-key\nsecond-"), leak: "first-line-of-key"},
	} {
		script := fmt.Sprintf(`head -c %d /dev/zero | tr '\0' A; printf '%s%%s\n' "$%s"`,
			process.DiagnosticLimit-len(assignment)-secret.cut, assignment, secret.env)
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
				err := stream.run(script + "; exit 3")
				if err == nil {
					t.Fatal("exit 3 must fail")
				}
				text := err.Error()
				if strings.Contains(text, secret.leak) {
					t.Fatalf("failure text shows the cut secret's first part %q: ...%q", secret.leak, text[max(len(text)-80, 0):])
				}
				if !strings.HasSuffix(text, "A"+assignment+"\n[diagnostic output truncated]") {
					t.Fatalf("failure text lost the text before the secret or the truncation flag: ...%q", text[max(len(text)-80, 0):])
				}
			})
		}
	}
}

func TestTailTextNeverShowsASecretTheWindowCut(t *testing.T) {
	t.Parallel()
	for _, secret := range []struct {
		name  string
		print string
		text  string
		cut   int
		leak  string
	}{
		{name: "environment secret", print: `"$` + captureSecretEnv + `"`, text: captureSecret, cut: 10, leak: captureSecret[10:]},
		{name: "URL credential", print: `'https://bot:s3cr3tpassword0123@github.com/x'`, text: "https://bot:s3cr3tpassword0123@github.com/x", cut: 22, leak: "sword0123"},
		{name: "bearer token", print: `'Authorization: Bearer abcdefghijklmnop'`, text: "Authorization: Bearer abcdefghijklmnop", cut: len("Authorization: Bea"), leak: "abcdefghijklmnop"},
		{name: "bearer token after a line break", print: `'Authorization: Bearer\nabcdefghijklmnop'`, text: "Authorization: Bearer\nabcdefghijklmnop", cut: len("Authorization: Be"), leak: "abcdefghijklmnop"},
		{name: "passphrase", print: `"$` + capturePhraseEnv + `"`, text: capturePhrase, cut: len("correct hor"), leak: "battery"},
		{name: "URL credential after a passphrase", print: `"$` + capturePhraseEnv + `://bot:s3cr3tpassword0123@github.com/x"`, text: capturePhrase + "://bot:s3cr3tpassword0123@github.com/x", cut: len("correct hor"), leak: "sword0123"},
		{name: "multi-line key", print: `"$` + captureLinesEnv + `"`, text: captureLines, cut: len("first-line-of-k"), leak: "second-line-of-key"},
	} {
		for _, layout := range []struct{ name, sep string }{{"lines", "\n"}, {"one line", " "}} {
			filler := process.TailLimit - (len(secret.text) - secret.cut) - len("KEPT-AFTER") - len("FINAL") - 4*len(layout.sep)
			script := fmt.Sprintf(`head -c %d /dev/zero | tr '\0' A; echo; printf %s; printf '%%sKEPT-AFTER%%s' '%s' '%s'; head -c %d /dev/zero | tr '\0' B; printf '%%sFINAL%%s' '%s' '%s'`,
				process.DiagnosticLimit+1000, secret.print, layout.sep, layout.sep, filler, layout.sep, layout.sep)
			for _, stream := range []struct {
				name   string
				script string
				pick   func(*process.ProcessOutput) process.Captured
			}{
				{name: "stdout", script: script, pick: func(out *process.ProcessOutput) process.Captured { return out.Stdout }},
				{name: "stderr", script: "{ " + script + "; } >&2", pick: func(out *process.ProcessOutput) process.Captured { return out.Stderr }},
			} {
				t.Run(secret.name+" in "+layout.name+" on "+stream.name, func(t *testing.T) {
					out, err := process.Capture(context.Background(), "bash", []string{"-c", stream.script}, t.TempDir(), 10, process.CaptureDiagnostic)
					if err != nil {
						t.Fatal(err)
					}
					captured := stream.pick(out)
					tail := captured.SafeTailText()
					if !captured.Truncated || len(captured.Bytes) != process.DiagnosticLimit || len(tail) > process.TailLimit {
						t.Fatalf("capture kept %d head bytes and %d tail bytes, truncated=%t", len(captured.Bytes), len(tail), captured.Truncated)
					}
					if strings.Contains(tail, secret.leak) {
						t.Fatalf("tail text shows the cut secret's rest %q: %.80q", secret.leak, tail)
					}
					if !strings.HasPrefix(tail, "KEPT-AFTER"+layout.sep) || !strings.HasSuffix(tail, layout.sep+"FINAL"+layout.sep) {
						t.Fatalf("tail text lost what followed the secret or the real end: %.80q ... %q", tail, tail[max(len(tail)-80, 0):])
					}
				})
			}
		}
	}
}

const (
	displayLimit     = 16384
	failureTextLimit = displayLimit - 1024
)

func TestFailureTextFitsWhole(t *testing.T) {
	t.Parallel()
	_, err := process.RunMachine(context.Background(), "bash",
		[]string{"-c", "printf o; printf e >&2; exit 3"}, t.TempDir(), 10)
	if err == nil || err.Error() != "bash exited with exit status: 3: o\ne" {
		t.Fatalf("failure text = %v; want the unchanged small-failure form", err)
	}
}

func TestFailureTextRedactsCredentialsSplitAcrossStreams(t *testing.T) {
	t.Parallel()
	const token = "opaque-cross-stream-credential"
	for _, tc := range []struct {
		name    string
		repeats int
	}{
		{name: "small", repeats: 1},
		{name: "over display limit before redaction", repeats: 1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := process.RunMachine(context.Background(), "bash",
				[]string{"-c", `printf 'Authorization: Bearer '; printf '%s' "$1" >&2; exit 3`, "--", strings.Repeat(token, tc.repeats)},
				t.TempDir(), 10)
			if err == nil {
				t.Fatal("exit 3 must fail")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("failure text leaked a credential split across stdout and stderr")
			}
			if err.Error() != "bash exited with exit status: 3: Authorization: [redacted]" {
				t.Fatal("failure text must preserve the failure context and redact the combined credential")
			}
		})
	}
}

func TestFailureTextRedactsOverlappingSecretsAcrossStreams(t *testing.T) {
	t.Parallel()
	for _, secret := range []struct{ name, value string }{
		{"token in stdout", captureSplit},
		{"token in stderr", captureReverse},
	} {
		for _, size := range []struct {
			name  string
			lines int
		}{
			{"small", 0},
			{"over display limit", 4000},
			{"truncated stdout", 40000},
		} {
			t.Run(secret.name+"/"+size.name, func(t *testing.T) {
				stdout, stderr, _ := strings.Cut(secret.value, "\n")
				_, err := process.RunPredicate(context.Background(), "bash",
					[]string{"-c", `if (( $3 > 0 )); then seq -f 'context %g' "$3"; fi; printf '%s' "$1"; printf '%s\nSTDERR-END' "$2" >&2; exit 3`, "--", stdout, stderr, strconv.Itoa(size.lines)},
					t.TempDir(), 10, nil)
				if err == nil {
					t.Fatal("exit 3 must fail")
				}
				text := err.Error()
				if strings.Contains(text, stdout) || strings.Contains(text, stderr) {
					t.Fatal("failure text leaked part of an environment secret overlapping a token across streams")
				}
				if !strings.HasPrefix(text, "bash exited with exit status: 3: ") || !strings.Contains(text, "[redacted]") || !strings.HasSuffix(text, "STDERR-END") {
					t.Fatal("failure text must preserve the exit status and stderr context while redacting the overlapping secret")
				}
				if utf8.RuneCountInString(text) > failureTextLimit {
					t.Fatal("failure text exceeded its display budget")
				}
			})
		}
	}
}

func TestFailureTextKeepsStderrAndStdoutEnds(t *testing.T) {
	t.Parallel()
	script := `echo STDOUT-HEAD
for i in $(seq 800); do echo "page $i token ghp_abcdefghijklmnopqrstuvwxyz0123456789 filler filler"; done
echo STDOUT-TAIL
echo 'gh: API rate limit exceeded (HTTP 403)' >&2
exit 1`
	_, err := process.RunMachine(context.Background(), "bash", []string{"-c", script}, t.TempDir(), 10)
	if err == nil {
		t.Fatal("exit 1 must fail")
	}
	text := err.Error()
	for _, want := range []string{"bash exited with exit status: 1: STDOUT-HEAD", "STDOUT-TAIL", "characters omitted", "[stderr]\ngh: API rate limit exceeded (HTTP 403)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("failure text lacks %q:\n%.300s", want, text)
		}
	}
	if strings.Contains(text, "ghp_") {
		t.Fatal("failure text leaked a token")
	}
	if n := utf8.RuneCountInString(text); n > failureTextLimit {
		t.Fatalf("failure text has %d characters; want at most %d", n, failureTextLimit)
	}
	if redact.Error(err) != text {
		t.Fatal("recording the failure text must not shorten it further")
	}
	for _, wrapped := range []error{
		fmt.Errorf("Open pull request inventory failed: %w", err),
		fmt.Errorf("Repository remote preflight failed: %w",
			fmt.Errorf("%w: %w", errors.New("Publication result is uncertain; reconcile the preserved output commit"), err)),
	} {
		if recorded := redact.Error(wrapped); !strings.HasSuffix(recorded, "[stderr]\ngh: API rate limit exceeded (HTTP 403)\n") {
			t.Fatalf("recorded wrapped failure lost the stderr cause: ...%q", recorded[max(len(recorded)-120, 0):])
		}
	}
}

func TestFailureTextBoundsLargeStderr(t *testing.T) {
	t.Parallel()
	script := `echo STDOUT-HEAD; head -c 40000 /dev/zero | tr '\0' o; echo; echo STDOUT-TAIL
{ echo STDERR-HEAD; head -c 40000 /dev/zero | tr '\0' e; echo; echo STDERR-TAIL; } >&2
exit 2`
	_, err := process.RunMachine(context.Background(), "bash", []string{"-c", script}, t.TempDir(), 10)
	if err == nil {
		t.Fatal("exit 2 must fail")
	}
	text := err.Error()
	stdout, stderr, ok := strings.Cut(text, "\n[stderr]\n")
	if !ok {
		t.Fatalf("failure text lacks a stderr section:\n%.200s", text)
	}
	for _, want := range []string{"STDOUT-HEAD", "STDOUT-TAIL", "characters omitted"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout section lacks %q", want)
		}
	}
	for _, want := range []string{"STDERR-HEAD", "STDERR-TAIL", "characters omitted"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr section lacks %q", want)
		}
	}
	if n := utf8.RuneCountInString(stderr); n < 4000 || n > 4096 {
		t.Fatalf("stderr section has %d characters; want its fixed share", n)
	}
	if n := utf8.RuneCountInString(text); n > failureTextLimit || n < failureTextLimit-64 {
		t.Fatalf("failure text has %d characters; want the bound used, not exceeded", n)
	}
}

func TestFailureTextOmitsAnEmptyStderrSection(t *testing.T) {
	t.Parallel()
	script := `echo STDOUT-HEAD; head -c 40000 /dev/zero | tr '\0' o; echo; echo STDOUT-TAIL; exit 4`
	_, err := process.RunMachine(context.Background(), "bash", []string{"-c", script}, t.TempDir(), 10)
	if err == nil {
		t.Fatal("exit 4 must fail")
	}
	text := err.Error()
	if strings.Contains(text, "[stderr]") {
		t.Fatalf("failure text carries an empty stderr section: %.200s", text)
	}
	if !strings.HasPrefix(text, "bash exited with exit status: 4: STDOUT-HEAD") ||
		!strings.Contains(text, "characters omitted") || !strings.HasSuffix(text, "STDOUT-TAIL\n") {
		t.Fatalf("failure text lost an end of stdout: %.200s", text)
	}
	if n := utf8.RuneCountInString(text); n > failureTextLimit {
		t.Fatalf("failure text has %d characters; want at most %d", n, failureTextLimit)
	}
}

func TestFailureTextKeepsTheRealEndOfTruncatedStreams(t *testing.T) {
	t.Parallel()
	script := `echo FIRST-STDOUT; seq -f 'stdout filler line %g' 40000; echo FINAL-STDOUT
{ echo FIRST-STDERR; seq -f 'stderr filler line %g' 40000; echo FINAL-STDERR; } >&2
exit 3`
	_, err := process.RunPredicate(context.Background(), "bash", []string{"-c", script}, t.TempDir(), 10, []int{1})
	if err == nil {
		t.Fatal("exit 3 must fail")
	}
	text := err.Error()
	stdout, stderr, ok := strings.Cut(text, "\n[stderr]\n")
	if !ok {
		t.Fatalf("failure text lacks a stderr section:\n%.200s", text)
	}
	for _, section := range []struct{ name, text, first, final string }{
		{name: "stdout", text: stdout, first: "bash exited with exit status: 3: FIRST-STDOUT\n", final: "\nFINAL-STDOUT\n"},
		{name: "stderr", text: stderr, first: "FIRST-STDERR\n", final: "\nFINAL-STDERR\n"},
	} {
		if !strings.HasPrefix(section.text, section.first) || !strings.HasSuffix(section.text, section.final) ||
			!strings.Contains(section.text, "\n[diagnostic output truncated]\n") {
			t.Fatalf("%s section lost its beginning, its real end or the truncation marker: %.80q ... %q",
				section.name, section.text, section.text[max(len(section.text)-80, 0):])
		}
	}
	if n := utf8.RuneCountInString(text); n > failureTextLimit {
		t.Fatalf("failure text has %d characters; want at most %d", n, failureTextLimit)
	}
}

func TestShellCheckRetainsBashPipefail(t *testing.T) {
	t.Parallel()
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
	if text := strings.TrimSpace(passed.Stdout.SafePreview()); text != "Ok" {
		t.Fatalf("check output = %q; want the filtered output", text)
	}
}

func TestWithDeadline(t *testing.T) {
	t.Parallel()
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
		t.Fatalf("completed work = %+v; want its output without expiry", done)
	}
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

func TestBoundedKeepsCallersWording(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cleaned := make(chan struct{})
	_, err := process.BoundedAt(ctx, time.Now().Add(20*time.Millisecond), "Check timed out", func(workCtx context.Context) (int, error) {
		<-workCtx.Done()
		time.Sleep(20 * time.Millisecond)
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
