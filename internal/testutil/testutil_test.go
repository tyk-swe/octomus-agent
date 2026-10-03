package testutil_test

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

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
