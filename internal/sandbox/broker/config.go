// Package broker is the only Octomus component that talks to the Docker daemon. It accepts a narrow request from the
// control plane over a unix socket, validates it, and builds every sandbox container itself: the control plane can
// choose a kind of work and an owned root, never an image, mount, capability or network.
package broker

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

type Config struct {
	Socket       string
	DockerSocket string
	Image        string
	DataDir      string
	DataVolume   string
	RunnerVolume string
	// RunnerDir is where the broker mounts the runner volume to prepare its subdirectories.
	RunnerDir   string
	ToolsVolume string
	// ToolsDir is where the broker mounts the tools volume to install its own executable for sandboxes.
	ToolsDir      string
	RunnerNetwork string
	VerifyNetwork string
	// Instance labels every sandbox this broker owns; it never touches a container without it.
	Instance string
	UID      int
	GID      int
	// ClientUID is the only peer uid the broker serves.
	ClientUID   int
	NanoCPUs    int64
	Memory      int64
	Pids        int64
	Tmpfs       int64
	Max         int
	MaxSeconds  uint64
	Runtime     string
	EgressProxy string
	LeaseDir    string
	// EgressCollector is the gateway's local socket for a finished sandbox's egress summary.
	EgressCollector string
	// Log receives what the broker could not do on its own, such as a removal it keeps retrying; nil is stderr.
	Log io.Writer
}

const (
	sandboxHome = "/home/octomus"
	toolsMount  = "/opt/octomus"
	toolsBinary = toolsMount + "/octomus-agent"
	// toolsGitConfig is runner sandboxes' global git configuration, read-only in the tools volume.
	toolsGitConfig = toolsMount + "/gitconfig"
	instanceLabel  = "octomus.sandbox.instance"
	kindLabel      = "octomus.sandbox.kind"
	rootLabel      = "octomus.sandbox.root"
)

var instancePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// LoadConfig reads the broker's deployment settings. Every security-relevant value comes from the host, never from
// a request.
func LoadConfig(getenv func(string) string) (Config, error) {
	value := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		Socket:          value("OCTOMUS_SANDBOXD_SOCKET", wire.DefaultSocket),
		DockerSocket:    value("OCTOMUS_DOCKER_SOCKET", "/var/run/docker.sock"),
		Image:           value("OCTOMUS_SANDBOX_IMAGE", ""),
		DataDir:         value("OCTOMUS_DATA_DIR", ""),
		DataVolume:      value("OCTOMUS_SANDBOX_DATA_VOLUME", ""),
		RunnerVolume:    value("OCTOMUS_SANDBOX_RUNNER_VOLUME", ""),
		RunnerDir:       value("OCTOMUS_SANDBOX_RUNNER_DIR", "/var/lib/octomus/runner"),
		ToolsVolume:     value("OCTOMUS_SANDBOX_TOOLS_VOLUME", ""),
		ToolsDir:        value("OCTOMUS_SANDBOX_TOOLS_DIR", "/opt/octomus-tools"),
		RunnerNetwork:   value("OCTOMUS_SANDBOX_RUNNER_NETWORK", ""),
		VerifyNetwork:   value("OCTOMUS_SANDBOX_VERIFY_NETWORK", ""),
		Instance:        value("OCTOMUS_SANDBOX_INSTANCE", "octomus"),
		Runtime:         value("OCTOMUS_SANDBOX_RUNTIME", ""),
		EgressProxy:     value("OCTOMUS_EGRESS_PROXY", ""),
		LeaseDir:        value("OCTOMUS_EGRESS_LEASES", ""),
		EgressCollector: value("OCTOMUS_EGRESS_COLLECTOR", ""),
	}
	var errs []error
	for key, v := range map[string]string{
		"OCTOMUS_SANDBOX_IMAGE":          c.Image,
		"OCTOMUS_DATA_DIR":               c.DataDir,
		"OCTOMUS_SANDBOX_DATA_VOLUME":    c.DataVolume,
		"OCTOMUS_SANDBOX_RUNNER_VOLUME":  c.RunnerVolume,
		"OCTOMUS_SANDBOX_TOOLS_VOLUME":   c.ToolsVolume,
		"OCTOMUS_SANDBOX_RUNNER_NETWORK": c.RunnerNetwork,
		"OCTOMUS_SANDBOX_VERIFY_NETWORK": c.VerifyNetwork,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
	}
	for key, path := range map[string]string{
		"OCTOMUS_SANDBOXD_SOCKET":    c.Socket,
		"OCTOMUS_DOCKER_SOCKET":      c.DockerSocket,
		"OCTOMUS_DATA_DIR":           c.DataDir,
		"OCTOMUS_SANDBOX_RUNNER_DIR": c.RunnerDir,
		"OCTOMUS_SANDBOX_TOOLS_DIR":  c.ToolsDir,
	} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
			errs = append(errs, fmt.Errorf("%s must be a clean absolute path", key))
		}
	}
	if c.LeaseDir != "" && (!filepath.IsAbs(c.LeaseDir) || filepath.Clean(c.LeaseDir) != c.LeaseDir) {
		errs = append(errs, errors.New("OCTOMUS_EGRESS_LEASES must be a clean absolute path"))
	}
	if (c.LeaseDir == "") != (c.EgressProxy == "") {
		errs = append(errs, errors.New("OCTOMUS_EGRESS_PROXY and OCTOMUS_EGRESS_LEASES are set together"))
	}
	if !instancePattern.MatchString(c.Instance) {
		errs = append(errs, errors.New("OCTOMUS_SANDBOX_INSTANCE must be 1-32 lowercase letters, digits or hyphens"))
	}
	integer := func(key string, fallback, low, high int64) int64 {
		raw := value(key, strconv.FormatInt(fallback, 10))
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < low || n > high {
			errs = append(errs, fmt.Errorf("%s must be an integer from %d to %d", key, low, high))
		}
		return n
	}
	bytes := func(key, fallback string, low, high int64) int64 {
		n, err := ParseBytes(value(key, fallback))
		if err != nil || n < low || n > high {
			errs = append(errs, fmt.Errorf("%s must be a size from %d to %d bytes (like 512m or 4g)", key, low, high))
		}
		return n
	}
	c.UID = int(integer("OCTOMUS_SANDBOX_UID", 10001, 1, math.MaxInt32))
	c.GID = int(integer("OCTOMUS_SANDBOX_GID", 10001, 1, math.MaxInt32))
	c.ClientUID = int(integer("OCTOMUS_SANDBOX_CLIENT_UID", int64(os.Getuid()), 0, math.MaxInt32))
	cpus, err := strconv.ParseFloat(value("OCTOMUS_SANDBOX_CPUS", "2"), 64)
	// Written so NaN, which ParseFloat accepts and every comparison lets through, fails it.
	if err != nil || !(cpus >= 0.1 && cpus <= 256) {
		errs = append(errs, errors.New("OCTOMUS_SANDBOX_CPUS must be a number of CPUs from 0.1 to 256"))
	}
	c.NanoCPUs = int64(cpus * 1e9)
	c.Memory = bytes("OCTOMUS_SANDBOX_MEMORY", "4g", 64<<20, 1<<40)
	c.Tmpfs = bytes("OCTOMUS_SANDBOX_TMPFS", "1g", 16<<20, 1<<40)
	c.Pids = integer("OCTOMUS_SANDBOX_PIDS", 1024, 32, 1<<22)
	c.Max = int(integer("OCTOMUS_SANDBOX_MAX", 12, 1, 256))
	c.MaxSeconds = uint64(integer("OCTOMUS_SANDBOX_MAX_SECONDS", 21600, 60, 7*24*3600))
	if c.Runtime != "" && !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`).MatchString(c.Runtime) {
		errs = append(errs, errors.New("OCTOMUS_SANDBOX_RUNTIME must be a runtime name such as runsc"))
	}
	return c, errors.Join(errs...)
}

// ParseBytes reads a size such as 1073741824, 512m or 4g (binary units).
func ParseBytes(raw string) (int64, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	multiplier := int64(1)
	for suffix, factor := range map[string]int64{"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40} {
		if trimmed, ok := strings.CutSuffix(raw, suffix); ok {
			raw, multiplier = trimmed, factor
			break
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("Invalid size %q", raw)
	}
	return n * multiplier, nil
}
