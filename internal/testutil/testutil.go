package testutil

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

func ProcessGone(pid string) bool {
	stat, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
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

// SkipVolumeUnderRace skips a single-goroutine data-volume check in the race
// build: the race detector finds nothing there, and race-instrumented SQLite
// slows it dozens of times over. The regular suite still runs the check.
func SkipVolumeUnderRace(t testing.TB) {
	t.Helper()
	if raceEnabled {
		t.Skip("single-goroutine data-volume check; the regular suite runs it")
	}
}
