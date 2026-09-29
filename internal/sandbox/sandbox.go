// Package sandbox is where every untrusted child starts: runner sessions, verification commands and containment
// probes. The orchestrator's own git and gh commands never pass through it.
package sandbox

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
)

// Kind names what a sandbox runs. The backend, not the caller, chooses the program for each kind.
type Kind uint8

const (
	KindRunner Kind = iota + 1
	KindVerify
	KindProbe
)

func (k Kind) String() string {
	switch k {
	case KindRunner:
		return "runner"
	case KindVerify:
		return "verify"
	case KindProbe:
		return "probe"
	}
	return "unknown"
}

// Spec is everything a caller may choose about a sandboxed child.
type Spec struct {
	Kind Kind
	// Dir is the working directory: an owned root's workspace, identical inside and outside a container.
	Dir string
	// Runner selects Codex or OpenCode for KindRunner.
	Runner config.Backend
	// Binary is the configured runner program. Only the host backend honours it; containers run their image's runner.
	Binary string
	// Command is the KindVerify shell command.
	Command string
	// Env adds runner policy variables (KEY=VALUE) such as OpenCode's unattended configuration.
	Env []string
	// Stdin keeps a writable stdin open, for Codex's JSON-RPC.
	Stdin bool
	// Stderr, when set, receives the child's stderr instead of Child.Stderr, so a runner can keep a diagnostic tail.
	Stderr io.Writer
}

// Root is the owned root directory a workspace belongs to.
func (s Spec) Root() string { return filepath.Dir(s.Dir) }

// DeadlineWriter is a child's stdin: a pipe on the host, a framed stream for a container.
type DeadlineWriter interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

// Child is one started sandboxed program. Wait reports how it ended; Kill stops everything it started.
type Child interface {
	process.Proc
	Stdin() DeadlineWriter
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
}

// OpenCodeServer is a started OpenCode server whose HTTP API is reachable only through Transport.
type OpenCodeServer struct {
	Base      string
	Transport http.RoundTripper
	Child     Child
	// Drained closes once the server's remaining stdout has been consumed.
	Drained <-chan struct{}
}

// StartError reports that a child never started, as opposed to one that started and then failed.
type StartError struct{ Err error }

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Backend starts sandboxed children. Implementations never fall back to one another.
type Backend interface {
	Mode() Mode
	Start(ctx context.Context, spec Spec) (Child, error)
	StartOpenCode(ctx context.Context, spec Spec, readinessSeconds uint64) (*OpenCodeServer, error)
	RunnerVersion(ctx context.Context, spec Spec, seconds uint64) (string, error)
}

// Mode is how untrusted children are isolated.
type Mode uint8

const (
	ModeOff Mode = iota + 1
	ModeDocker
)

func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeDocker:
		return "docker"
	}
	return "unknown"
}

func ParseMode(value string) (Mode, error) {
	switch value {
	case "off":
		return ModeOff, nil
	case "docker":
		return ModeDocker, nil
	}
	return 0, errors.New("Sandbox must be docker or off")
}

// Verify starts one verification command in the backend's verify sandbox and bounds it like any captured command.
func Verify(ctx context.Context, backend Backend, dir, command string, seconds uint64) (*process.ProcessOutput, error) {
	if ctx.Err() != nil {
		return nil, process.ErrCancelled
	}
	child, err := backend.Start(ctx, Spec{Kind: KindVerify, Dir: dir, Command: command})
	if err != nil {
		return nil, err
	}
	return process.CaptureStarted(ctx, child, child.Stdout(), child.Stderr(), seconds, process.CaptureDiagnostic)
}
