package broker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	octomus "github.com/tyk-swe/octomus-agent"
	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// isolatedGateway is the bridge option that gives an internal network no address on the host, so sandboxes cannot
// reach host services through their gateway.
const isolatedGateway = "com.docker.network.bridge.gateway_mode_ipv4"

// isolatedGatewayAPI is the Engine API of Docker Engine 28, the first whose bridge driver enforces an isolated gateway.
// Older engines store the option as given without acting on it, so the network's options alone cannot tell.
const isolatedGatewayAPI = "1.48"

type Broker struct {
	cfg    Config
	engine *engineapi.Client
	info   wire.BrokerInfo
	// slots admits sandboxes up to the limit. A sandbox gives its slot back only once its removal is confirmed.
	slots   chan struct{}
	leases  *egress.Leases
	started time.Time
	// closing closes when Serve begins to shut down.
	closing chan struct{}
	// refresh admits one request at a time to probe an image the configured tag newly resolves to. Its holder owns
	// failed.
	refresh chan struct{}
	// failed is the last rebuilt image whose probe failed, so requests soon after fail without probing it again.
	failed imageFailure
	// mu guards info and live.
	mu   sync.Mutex
	live map[string]string
}

type imageFailure struct {
	id  string
	err error
	at  time.Time
}

var (
	// startupSweepWait bounds the removal of a previous broker's leftovers, so one the daemon cannot remove fails
	// startup, and the broker restarts, rather than leaving it waiting with nothing served.
	startupSweepWait = 2 * time.Minute
	// probeRetry is how long a rebuilt image whose runner versions could not be probed is refused without probing it
	// again.
	probeRetry = 30 * time.Second
)

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
		if _, err := b.engine.VolumeInspect(ctx, name); err != nil {
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
	versions, err := b.probeVersions(ctx, image.ID)
	if err != nil {
		return nil, fmt.Errorf("Probing runner versions in the sandbox image: %w", err)
	}
	b.info.Runners = versions
	return b, nil
}

// newBroker is a broker for cfg that has checked nothing yet.
func newBroker(cfg Config) *Broker {
	b := &Broker{
		cfg:     cfg,
		engine:  engineapi.New(cfg.DockerSocket),
		slots:   make(chan struct{}, cfg.Max),
		started: time.Now(),
		closing: make(chan struct{}),
		refresh: make(chan struct{}, 1),
		live:    map[string]string{},
	}
	if cfg.LeaseDir != "" {
		b.leases = &egress.Leases{Dir: cfg.LeaseDir}
	}
	return b
}

// inspectImage resolves the configured tag. Only an image the daemon does not have is reported as missing.
func (b *Broker) inspectImage(ctx context.Context) (engineapi.Image, error) {
	image, err := b.engine.ImageInspect(ctx, b.cfg.Image)
	switch {
	case engineapi.IsNotFound(err):
		return image, fmt.Errorf("Sandbox image %s is not available locally (the broker never pulls): %w", b.cfg.Image, err)
	case err != nil:
		return image, fmt.Errorf("Inspecting sandbox image %s: %w", b.cfg.Image, err)
	}
	return image, nil
}

