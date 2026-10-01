// Package wire is the contract between the control plane and the sandbox broker: the request, the framed stream, the
// broker's info document, the runner and verification programs and the owned-root layout both sides agree on. It
// starts nothing.
package wire

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// UpgradeProtocol names the framed stream a broker switches to after accepting a sandbox request. One stream carries
// one sandbox for its whole life: when it closes, the broker kills and removes the container.
const UpgradeProtocol = "octomus-sandbox/1"

// Request.Kind names what a sandbox runs.
const (
	KindRunner = "runner"
	KindVerify = "verify"
	KindProbe  = "probe"
)

const (
	FrameStdin    byte = 1
	FrameStdinEOF byte = 2
	FrameStdout   byte = 3
	FrameStderr   byte = 4
	FrameExit     byte = 5
	FrameSignal   byte = 6
)

const (
	// maxFrame bounds any single frame either side accepts.
	maxFrame = 1 << 20
	// dataChunk is the largest payload a data frame is written with.
	dataChunk = 32 << 10
)

const (
	SignalTerminate = "TERM"
	SignalKill      = "KILL"
)

// Request is everything the control plane may ask of the broker. The broker derives the image, program, mounts,
// network, limits and environment from it and from its own configuration.
type Request struct {
	Kind    string   `json:"kind"`
	Dir     string   `json:"dir"`
	Runner  string   `json:"runner"`
	Mode    string   `json:"mode"`
	Command string   `json:"command"`
	Env     []string `json:"env"`
	Stdin   bool     `json:"stdin"`
	// FreshHome asks for an empty verification home, at the first command of a verification run.
	FreshHome bool   `json:"fresh_home"`
	Timeout   uint64 `json:"timeout_seconds"`
	// Readiness bounds an OpenCode server's startup inside the sandbox.
	Readiness uint64 `json:"readiness_seconds"`
}

const (
	RunnerModeStdio    = "stdio"
	RunnerModeOpenCode = "opencode"
	ProbeVersions      = "versions"
	ProbeContainment   = "containment"
)

// ExitReport is the broker's account of how a sandbox ended.
type ExitReport struct {
	Code    int                  `json:"code"`
	OOM     bool                 `json:"oom"`
	Killed  bool                 `json:"killed"`
	Error   string               `json:"error,omitempty"`
	Sandbox *model.SandboxRecord `json:"sandbox,omitempty"`
}

var errFrameTooLarge = errors.New("Sandbox stream frame exceeds its size limit")

func ReadFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	size := binary.BigEndian.Uint32(header[1:])
	if size > maxFrame {
		return 0, nil, errFrameTooLarge
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return header[0], payload, nil
}

// FrameWriter serializes frames from several writers onto one stream.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

func (f *FrameWriter) Frame(kind byte, payload []byte) error {
	if len(payload) > maxFrame {
		return errFrameTooLarge
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = kind
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.w.Write(buf)
	return err
}

// Data writes p as one or more data frames and reports how much was written.
func (f *FrameWriter) Data(kind byte, p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), dataChunk)
		if err := f.Frame(kind, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}
