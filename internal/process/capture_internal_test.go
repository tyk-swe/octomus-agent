// Redaction of captured streams, including secrets cut across stdout, stderr and retained tails.

package process

import (
	"bytes"
	"testing"
)

func TestSafeCaptures(t *testing.T) {
	t.Parallel()
	const credential = "opaque-cross-stream-credential"
	for _, tc := range []struct {
		name           string
		output         ProcessOutput
		stdout, stderr SafeCapture
	}{
		{
			name:   "complete streams",
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("Authorization: Bearer ")}, Stderr: Captured{Bytes: []byte(credential + "\nSTDERR-END")}},
			stdout: SafeCapture{Head: "Authorization: [redacted]"},
			stderr: SafeCapture{Head: "\nSTDERR-END"},
		},
		{
			name: "retained tails",
			output: ProcessOutput{
				Stdout: Captured{Bytes: []byte("STDOUT-HEAD\ncut"), Truncated: true, tail: []byte("cut\nAuthorization: Bearer ")},
				Stderr: Captured{Bytes: []byte(credential + "\nSTDERR-HEAD\ncut"), Truncated: true, tail: []byte("cut\nSTDERR-END")},
			},
			stdout: SafeCapture{Head: "STDOUT-HEAD", Tail: "Authorization: [redacted]", Truncated: true},
			stderr: SafeCapture{Head: "\nSTDERR-HEAD", Tail: "STDERR-END", Truncated: true},
		},
		{
			name: "overlapping environment secret",
			// TestMain installs the full two-line value as an environment secret.
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("ghp_abcdefghijklmnop")}, Stderr: Captured{Bytes: []byte("sensitive-suffix\nSTDERR-END")}},
			stdout: SafeCapture{Head: "[redacted]"},
			stderr: SafeCapture{Head: "\nSTDERR-END"},
		},
		{
			name:   "token starts stderr after ordinary stdout",
			output: ProcessOutput{Stdout: Captured{Bytes: []byte("progress")}, Stderr: Captured{Bytes: []byte("sk-abcdefghijklmnop\nSTDERR-END")}},
			stdout: SafeCapture{Head: "progress"},
			stderr: SafeCapture{Head: "[redacted]\nSTDERR-END"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeOut := bytes.Clone(tc.output.Stdout.Bytes)
			beforeErr := bytes.Clone(tc.output.Stderr.Bytes)
			stdout, stderr := tc.output.SafeCaptures()
			if stdout != tc.stdout || stderr != tc.stderr {
				t.Fatalf("safe captures = %+v, %+v; want %+v, %+v", stdout, stderr, tc.stdout, tc.stderr)
			}
			if !bytes.Equal(beforeOut, tc.output.Stdout.Bytes) || !bytes.Equal(beforeErr, tc.output.Stderr.Bytes) {
				t.Fatal("redaction modified the original capture")
			}
		})
	}
}
