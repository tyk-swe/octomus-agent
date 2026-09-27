package process

import "testing"

// TestSignalStringNamesLinuxSignals pins the searchable names status text
// carries for every standard Linux signal, including the Linux-only and
// aliased ones, and that unknown or real-time numbers carry no name.
func TestSignalStringNamesLinuxSignals(t *testing.T) {
	for signal, want := range map[int]string{
		1:  " (SIGHUP)",
		2:  " (SIGINT)",
		6:  " (SIGABRT)",
		9:  " (SIGKILL)",
		13: " (SIGPIPE)",
		15: " (SIGTERM)",
		16: " (SIGSTKFLT)",
		17: " (SIGCHLD)",
		19: " (SIGSTOP)",
		29: " (SIGIO)",
		30: " (SIGPWR)",
		31: " (SIGSYS)",
		0:  "",
		32: "",
		64: "",
		-1: "",
	} {
		if got := signalString(signal); got != want {
			t.Errorf("signalString(%d) = %q; want %q", signal, got, want)
		}
	}
}