// image resolves the configured tag for a new sandbox. An image rebuilt under the same tag takes effect for the next
// sandbox, with its runner versions probed again; a tag that no longer resolves fails clearly, rather than leaving
// sandboxes on an image that may already be pruned. A rebuilt image whose probe failed is refused for probeRetry.
func (b *Broker) image(ctx context.Context) (string, error) {
	image, err := b.inspectImage(ctx)
	if err != nil {
		return "", err
	}
	current := func() string {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.info.ImageID
	}
	if image.ID == current() {
		return image.ID, nil
	}
	select {
	case b.refresh <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-b.refresh }()
	if image.ID == current() {
		return image.ID, nil
	}
	if b.failed.id == image.ID && time.Since(b.failed.at) < probeRetry {
		return "", b.failed.err
	}
	versions, err := b.probeVersions(ctx, image.ID)
	if err != nil {
		err = fmt.Errorf("Probing runner versions in the rebuilt sandbox image %s: %w", b.cfg.Image, err)
		// A request that gave up says nothing about the image.
		if ctx.Err() == nil {
			b.failed = imageFailure{id: image.ID, err: err, at: time.Now()}
		}
		return "", err
	}
	b.mu.Lock()
	b.info.ImageID, b.info.ImageDigests, b.info.Runners = image.ID, image.RepoDigests, versions
	b.mu.Unlock()
	b.logf("Sandbox image %s now resolves to %s", b.cfg.Image, image.ID)
	return image.ID, nil
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

// sweep removes sandboxes this instance left behind. Only containers carrying this broker's instance label are ever
// touched; everything else on the host is not the broker's to manage. One failure does not stop the rest, and every
// egress lease is cleared even then: no sandbox an earlier broker started keeps its way out.
func (b *Broker) sweep(ctx context.Context) error {
	var errs []error
	containers, err := b.engine.ContainerList(ctx, map[string]string{instanceLabel: b.cfg.Instance})
	if err != nil {
		errs = append(errs, fmt.Errorf("Listing leftover sandboxes: %w", err))
	}
	for _, container := range containers {
		if container.Labels[instanceLabel] != b.cfg.Instance {
			continue
		}
		if err := b.engine.ContainerRemove(ctx, container.ID); err != nil {
			errs = append(errs, fmt.Errorf("Removing leftover sandbox %s: %w", container.ID, err))
		}
	}
	if b.leases != nil {
		if err := b.leases.Clear(); err != nil {
			errs = append(errs, fmt.Errorf("Clearing egress leases: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (b *Broker) probeVersions(ctx context.Context, image string) (map[string]string, error) {
	p, err := b.cfg.plan(wire.Request{Kind: wire.KindProbe, Mode: wire.ProbeVersions, Timeout: 120})
	if err != nil {
		return nil, err
	}
	p.image = image
	var stdout, stderr bytes.Buffer
	collect := func(buf *bytes.Buffer) func([]byte) error {
		return func(p []byte) error {
			if buf.Len()+len(p) > 64<<10 {
				return errors.New("version probe output is too large")
			}
			buf.Write(p)
			return nil
		}
	}
	report, err := b.runSandbox(ctx, p, collect(&stdout), collect(&stderr), nil)
	if err != nil {
		return nil, err
	}
	if report.Error != "" && !report.Killed {
		return nil, fmt.Errorf("version probe failed: %s", report.Error)
	}
	if report.Code != 0 || report.Killed || report.OOM {
		return nil, fmt.Errorf("version probe failed with exit %d: %s", report.Code, strings.TrimSpace(stderr.String()))
	}
	versions := map[string]string{}
	if err := json.Unmarshal(stdout.Bytes(), &versions); err != nil {
		return nil, fmt.Errorf("version probe output: %w", err)
	}
	return versions, nil
}

// Info is what the broker serves the control plane about itself.
func (b *Broker) Info() wire.BrokerInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	info := b.info
	info.Live = len(b.live)
	return info
}

// logf reports what the broker could not do on its own, such as a removal it keeps retrying.
func (b *Broker) logf(format string, args ...any) {
	w := b.cfg.Log
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "Octomus sandbox broker: "+format+"\n", args...)
}

// Listen opens the broker socket, readable and writable only by its owner.
func (b *Broker) Listen() (net.Listener, error) {
	if err := os.Remove(b.cfg.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	old := unix.Umask(0o177)
	listener, err := net.Listen("unix", b.cfg.Socket)
	unix.Umask(old)
	if err != nil {
		return nil, err
	}
	return &peerListener{Listener: listener, uid: b.cfg.ClientUID}, nil
}

// peerListener serves only connections from the configured client uid, checked by the kernel's peer credentials.
type peerListener struct {
	net.Listener
	uid int
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if uid, ok := peerUID(conn); ok && uid == l.uid {
			return conn, nil
		}
		conn.Close()
	}
}

func peerUID(conn net.Conn) (int, bool) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || credErr != nil {
		return 0, false
	}
	return int(cred.Uid), true
}

// Serve answers the control plane until ctx ends, then stops every sandbox it started.
func (b *Broker) Serve(ctx context.Context, listener net.Listener) error {
	requests, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, b.Info())
	})
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) { b.handleSandbox(requests, w, r) })
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return requests }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		cancelRequests()
		close(b.closing)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(shutdown)
		cancel()
		if shutdownErr != nil {
			_ = server.Close()
		}
		// Shutdown does not track hijacked streams: their handlers are still removing their sandboxes. Racing them
		// would only make the sweep's removals collide with theirs. The steps take at most 5s + 10s + 10s, inside
		// compose's 30s stop grace period with time left to exit.
		b.awaitSandboxes(10 * time.Second)
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		return errors.Join(shutdownErr, b.sweep(cleanup))
	case err := <-done:
		return err
	}
}

