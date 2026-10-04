package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	octomus "github.com/tyk-swe/octomus-agent"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// isolatedGateway is the bridge option that gives an internal network no address on the host, so sandboxes cannot
// reach host services through their gateway.
const isolatedGateway = "com.docker.network.bridge.gateway_mode_ipv4"

// isolatedGatewayAPI is the Engine API of Docker Engine 28, the first whose bridge driver enforces an isolated gateway.
// Older engines store the option as given without acting on it, so the network's options alone cannot tell.
const isolatedGatewayAPI = "1.48"

// startupSweepWait bounds the removal of a previous broker's leftovers, so one the daemon cannot remove fails startup,
// and the broker restarts, rather than leaving it waiting with nothing served.
const startupSweepWait = 2 * time.Minute

// New removes any sandbox a previous broker left behind, checks the daemon, image, networks and volumes and installs
// the broker's executable for sandboxes. It refuses to serve a deployment that would weaken isolation.
func New(ctx context.Context, cfg Config, executable string) (*Broker, error) {
	b := newBroker(cfg)
	version, err := b.engine.Version(ctx)
	if err != nil {
		return nil, fmt.Errorf("Docker Engine is unreachable at %s: %w", cfg.DockerSocket, err)
	}
	if !apiAtLeast(version.APIVersion, isolatedGatewayAPI) {
		return nil, fmt.Errorf("Docker Engine API %s is older than %s; sandboxes need Docker Engine 28 or later to isolate their networks from the host",
			version.APIVersion, isolatedGatewayAPI)
	}
	// Leftovers go first: their time limits died with the previous broker, so a start that any later check refuses
	// must not leave them running with live egress leases.
	sweepCtx, cancelSweep := context.WithTimeout(ctx, startupSweepWait)
	err = b.sweep(sweepCtx)
	cancelSweep()
	if err != nil {
		return nil, err
	}
	image, err := b.inspectImage(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{cfg.RunnerNetwork, cfg.VerifyNetwork} {
		network, err := b.engine.NetworkInspect(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("Sandbox network %s: %w", name, err)
		}
		if !network.Internal {
			return nil, fmt.Errorf("Sandbox network %s must be internal so sandboxes have no direct route out", name)
		}
		if network.EnableIPv6 {
			return nil, fmt.Errorf("Sandbox network %s must not enable IPv6", name)
		}
		if network.Options[isolatedGateway] != "isolated" {
			return nil, fmt.Errorf("Sandbox network %s must set %s=isolated so sandboxes cannot reach host services", name, isolatedGateway)
		}
	}
	for _, name := range []string{cfg.DataVolume, cfg.RunnerVolume, cfg.ToolsVolume} {
		if err := b.engine.VolumeInspect(ctx, name); err != nil {
			return nil, fmt.Errorf("Sandbox volume %s: %w", name, err)
		}
	}
	for _, dir := range wire.RunnerHomeDirs {
		if err := os.MkdirAll(filepath.Join(cfg.RunnerDir, dir.Volume), 0o700); err != nil {
			return nil, fmt.Errorf("Preparing the runner state volume: %w", err)
		}
	}
	if err := installTools(executable, cfg.ToolsDir); err != nil {
		return nil, fmt.Errorf("Installing the sandbox helper: %w", err)
	}
	b.info = wire.BrokerInfo{
		Version:       octomus.Version,
		DockerVersion: version.Version,
		APIVersion:    version.APIVersion,
		Image:         cfg.Image,
		ImageID:       image.ID,
		ImageDigests:  image.RepoDigests,
		Runtime:       cfg.Runtime,
		Limits: wire.BrokerLimits{
			NanoCPUs: cfg.NanoCPUs, Memory: cfg.Memory, Pids: cfg.Pids, Tmpfs: cfg.Tmpfs, Max: cfg.Max, MaxSeconds: cfg.MaxSeconds,
		},
		Networks: wire.BrokerNetworks{Runner: cfg.RunnerNetwork, Verify: cfg.VerifyNetwork},
		Egress:   cfg.EgressProxy != "",
	}
	versions, failures, err := b.probeVersions(ctx, image.ID)
	if err != nil {
		return nil, fmt.Errorf("Probing runner versions in the sandbox image: %w", err)
	}
	b.info.Runners, b.info.RunnerErrors = versions, failures
	return b, nil
}

func apiAtLeast(have, want string) bool {
	parse := func(v string) (int, int) {
		major, minor, _ := strings.Cut(v, ".")
		a, _ := strconv.Atoi(major)
		b, _ := strconv.Atoi(minor)
		return a, b
	}
	haveMajor, haveMinor := parse(have)
	wantMajor, wantMinor := parse(want)
	return haveMajor > wantMajor || haveMajor == wantMajor && haveMinor >= wantMinor
}

// installTools copies the broker's own static executable into the tools volume so every sandbox, whatever its image,
// runs the matching helper for OpenCode bridging and probes. It also writes the git configuration runner sandboxes
// use instead of their home's.
func installTools(executable, dir string) error {
	source, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	if err := install(dir, filepath.Base(toolsBinary), source, 0o755); err != nil {
		return err
	}
	return install(dir, filepath.Base(toolsGitConfig), []byte(runnerGitConfig), 0o644)
}

// install atomically replaces dir/name with data unless it already holds exactly that.
func install(dir, name string, data []byte, mode os.FileMode) error {
	target := filepath.Join(dir, name)
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, data) {
		return nil
	}
	temp := filepath.Join(dir, "."+name+"."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(temp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(temp, mode); err != nil {
		os.Remove(temp)
		return err
	}
	if err := os.Rename(temp, target); err != nil {
		os.Remove(temp)
		return err
	}
	installed, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if sha256.Sum256(installed) != sha256.Sum256(data) {
		return fmt.Errorf("installed %s does not match what the broker wrote", name)
	}
	return nil
}
