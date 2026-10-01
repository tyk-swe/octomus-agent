package wire

import (
	"bytes"
	"testing"
)

func FuzzReadFrame(f *testing.F) {
	var seed bytes.Buffer
	_ = NewFrameWriter(&seed).Frame(FrameStdout, []byte("hello"))
	f.Add(seed.Bytes())
	f.Add([]byte{FrameExit, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		kind, payload, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			return
		}
		if len(payload) > maxFrame || len(data) < 5+len(payload) {
			t.Fatalf("frame %d with %d bytes from %d input bytes", kind, len(payload), len(data))
		}
		var again bytes.Buffer
		if err := NewFrameWriter(&again).Frame(kind, payload); err != nil || !bytes.Equal(again.Bytes(), data[:5+len(payload)]) {
			t.Fatal("frame does not round-trip")
		}
	})
}
