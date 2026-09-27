package testutil_test

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
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

// A live process whose command name mimics a zombie's stat line is still
// running; once killed it is a zombie until reaped, and gone after.
func TestProcessGoneReadsTheStateAfterTheCommandName(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `printf 'x) Z 0' > /proc/self/comm && echo ready && read line`)
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
		t.Fatalf("the shell never renamed itself: %q, %v", ready, err)
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil || !strings.Contains(string(stat), "(x) Z 0) ") {
		t.Fatalf("stat = %q, %v; want the shell's zombie-like command name", stat, err)
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
