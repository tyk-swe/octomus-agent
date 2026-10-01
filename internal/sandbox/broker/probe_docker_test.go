package broker_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// probeContainer runs the containment helper in a container the test configures itself, so the probe can be shown
// to fail on postures the broker would never produce.
func probeContainer(t *testing.T, network string, readOnly bool) map[string]sandbox.ProbeCheck {
	t.Helper()
	build := buildArtifacts(t)
	args := []string{"run", "--rm", "--user", "10001:10001", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--network", network, "--pids-limit", "64", "--memory", "128m",
		"--mount", "type=bind,source=" + filepath.Join(build, "octomus-agent") + ",target=/octomus/octomus-agent,readonly"}
	if readOnly {
		args = append(args, "--read-only")
	}
	args = append(args, "debian:trixie-slim", "/octomus/octomus-agent", "--sandbox-init", wire.ProbeContainment)
	var report sandbox.ProbeReport
	if err := json.Unmarshal([]byte(docker(t, args...)), &report); err != nil {
		t.Fatal(err)
	}
	checks := map[string]sandbox.ProbeCheck{}
	for _, check := range report.Checks {
		checks[check.ID] = check
	}
	return checks
}

func probeNetwork(t *testing.T, options ...string) string {
	t.Helper()
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	name := "octomus-test-probe-" + hex.EncodeToString(suffix[:])
	docker(t, append(append([]string{"network", "create", "--internal"}, options...), name)...)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", name).Run() })
	return name
}

// Docker installs no default route on any internal network, so the probe has to reach for the host's bridge address
// itself. A plain internal network leaves the host one connection away; an isolated gateway leaves its address free,
// and the container that takes it is a neighbour, not the host.
func TestDockerProbeTellsAPlainGatewayFromAnIsolatedOne(t *testing.T) {
	if os.Getenv("OCTOMUS_DOCKER_TEST") != "1" {
		t.Skip("set OCTOMUS_DOCKER_TEST=1 to run the probe against the local Docker daemon")
	}
	plain := probeContainer(t, probeNetwork(t), true)["no_host_route"]
	if plain.Passed || !strings.HasPrefix(plain.Detail, "reached ") {
		t.Fatalf("no_host_route on a plain internal network = %+v; want the host's bridge address reached", plain)
	}
	isolated := probeNetwork(t, "-o", "com.docker.network.bridge.gateway_mode_ipv4=isolated")
	neighbour := isolated + "-neighbour"
	docker(t, "run", "-d", "--rm", "--name", neighbour, "--network", isolated, "debian:trixie-slim", "sleep", "120")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", neighbour).Run() })
	checks := probeContainer(t, isolated, true)
	for _, id := range []string{"no_host_route", "no_direct_egress", "read_only_image"} {
		if !checks[id].Passed {
			t.Errorf("%s on an isolated internal network = %+v", id, checks[id])
		}
	}
	if detail := checks["no_host_route"].Detail; !strings.Contains(detail, " is the container "+neighbour) {
		t.Errorf("no_host_route = %q; want the neighbour holding the first address named", detail)
	}
}

// The probe runs as a non-root user, which cannot write to /usr or /etc on a writable image either.
func TestDockerProbeFailsAWritableImage(t *testing.T) {
	if os.Getenv("OCTOMUS_DOCKER_TEST") != "1" {
		t.Skip("set OCTOMUS_DOCKER_TEST=1 to run the probe against the local Docker daemon")
	}
	check := probeContainer(t, "none", false)["read_only_image"]
	if check.Passed || !strings.Contains(check.Detail, "mount / rw") {
		t.Fatalf("read_only_image on a writable image = %+v", check)
	}
}
