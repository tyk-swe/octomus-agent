// Package testutil holds the polling and process checks that the Go tests of
// several packages share. Like runnertest it is imported only by tests. It
// depends on the standard library alone, so the tests of any package, the
// store included, can import it without an import cycle.
package testutil

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"time"
)

// WaitUntil calls cond every 10 ms until it returns true, and reports whether
// it did. It gives up only when a call returns false after timeout has
// elapsed, so a condition that holds by the deadline is never reported false.
func WaitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ProcessGone reports whether pid has exited: its /proc entry is gone because
// it was reaped, or it is a zombie its parent has not reaped yet. The state is
// the first field after the last ')', because the command name before it is
// arbitrary text and may itself contain ") Z".
func ProcessGone(pid string) bool {
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		// ESRCH: the process was reaped between opening and reading.
		return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
	}
	text := string(stat)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(text[end+1:])
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X")
}
