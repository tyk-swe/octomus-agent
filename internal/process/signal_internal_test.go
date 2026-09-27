package process

import "testing"

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
