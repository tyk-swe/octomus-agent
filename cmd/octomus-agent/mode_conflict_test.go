package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrintConfigCannotSilentlySkipDoctor(t *testing.T) {
	for _, args := range [][]string{
		{"--doctor", "--print-config"},
		{"--print-config", "--doctor"},
		{"--doctor", "--audit", "--print-config"},
		{"--print-config", "--doctor", "--audit"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "unused-state")
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"--data-dir", data}, args...), func(string) (string, bool) { return "", false }, &stdout, &stderr)
			if code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--doctor cannot be used with --print-config") {
				t.Fatalf("conflicting diagnostic modes: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatalf("conflicting modes touched application state: %v", err)
			}
		})
	}
}
