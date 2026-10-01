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
	"github.com/tyk-swe/octomus-agent/internal/model"
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
	// FreshHome gives a verification sandbox an empty home: set on the first command of each verification run.
	FreshHome bool
	// Timeout is a hard limit in seconds, beyond the caller's own graceful one. The Docker backend's broker enforces it,
	// capped at and defaulting (zero) to its maximum; the host backend sets no limit beyond the caller's own.
	Timeout uint64
	// Probe names the KindProbe check to run.
	Probe string
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

// SandboxError reports that the sandbox, not the program in it, failed: the broker refused the child, lost its
// stream, or could not confirm how it ended. Callers must not record it as the program's own result.
type SandboxError struct{ Err error }

func (e *SandboxError) Error() string { return e.Err.Error() }
func (e *SandboxError) Unwrap() error { return e.Err }

// Infrastructure reports whether err means the sandbox failed rather than the program it ran.
func Infrastructure(err error) bool {
	var started *StartError
	var failed *SandboxError
	return errors.As(err, &started) || errors.As(err, &failed)
}

// Backend starts sandboxed children. Implementations never fall back to one another.
type Backend interface {
	Mode() Mode
	Start(ctx context.Context, spec Spec) (Child, error)
	StartOpenCode(ctx context.Context, spec Spec, readinessSeconds uint64) (*OpenCodeServer, error)
	// RunnerVersion reports the runner's version. The host backend runs its --version, bounded by seconds; the Docker
	// backend runs nothing and reports the version its broker probed in the current sandbox image.
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

// EvidenceOf returns what the backend recorded about a finished child, if it records anything.
func EvidenceOf(child Child) *model.SandboxRecord {
	if evidenced, ok := child.(interface{ Evidence() *model.SandboxRecord }); ok {
		return evidenced.Evidence()
	}
	return nil
}

// verifyGrace lets a verification command's own timeout and graceful termination act before the backend's hard limit.
const verifyGrace = 60

// Verify starts one verification command in the backend's verify sandbox and bounds it like any captured command.
// fresh starts the verification run's home empty.
func Verify(ctx context.Context, backend Backend, dir, command string, seconds uint64, fresh bool) (*process.ProcessOutput, *model.SandboxRecord, error) {
	if ctx.Err() != nil {
		return nil, nil, process.ErrCancelled
	}
	child, err := backend.Start(ctx, Spec{Kind: KindVerify, Dir: dir, Command: command, FreshHome: fresh, Timeout: seconds + verifyGrace})
	if err != nil {
		return nil, nil, err
	}
	out, err := process.CaptureStarted(ctx, child, child.Stdout(), child.Stderr(), seconds, process.CaptureDiagnostic)
	return out, EvidenceOf(child), err
}
