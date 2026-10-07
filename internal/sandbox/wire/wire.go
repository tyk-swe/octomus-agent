// Package wire is the contract between the control plane and the sandbox broker: the request, the framed stream, the
// broker's info document, the runner and verification programs, the owned-root layout and the literals both sides
// repeat. It starts nothing.
package wire

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// ProbeTargetEnv carries the gateway-selected refusal target into the containment helper.
const ProbeTargetEnv = "OCTOMUS_EGRESS_PROBE_TARGET"

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidLabel reports whether label is one lowercase DNS label.
func ValidLabel(label string) bool { return hostLabel.MatchString(label) }

// NormalizeHost lowercases a DNS name and refuses anything that is not one, including IP literals: an allowlist
// names services, never addresses.
func NormalizeHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || len(host) > 253 {
		return "", errors.New("host name length")
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return "", errors.New("IP literal")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("single-label host")
	}
	for _, label := range labels {
		if !ValidLabel(label) {
			return "", errors.New("invalid host name")
		}
	}
	return host, nil
}

// ValidProbeTarget accepts only a canonical DNS name on HTTPS's port. Invalid names and addresses would exercise a
// different gateway boundary and cannot stand in for the unlisted-host check.
func ValidProbeTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	if err != nil || port != "443" {
		return false
	}
	normalized, err := NormalizeHost(host)
	return err == nil && normalized == host
}

// UpgradeProtocol names the framed stream a broker switches to after accepting a sandbox request. One stream carries
// one sandbox for its whole life: when it closes, the broker kills and removes the container.
const UpgradeProtocol = "octomus-sandbox/1"

// SocketEnv names the broker's socket for both the broker and the control plane; DefaultSocket is used when it is
// unset.
const (
	SocketEnv     = "OCTOMUS_SANDBOXD_SOCKET"
	DefaultSocket = "/run/octomus/sandboxd.sock"
)

// The broker's two endpoints: its info document, and the request that upgrades to a sandbox stream.
const (
	InfoPath      = "/v1/info"
	SandboxesPath = "/v1/sandboxes"
)

// InitFlag is the first argument of the in-sandbox helper (octomus-agent --sandbox-init), which the broker starts for
// the OpenCode bridge and the probes.
const InitFlag = "--sandbox-init"

// TimeLimitReason is the broker's report of a sandbox it killed at its time limit. Running too long is the program's
// own result, as a timeout is; any other error an exit report carries means the broker failed the sandbox.
const TimeLimitReason = "Sandbox time limit reached"

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

// BrokerInfo is what a sandbox broker reports about the isolation it enforces.
type BrokerInfo struct {
	InstanceID    string            `json:"instance_id"`
	Version       string            `json:"version"`
	DockerVersion string            `json:"docker_version"`
	APIVersion    string            `json:"api_version"`
	Image         string            `json:"image"`
	ImageID       string            `json:"image_id"`
	ImageDigests  []string          `json:"image_digests"`
	Runtime       string            `json:"runtime"`
	Runners       map[string]string `json:"runners"`
	// RunnerErrors says why each runner that is installed in the image did not report its version, so it is not
	// mistaken for a missing one.
	RunnerErrors map[string]string `json:"runner_errors"`
	Limits       BrokerLimits      `json:"limits"`
	Networks     BrokerNetworks    `json:"networks"`
	Egress       bool              `json:"egress"`
	Gateway      *GatewayPosture   `json:"gateway"`
	Live         int               `json:"live"`
}

// GatewayPosture identifies the live gateway process and its immutable effective allowlists. A missing gateway
// report when egress is enabled means that a saved containment proof cannot be treated as current.
type GatewayPosture struct {
	InstanceID        string `json:"instance_id"`
	PolicyFingerprint string `json:"policy_fingerprint"`
}

type BrokerLimits struct {
	NanoCPUs   int64  `json:"nano_cpus"`
	Memory     int64  `json:"memory_bytes"`
	Pids       int64  `json:"pids"`
	Tmpfs      int64  `json:"tmpfs_bytes"`
	Max        int    `json:"max_sandboxes"`
	MaxSeconds uint64 `json:"max_seconds"`
}

type BrokerNetworks struct {
	Runner string `json:"runner"`
	Verify string `json:"verify"`
}

// Request.Runner names a runner, as its program is called inside the sandbox image.
const (
	RunnerCodex    = "codex"
	RunnerOpenCode = "opencode"
)

// Runners are every runner a sandbox can serve.
var Runners = []string{RunnerCodex, RunnerOpenCode}

// VersionFailed joins a runner's name and why its --version failed on a line of the version probe's stderr.
const VersionFailed = " --version failed: "

// RunnerArgs are the fixed arguments each runner is served with, or nil for an unknown runner.
func RunnerArgs(name string) []string {
	switch name {
	case RunnerCodex:
		return []string{"app-server", "--listen", "stdio://"}
	case RunnerOpenCode:
		return []string{"serve", "--hostname", "127.0.0.1", "--port", "0"}
	}
	return nil
}

// VerifyProgram is the program and arguments that run one verification command.
func VerifyProgram(command string) (string, []string) {
	return "bash", []string{"-o", "pipefail", "-c", command}
}

// The egress gateway's refusal reasons the containment probe looks for: a target that is not an allowlisted host
// name, and a host missing from the allowlist of the lease's kind (the format takes the kind).
const (
	RefusalNotHostName    = "target is not an allowlisted host name"
	RefusalNotAllowlisted = "host is not on the %s allowlist"
)

// WorkspaceDir is the work tree inside every owned root: Request.Dir names it, and it is the only directory of the
// root a sandbox may write.
const WorkspaceDir = "workspace"

// RunnerHomeDirs are where the shared runner state appears inside a runner sandbox's home, with the runner volume
// subdirectory each one mounts.
var RunnerHomeDirs = []struct{ Home, Volume string }{
	{".codex", "codex"},
	{".local/share/opencode", "opencode/data"},
	{".config/opencode", "opencode/config"},
	{".cache/opencode", "opencode/cache"},
	{".local/state/opencode", "opencode/state"},
}

const (
	RunnerHome = "home"
	VerifyHome = "verify-home"
)

// HomeDirs are the directories a sandbox of this kind mounts inside its owned root, relative to the root, in the order
// they are created.
func HomeDirs(kind string) []string {
	switch kind {
	case KindRunner:
		dirs := []string{RunnerHome}
		for _, dir := range RunnerHomeDirs {
			dirs = append(dirs, filepath.Join(RunnerHome, dir.Home))
		}
		return dirs
	case KindVerify:
		return []string{VerifyHome}
	}
	return nil
}
