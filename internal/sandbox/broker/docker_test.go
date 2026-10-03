package broker_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/broker"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// dockerBroker runs a real broker against the local Docker daemon, with named volumes bound to temporary directories
// so the test can see what sandboxes wrote. It is opt-in: OCTOMUS_DOCKER_TEST=1.
type dockerBroker struct {
	cfg      broker.Config
	remote   *sandbox.Remote
	instance string
	image    string
	cancel   context.CancelFunc
	served   chan error
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s\n%s", strings.Join(args, " "), err, out, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

var buildOnce sync.Once
var buildDir string
var buildErr error

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// buildArtifacts builds the octomus-agent helper and the fake runners once per test binary.
func buildArtifacts(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "octomus-broker-build-")
		if buildErr != nil {
			return
		}
		root := repoRoot(t)
		for output, pkg := range map[string]string{"octomus-agent": "./cmd/octomus-agent", "fakerunner": "./internal/sandbox/broker/testdata/fakerunner"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, output), pkg)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
				return
			}
		}
		dockerfile := "FROM debian:trixie-slim\nCOPY fakerunner /usr/local/bin/codex\nCOPY fakerunner /usr/local/bin/opencode\n"
		buildErr = os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644)
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildDir
}

func startDockerBroker(t *testing.T, tune func(*broker.Config)) *dockerBroker {
	t.Helper()
	if os.Getenv("OCTOMUS_DOCKER_TEST") != "1" {
		t.Skip("set OCTOMUS_DOCKER_TEST=1 to run the broker against the local Docker daemon")
	}
	build := buildArtifacts(t)
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	id := hex.EncodeToString(suffix[:])
	image := "octomus-broker-test:" + id
	docker(t, "build", "-q", "-t", image, build)
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", image).Run() })
	dirs := map[string]string{}
	for _, name := range []string{"data", "runner", "tools"} {
		dir := t.TempDir()
		dirs[name] = dir
		volume := fmt.Sprintf("octomus-test-%s-%s", id, name)
		docker(t, "volume", "create", "--driver", "local", "--opt", "type=none", "--opt", "device="+dir, "--opt", "o=bind", volume)
		t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", volume).Run() })
	}
	for _, name := range []string{"runner", "verify"} {
		network := fmt.Sprintf("octomus-test-%s-%s", id, name)
		docker(t, "network", "create", "--internal", "-o", "com.docker.network.bridge.gateway_mode_ipv4=isolated", network)
		t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	}
	cfg := broker.Config{
		Socket:        testutil.SocketPath(t, "sandboxd.sock"),
		DockerSocket:  "/var/run/docker.sock",
		Image:         image,
		DataDir:       dirs["data"],
		DataVolume:    "octomus-test-" + id + "-data",
		RunnerVolume:  "octomus-test-" + id + "-runner",
		RunnerDir:     dirs["runner"],
		ToolsVolume:   "octomus-test-" + id + "-tools",
		ToolsDir:      dirs["tools"],
		RunnerNetwork: "octomus-test-" + id + "-runner",
		VerifyNetwork: "octomus-test-" + id + "-verify",
		Instance:      "test-" + id,
		UID:           os.Getuid(),
		GID:           os.Getgid(),
		ClientUID:     os.Getuid(),
		NanoCPUs:      1e9,
		Memory:        256 << 20,
		Pids:          256,
		Tmpfs:         64 << 20,
		Max:           4,
		MaxSeconds:    120,
	}
	if tune != nil {
		tune(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b, err := broker.New(ctx, cfg, filepath.Join(build, "octomus-agent"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	listener, err := b.Listen()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	h := &dockerBroker{cfg: cfg, remote: sandbox.NewRemote(cfg.Socket), instance: cfg.Instance, image: image, cancel: cancel, served: make(chan error, 1)}
	go func() { h.served <- b.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-h.served; err != nil {
			t.Errorf("broker shutdown = %v", err)
		}
		if left := h.containers(t); left != "" {
			t.Errorf("broker left sandboxes behind: %s", left)
			_ = exec.Command("sh", "-c", "docker ps -aq --filter label=octomus.sandbox.instance="+h.instance+" | xargs -r docker rm -f").Run()
		}
	})
	return h
}

func (h *dockerBroker) containers(t *testing.T) string {
	return docker(t, "ps", "-aq", "--filter", "label=octomus.sandbox.instance="+h.instance)
}

// taskRoot makes an owned task root whose trusted git metadata has a HEAD.
func (h *dockerBroker) taskRoot(t *testing.T) string {
	t.Helper()
	ws := broker.OwnedRoot(t, h.cfg, "tasks/"+uuid.NewString())
	if err := os.WriteFile(filepath.Join(filepath.Dir(ws), "repo.git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestDockerVerifySandboxIsContained(t *testing.T) {
	h := startDockerBroker(t, nil)
	if err := os.WriteFile(filepath.Join(h.cfg.DataDir, "state.db"), []byte("secret state"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := h.taskRoot(t)
	sibling := h.taskRoot(t)
	if err := os.WriteFile(filepath.Join(sibling, "sibling-marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.Join([]string{
		`echo "uid=$(id -u) cwd=$(pwd)"`,
		`grep -E '^(CapEff|NoNewPrivs|Seccomp):' /proc/self/status | tr -s '\t ' ' '`,
		`touch /usr/escape 2>/dev/null && echo ROOTFS_WRITABLE || echo rootfs_read_only`,
		`touch ../repo.git/escape 2>/dev/null && echo REPO_WRITABLE || echo repo_read_only`,
		`test -e ../../../state.db && echo STATE_VISIBLE || echo state_hidden`,
		`test -e /var/run/docker.sock && echo DOCKER_VISIBLE || echo docker_hidden`,
		`ls ../.. | tr '\n' ' '`,
		`test -e ../../*/workspace/sibling-marker && echo SIBLING_REACHED || echo sibling_unreached`,
		`rm .git 2>/dev/null && echo POINTER_DELETED || echo pointer_pinned`,
		`mv .git .git.swap 2>/dev/null && echo POINTER_MOVED || echo pointer_pinned`,
		`mkdir .git 2>/dev/null && echo POINTER_DIR_MADE || echo pointer_pinned`,
		`(echo x > .git) 2>/dev/null && echo POINTER_WRITTEN || echo pointer_pinned`,
		`grep -q gitdir .git && echo pointer_readable`,
		`echo "pids=$(cat /sys/fs/cgroup/pids.max) memory=$(cat /sys/fs/cgroup/memory.max)"`,
		`getent hosts example.com >/dev/null 2>&1 && echo DNS_RESOLVES || echo dns_blocked`,
		`echo "home=$HOME" && touch "$HOME/cache" && echo home_writable`,
		`echo written > result.txt`,
		`echo to-stderr >&2`,
		`exit 3`,
	}, "\n")
	out, _, err := sandbox.Verify(context.Background(), h.remote, ws, script, 60, true)
	if err != nil {
		t.Fatal(err)
	}
	stdout := string(out.Stdout.Bytes)
	for _, want := range []string{
		fmt.Sprintf("uid=%d cwd=%s", os.Getuid(), ws),
		"CapEff: 0000000000000000", "NoNewPrivs: 1", "Seccomp: 2",
		"rootfs_read_only", "repo_read_only", "state_hidden", "docker_hidden",
		"sibling_unreached", "pointer_readable",
		"pids=256 memory=268435456", "dns_blocked", "home=/home/octomus", "home_writable",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("sandbox output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "POINTER_") || strings.Contains(stdout, "SIBLING_REACHED") {
		t.Errorf("the .git pointer or a sibling root was reachable:\n%s", stdout)
	}
	if strings.Contains(stdout, filepath.Base(filepath.Dir(sibling))) {
		t.Errorf("the sandbox saw a sibling root:\n%s", stdout)
	}
	if code, ok := out.Status.Code(); !ok || code != 3 || out.Status.Success() {
		t.Fatalf("status = %v; want exit 3", out.Status)
	}
	if string(out.Stderr.Bytes) != "to-stderr\n" {
		t.Fatalf("stderr = %q", out.Stderr.Bytes)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "result.txt")); err != nil || string(data) != "written\n" {
		t.Fatalf("work tree write = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(ws), "repo.git", "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the sandbox wrote the trusted git metadata")
	}
	if left := h.containers(t); left != "" {
		t.Fatalf("finished sandbox was not removed: %s", left)
	}
}

func TestDockerRunnerStdioStreams(t *testing.T) {
	h := startDockerBroker(t, nil)
	ws := h.taskRoot(t)
	var stderr testutil.SyncBuffer
	child, err := h.remote.Start(context.Background(), sandbox.Spec{
		Kind: sandbox.KindRunner, Runner: config.BackendCodex, Dir: ws, Stdin: true, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(child.Stdout())
	lines.Buffer(make([]byte, 1<<20), 32<<20)
	big := strings.Repeat("x", 3<<20)
	for _, line := range []string{"hello", big, "exit"} {
		if err := child.Stdin().SetWriteDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		for payload := []byte(line + "\n"); len(payload) > 0; {
			n, err := child.Stdin().Write(payload)
			payload = payload[n:]
			if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal(err)
			}
			_ = child.Stdin().SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
		}
	}
	want := []string{"echo:5:HELLO", fmt.Sprintf("echo:%d:%s", len(big), strings.Repeat("X", 16))}
	for _, expected := range want {
		if !lines.Scan() || lines.Text() != expected {
			t.Fatalf("stdout line = %q (%v); want %q", lines.Text(), lines.Err(), expected)
		}
	}
	status, err := child.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := status.Code(); code != 4 {
		t.Fatalf("status = %v; want the runner's exit 4", status)
	}
	if !strings.Contains(stderr.String(), "codex diagnostic") {
		t.Fatalf("stderr sink = %q", stderr.String())
	}
	version, err := h.remote.RunnerVersion(context.Background(), sandbox.Spec{Runner: config.BackendCodex}, 10)
	if err != nil || version != "codex-fake 1.0.0" {
		t.Fatalf("runner version = %q, %v", version, err)
	}
}

func TestDockerContainmentProbePasses(t *testing.T) {
	h := startDockerBroker(t, nil)
	report, err := sandbox.Probe(context.Background(), h.remote)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, check := range report.Checks {
		ids = append(ids, check.ID)
		if !check.Passed {
			t.Errorf("containment check %s failed: %s", check.ID, check.Detail)
		}
	}
	if !report.Passed() || len(report.Checks) != 11 {
		t.Fatalf("probe checks = %v", ids)
	}
	t.Logf("kernel %s; checks %v", report.Kernel, ids)
}
