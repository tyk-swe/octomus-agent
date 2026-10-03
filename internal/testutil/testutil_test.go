package testutil_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
	"golang.org/x/sys/unix"
)

func TestWaitUntilReportsWhetherTheConditionHeld(t *testing.T) {
	calls := 0
	if !testutil.WaitUntil(5*time.Second, func() bool { calls++; return calls == 3 }) {
		t.Fatal("a condition that became true was reported false")
	}
	if calls != 3 {
		t.Fatalf("condition called %d times after it held; want 3", calls)
	}
	start := time.Now()
	calls = 0
	if testutil.WaitUntil(30*time.Millisecond, func() bool { calls++; return false }) {
		t.Fatal("a condition that never held was reported true")
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond || calls < 2 {
		t.Fatalf("gave up after %v and %d calls; want the whole timeout", elapsed, calls)
	}
}

// Rename the initial thread before testing starts: /proc/PID/stat uses the
// thread-group leader's name, and test goroutines may run on other threads.
// PR_SET_NAME works even when procfs is mounted read-only.
func init() {
	if os.Getenv("OCTOMUS_TEST_PROCESS_GONE_HELPER") != "1" {
		return
	}
	runtime.LockOSThread()
	name := []byte("x) Z 0\x00")
	if err := unix.Prctl(unix.PR_SET_NAME, uintptr(unsafe.Pointer(&name[0])), 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "rename process:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestProcessGoneReadsTheStateAfterTheCommandName(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^$")
	cmd.Env = append(os.Environ(), "OCTOMUS_TEST_PROCESS_GONE_HELPER=1")
	var stderr testutil.SyncBuffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != "ready\n" {
		t.Fatalf("the helper never renamed itself: %q, %v; stderr: %s", ready, err, stderr.String())
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil || !strings.Contains(string(stat), "(x) Z 0) ") {
		t.Fatalf("stat = %q, %v; want the helper's zombie-like command name", stat, err)
	}
	if testutil.ProcessGone(pid) {
		t.Fatal("a running process named like a zombie was reported gone")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if !testutil.WaitUntil(5*time.Second, func() bool { return testutil.ProcessGone(pid) }) {
		t.Fatal("the killed process never became a zombie")
	}
	if _, err := os.Stat("/proc/" + pid); err != nil {
		t.Fatalf("the zombie was reaped before Wait: %v", err)
	}
	_ = cmd.Wait()
	reaped = true
	if !testutil.ProcessGone(pid) {
		t.Fatal("a reaped process was reported running")
	}
}