// awaitSandboxes waits, up to limit, until every sandbox has given its slot back, which each does once its removal
// is confirmed. The sweep after it then meets only what a crash or a failed removal left behind.
func (b *Broker) awaitSandboxes(limit time.Duration) {
	timer := time.NewTimer(limit)
	defer timer.Stop()
	taken := 0
	defer func() {
		for range taken {
			<-b.slots
		}
	}()
	for taken < cap(b.slots) {
		select {
		case b.slots <- struct{}{}:
			taken++
		case <-timer.C:
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// handleSandbox serves one sandbox request. base is the broker's serving lifetime, which outlasts the request.
func (b *Broker) handleSandbox(base context.Context, w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, message string) {
		writeJSON(w, status, map[string]string{"error": message})
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), wire.UpgradeProtocol) {
		refuse(http.StatusUpgradeRequired, "Sandbox requests must upgrade to "+wire.UpgradeProtocol)
		return
	}
	var req wire.Request
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		refuse(http.StatusBadRequest, "Invalid sandbox request: "+err.Error())
		return
	}
	p, err := b.cfg.plan(req)
	if err != nil {
		refuse(http.StatusBadRequest, err.Error())
		return
	}
	// A full broker makes the request wait for as long as the client does. The control plane keeps its own count of
	// the same slots, but one can stay taken after it counts it free: a removal the broker is still retrying, or a
	// teardown the client stopped waiting for.
	select {
	case b.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	release := sync.OnceFunc(func() { <-b.slots })
	if p.image, err = b.image(r.Context()); err != nil {
		release()
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	prepared, err := b.prepare(r.Context(), base, p, release)
	if err != nil {
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		prepared.discard()
		refuse(http.StatusInternalServerError, "Sandbox stream cannot be hijacked")
		return
	}
	conn, stream, err := hijacker.Hijack()
	if err != nil {
		prepared.discard()
		return
	}
	defer conn.Close()
	if _, err := stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " +
		wire.UpgradeProtocol + "\r\n\r\n"); err != nil || stream.Flush() != nil {
		prepared.discard()
		return
	}
	b.stream(r.Context(), prepared, p, conn, stream.Reader)
}

// prepared is a created container with its attach stream already registered, not yet started.
type prepared struct {
	b      *Broker
	id     string
	name   string
	image  string
	lease  string
	attach *engineapi.Attached
	// release gives the sandbox's admission slot back, once its removal is confirmed.
	release func()
	once    sync.Once
	removed error
}

const (
	// createTimeout matches how long the control plane waits for a sandbox to open.
	createTimeout = 2 * time.Minute
	attachTimeout = time.Minute
)

// prepare creates a sandbox's container and attaches to it. ctx is the request's; the create alone runs under base,
// the broker's lifetime, because a client that gives up mid-create must not leave behind a container the daemon
// still finishes: once created, it has an ID and is removed like any other. prepare owns release from here on.
func (b *Broker) prepare(ctx, base context.Context, p plan, release func()) (*prepared, error) {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := fmt.Sprintf("octomus-%s-%s-%s", b.cfg.Instance, p.kind, hex.EncodeToString(suffix[:]))
	var extraEnv []string
	lease := ""
	if b.leases != nil && (p.kind != wire.KindProbe || p.probe == wire.ProbeContainment) {
		// The containment probe proves what the gateway refuses a runner sandbox, so it holds a runner's lease.
		kind := p.kind
		if kind == wire.KindProbe {
			kind = wire.KindRunner
		}
		token, err := b.leases.Grant(name, kind)
		if err != nil {
			release()
			return nil, fmt.Errorf("Granting the sandbox egress lease: %w", err)
		}
		lease, extraEnv = token, egress.ProxyEnv(b.cfg.EgressProxy, token)
	}
	spec := b.cfg.container(p, extraEnv)
	// A sandbox runs the image ID its tag resolved to, so its evidence names exactly what ran.
	spec.Image = p.image
	if spec.Image == "" {
		spec.Image = b.cfg.Image
	}
	createCtx, cancel := context.WithTimeout(base, createTimeout)
	id, warnings, err := b.engine.ContainerCreate(createCtx, name, spec)
	cancel()
	if err != nil {
		if lease != "" {
			b.leases.Revoke(lease)
		}
		// A create cut short can still finish in the daemon; removing by name finds the container if it did.
		removeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := b.engine.ContainerRemove(removeCtx, name); err != nil {
			b.logf("Removing sandbox %s after its create failed did not succeed; the next sweep removes it: %v", name, err)
		}
		cancel()
		release()
		return nil, fmt.Errorf("Creating the sandbox: %w", err)
	}
	b.mu.Lock()
	b.live[id] = p.rel
	b.mu.Unlock()
	s := &prepared{b: b, id: id, name: name, image: spec.Image, lease: lease, release: release}
	if len(warnings) > 0 {
		// Docker drops a limit the host cannot enforce and only warns; every limit in the spec is part of the boundary.
		s.discard()
		return nil, fmt.Errorf("Docker Engine would not enforce the sandbox spec: %s", strings.Join(warnings, " "))
	}
	if err := ctx.Err(); err != nil {
		s.discard()
		return nil, fmt.Errorf("Creating the sandbox: %w", err)
	}
	attachCtx, cancel := context.WithTimeout(ctx, attachTimeout)
	attach, err := b.engine.ContainerAttach(attachCtx, id, p.stdin)
	cancel()
	if err != nil {
		s.discard()
		return nil, fmt.Errorf("Attaching to the sandbox: %w", err)
	}
	s.attach = attach
	return s, nil
}

// remove closes the attach stream, revokes the egress lease and removes the container, retrying until ctx ends. Only
// a confirmed removal frees the sandbox's live entry and slot; otherwise a reaper keeps both while it keeps trying,
// so the cap on running sandboxes holds.
func (s *prepared) remove(ctx context.Context) error {
	s.once.Do(func() {
		if s.attach != nil {
			s.attach.Close()
		}
		// Egress is cut at once; that needs no confirmation.
		if s.lease != "" {
			s.b.leases.Revoke(s.lease)
		}
		if s.removed = s.b.removeContainer(ctx, s.id); s.removed != nil {
			s.b.logf("Removing sandbox %s failed; it keeps its slot while the broker retries: %v", s.name, s.removed)
			s.b.reap(s)
			return
		}
		s.forget()
	})
	return s.removed
}

// discard removes a sandbox that never ran.
func (s *prepared) discard() {
	ctx, cancel := context.WithTimeout(context.Background(), teardownBudget)
	defer cancel()
	_ = s.remove(ctx)
}

func (s *prepared) forget() {
	s.b.mu.Lock()
	delete(s.b.live, s.id)
	s.b.mu.Unlock()
	s.release()
}

// removeContainer force-removes a container, retrying until the daemon confirms it is gone or ctx ends.
func (b *Broker) removeContainer(ctx context.Context, id string) error {
	for delay := 250 * time.Millisecond; ; delay = min(2*delay, 2*time.Second) {
		err := b.engine.ContainerRemove(ctx, id)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
}

// reap keeps removing a sandbox whose teardown could not confirm its removal. Shutdown hands it to the final sweep.
func (b *Broker) reap(s *prepared) {
	go func() {
		for delay := reapDelay; ; delay = min(2*delay, 5*time.Minute) {
			select {
			case <-time.After(delay):
			case <-b.closing:
				s.forget()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := b.engine.ContainerRemove(ctx, s.id)
			cancel()
			if err == nil {
				b.logf("Removed sandbox %s after earlier failures", s.name)
				s.forget()
				return
			}
		}
	}()
}

type control struct {
	stdin  []byte
	eof    bool
	signal string
}

// output is where a sandbox's stdout and stderr go. interrupt, when set, unblocks a sink stuck writing to a client
// that stopped reading; the sinks fail from then on.
type output struct {
	stdout, stderr func([]byte) error
	interrupt      func()
}

// Teardown bounds. Once a sandbox ends, or a kill is asked for, the broker drains its output, reads its state and
// evidence, and confirms its removal within teardownBudget, so the control plane's wait for the report holds.
var (
	teardownBudget = 45 * time.Second
	// removeReserve is the part of the budget kept for the removal.
	removeReserve = 15 * time.Second
	// stopWait is how long a killed sandbox has to report its exit before the removal stops it by force.
	stopWait = 15 * time.Second
	// drainWait is how long a sandbox's output has to end once the sandbox has.
	drainWait = 10 * time.Second
	// joinWait is how long the output pump has to finish once its source is closed.
	joinWait = 5 * time.Second
	// reapDelay is the first pause before a removal that failed is tried again.
	reapDelay = 5 * time.Second
)

const streamClosed = "Sandbox stream closed"

// ending is how a sandbox's run ended.
type ending struct {
	// result is the exit the daemon reported, if it did.
	result *engineapi.WaitResult
	// killed is set when a SIGKILL the daemon delivered, or the removal by force, stopped the sandbox.
	killed bool
	reason string
	// cut is set when the control stream closed, leaving nobody to report to.
	cut bool
	// err is a failure before the sandbox could report an exit.
	err error
	// deadline bounds the teardown.
	deadline time.Time
}

// execute starts a prepared container and pumps it until it exits, the time limit passes or the control source
// ends, then tears it down within teardownBudget. It always removes the container, or leaves it to a reaper; a
// report with an Error and no kill means the broker cannot vouch for how the sandbox ended.
func (s *prepared) execute(ctx context.Context, timeout time.Duration, out output, controls <-chan control) (wire.ExitReport, error) {
	b := s.b
	input := make(chan control)
	stopInput, inputDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(inputDone)
		for {
			select {
			case <-stopInput:
				return
			case msg := <-input:
				if msg.eof {
					_ = s.attach.CloseStdin()
				} else if _, err := s.attach.Conn.Write(msg.stdin); err != nil {
					_ = s.attach.CloseStdin()
				}
			}
		}
	}()
	attached := make(chan error, 1)
	go func() {
		attached <- engineapi.Demux(s.attach.Reader, out.stdout, out.stderr)
	}()
	end := s.run(ctx, timeout, controls, input)
	if end.deadline.IsZero() {
		end.deadline = time.Now().Add(teardownBudget)
	}
	// within bounds one teardown step by what the budget leaves once the removal's share is kept back.
	within := func(step time.Duration) time.Duration {
		return max(0, min(step, time.Until(end.deadline)-removeReserve))
	}
	close(stopInput)
	// Draining closes the attach stream, which also ends a blocked stdin write.
	truncated, outErr := s.drain(end, attached, out, within)
	<-inputDone
	var report wire.ExitReport
	var failure error
	switch {
	case end.cut:
		report = wire.ExitReport{Killed: true, Error: streamClosed}
	case end.err != nil:
		failure = end.err
	default:
		report = wire.ExitReport{Killed: end.killed, Error: end.reason}
		oom := false
		if end.result != nil {
			report.Code = end.result.StatusCode
			oom = s.oomKilled(within(5 * time.Second))
		}
		// Docker marks a container OOM-killed when the kernel killed any process in it. The evidence keeps that; the
		// status says the memory limit ended the command only when the command failed.
		report.OOM = oom && report.Code != 0
		report.Sandbox = b.evidence(s.name, s.image, oom)
		switch {
		case end.killed:
		case truncated:
			report.Error = "Sandbox output was cut short after the sandbox ended"
		case outErr != nil:
			report.Error = "Sandbox output failed: " + outErr.Error()
		}
	}
	removeCtx, cancel := context.WithDeadline(context.Background(), end.deadline)
	removed := s.remove(removeCtx)
	cancel()
	switch {
	case removed == nil || end.cut:
	case failure != nil:
		failure = fmt.Errorf("%w; removing the sandbox also failed: %v", failure, removed)
	default:
		report.Killed, report.Error = false, "Removing the sandbox failed: "+removed.Error()
	}
	return report, failure
}

// run starts the container and serves its controls until it ends. Only a SIGKILL the daemon delivered marks it
// killed, and the first one names the reason: a kill that finds it already exited leaves its own exit status.
func (s *prepared) run(ctx context.Context, timeout time.Duration, controls <-chan control, input chan<- control) (end ending) {
	b := s.b
	if err := b.engine.ContainerStart(ctx, s.id); err != nil {
		end.err = fmt.Errorf("Starting the sandbox: %w", err)
		return end
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	// Waiting for not-running after start also observes an exit that happened before the wait request arrived.
	results, errs := b.engine.ContainerWait(waitCtx, s.id)
	limit := time.NewTimer(timeout)
	defer limit.Stop()
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	var stopped <-chan time.Time
	asked := ""
	kill := func(reason string) {
		if end.deadline.IsZero() {
			end.deadline, stopped, asked = time.Now().Add(teardownBudget), time.After(stopWait), reason
		}
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := b.engine.ContainerKill(killCtx, s.id, "SIGKILL")
		cancel()
		switch {
		case err == nil:
			if !end.killed {
				end.killed, end.reason = true, reason
			}
		case engineapi.IsConflict(err) || engineapi.IsNotFound(err):
			// It is no longer running: it ended on its own, and the wait reports how.
		default:
			b.logf("Killing sandbox %s failed; retrying: %v", s.name, err)
			retry.Reset(time.Second)
		}
	}
	var pendingInput []control
	pendingBytes := 0
	for {
		var sendInput chan<- control
		var nextInput control
		if len(pendingInput) > 0 {
			sendInput, nextInput = input, pendingInput[0]
		}
		select {
		case sendInput <- nextInput:
			pendingBytes -= len(nextInput.stdin)
			pendingInput[0] = control{}
			pendingInput = pendingInput[1:]
		case result := <-results:
			end.result = &result
			return end
		case err := <-errs:
			end.err = fmt.Errorf("Waiting for the sandbox: %w", err)
			return end
		case <-limit.C:
			kill("Sandbox time limit reached")
		case <-retry.C:
			kill(asked)
		case <-stopped:
			b.logf("Sandbox %s did not report its exit after a kill; removing it by force", s.name)
			if !end.killed {
				end.killed, end.reason = true, asked
			}
			return end
		case <-ctx.Done():
			end.cut = true
			return end
		case msg, ok := <-controls:
			switch {
			case !ok:
				end.cut = true
				return end
			case msg.stdin != nil || msg.eof:
				// Keep input ordered and bounded without delaying cancellation, signals or the time limit.
				if len(pendingInput) >= 1024 || pendingBytes+len(msg.stdin) > 32<<20 {
					end.err = errors.New("Sandbox stdin backlog exceeded")
					return end
				}
				pendingInput = append(pendingInput, msg)
				pendingBytes += len(msg.stdin)
			case msg.signal == wire.SignalTerminate:
				termCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = b.engine.ContainerKill(termCtx, s.id, "SIGTERM")
				cancel()
			case msg.signal == wire.SignalKill:
				kill("")
			}
		}
	}
}

// drain joins the output pump. A sandbox that exited has drainWait for the rest of its output; then, or at once when
// it did not exit, its attach stream is closed. A sink still blocked on a client that stopped reading is interrupted,
// so no output can follow the exit report. It reports whether output was cut off, else the pump's own error.
func (s *prepared) drain(end ending, attached <-chan error, out output, within func(time.Duration) time.Duration) (bool, error) {
	join := func(wait time.Duration) (bool, error) {
		select {
		case err := <-attached:
			return true, err
		default:
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case err := <-attached:
			return true, err
		case <-timer.C:
			return false, nil
		}
	}
	if end.result != nil {
		if done, err := join(within(drainWait)); done {
			s.attach.Close()
			return false, err
		}
	}
	s.attach.Close()
	if done, _ := join(within(joinWait)); !done {
		if out.interrupt != nil {
			out.interrupt()
		}
		<-attached
	}
	return true, nil
}

// oomKilled reports whether the kernel killed any process in the sandbox for memory.
func (s *prepared) oomKilled(wait time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	state, err := s.b.engine.ContainerInspect(ctx, s.id)
	if err != nil {
		s.b.logf("Reading the state of sandbox %s failed; a memory-limit kill would go unrecorded: %v", s.name, err)
		return false
	}
	return state.State.OOMKilled
}

// evidence records what one finished sandbox ran and, when the gateway is configured, where it reached out.
func (b *Broker) evidence(name, image string, oom bool) *model.SandboxRecord {
	record := &model.SandboxRecord{ImageID: image, Runtime: b.cfg.Runtime, Runs: 1, OOM: oom,
		Egress: model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{}}}
	if b.cfg.EgressCollector == "" {
		return record
	}
	// The gateway counts accepted tunnels immediately, even when upstream connections are still closing.
	summary, err := egress.FetchSummary(context.Background(), b.cfg.EgressCollector, name)
	if err != nil {
		b.logf("Reading the egress record of sandbox %s failed; its record shows no connections: %v", name, err)
		return record
	}
	for host, count := range summary.Allowed {
		record.Egress.Allowed[host] = uint64(count.Count)
	}
	for host, count := range summary.Denied {
		record.Egress.Denied[host] = uint64(count.Count)
	}
	record.Egress.Failed = map[string]uint64{}
	for host, count := range summary.Failed {
		record.Egress.Failed[host] = uint64(count.Count)
	}
	return model.MergeSandbox(nil, record)
}

// runSandbox runs a plan to completion without a control-plane stream, for the broker's own probes.
func (b *Broker) runSandbox(ctx context.Context, p plan, stdout, stderr func([]byte) error, controls <-chan control) (wire.ExitReport, error) {
	prepared, err := b.prepare(ctx, ctx, p, func() {})
	if err != nil {
		return wire.ExitReport{}, err
	}
	if controls == nil {
		controls = make(chan control)
	}
	return prepared.execute(ctx, p.timeout, output{stdout: stdout, stderr: stderr}, controls)
}

// streamWriter is a sandbox stream's connection that stays failed after its first failed write: a frame cut short
// leaves the stream unreadable, so nothing may follow it.
type streamWriter struct {
	conn   net.Conn
	failed atomic.Bool
}

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.failed.Load() {
		return 0, net.ErrClosed
	}
	n, err := w.conn.Write(p)
	if err != nil {
		w.failed.Store(true)
	}
	return n, err
}

// stream serves one sandbox over an upgraded connection. The connection is the sandbox's lifeline: if it closes,
// the container is killed and removed.
func (b *Broker) stream(ctx context.Context, s *prepared, p plan, conn net.Conn, reader *bufio.Reader) {
	written := &streamWriter{conn: conn}
	out := wire.NewFrameWriter(written)
	controls := make(chan control)
	lifeline, cut := context.WithCancel(ctx)
	defer cut()
	go func() {
		defer cut()
		for {
			kind, payload, err := wire.ReadFrame(reader)
			if err != nil {
				return
			}
			var msg control
			switch kind {
			case wire.FrameStdin:
				if !p.stdin {
					continue
				}
				msg.stdin = payload
			case wire.FrameStdinEOF:
				msg.eof = true
			case wire.FrameSignal:
				msg.signal = string(payload)
				if msg.signal != wire.SignalTerminate && msg.signal != wire.SignalKill {
					continue
				}
			default:
				return
			}
			select {
			case controls <- msg:
			case <-lifeline.Done():
				return
			}
		}
	}()
	forward := func(kind byte) func([]byte) error {
		return func(data []byte) error {
			_, err := out.Data(kind, data)
			return err
		}
	}
	report, err := s.execute(lifeline, p.timeout, output{stdout: forward(wire.FrameStdout), stderr: forward(wire.FrameStderr),
		interrupt: func() { _ = conn.SetWriteDeadline(time.Now()) }}, controls)
	if err != nil {
		report = wire.ExitReport{Error: err.Error()}
	}
	if lifeline.Err() != nil && report.Error == streamClosed || written.failed.Load() {
		return
	}
	payload, _ := json.Marshal(report)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = out.Frame(wire.FrameExit, payload)
}
