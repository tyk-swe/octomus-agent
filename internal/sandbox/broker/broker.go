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
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	octomus "github.com/tyk-swe/octomus-agent"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
)

// isolatedGateway is the bridge option that gives an internal network no address on the host, so sandboxes cannot
// reach host services through their gateway.
const isolatedGateway = "com.docker.network.bridge.gateway_mode_ipv4"

// isolatedGatewayAPI is the Engine API of Docker Engine 28, the first whose bridge driver enforces an isolated gateway.
// Older engines store the option as given without acting on it, so the network's options alone cannot tell.
const isolatedGatewayAPI = "1.48"

type Broker struct {
	cfg     Config
	engine  *engineapi.Client
	info    sandbox.BrokerInfo
	slots   chan struct{}
	leases  *leases
	started time.Time
	mu      sync.Mutex
	live    map[string]string
}

func runnerBackend(name string) config.Backend {
	if name == "opencode" {
		return config.BackendOpencode
	}
	return config.BackendCodex
}

// New removes any sandbox a previous broker left behind, checks the daemon, image, networks and volumes and installs
// the broker's executable for sandboxes. It refuses to serve a deployment that would weaken isolation.
func New(ctx context.Context, cfg Config, executable string) (*Broker, error) {
	b := &Broker{
		cfg:     cfg,
		engine:  engineapi.New(cfg.DockerSocket),
		slots:   make(chan struct{}, cfg.Max),
		started: time.Now(),
		live:    map[string]string{},
	}
	if cfg.LeaseDir != "" {
		b.leases = &leases{dir: cfg.LeaseDir}
	}
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
	if err := b.sweep(ctx); err != nil {
		return nil, err
	}
	image, err := b.engine.ImageInspect(ctx, cfg.Image)
	if err != nil {
		return nil, fmt.Errorf("Sandbox image %s is not available locally (the broker never pulls): %w", cfg.Image, err)
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
	for _, dir := range sandbox.RunnerHomeDirs {
		if err := os.MkdirAll(filepath.Join(cfg.RunnerDir, dir.Volume), 0o700); err != nil {
			return nil, fmt.Errorf("Preparing the runner state volume: %w", err)
		}
	}
	if err := installTools(executable, cfg.ToolsDir); err != nil {
		return nil, fmt.Errorf("Installing the sandbox helper: %w", err)
	}
	b.info = sandbox.BrokerInfo{
		Version:       octomus.Version,
		DockerVersion: version.Version,
		APIVersion:    version.APIVersion,
		Image:         cfg.Image,
		ImageID:       image.ID,
		ImageDigests:  image.RepoDigests,
		Runtime:       cfg.Runtime,
		Limits: sandbox.BrokerLimits{
			NanoCPUs: cfg.NanoCPUs, Memory: cfg.Memory, Pids: cfg.Pids, Tmpfs: cfg.Tmpfs, Max: cfg.Max, MaxSeconds: cfg.MaxSeconds,
		},
		Networks: sandbox.BrokerNetworks{Runner: cfg.RunnerNetwork, Verify: cfg.VerifyNetwork},
		Egress:   cfg.EgressProxy != "",
	}
	versions, err := b.probeVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("Probing runner versions in the sandbox image: %w", err)
	}
	b.info.Runners = versions
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
// runs the matching helper for OpenCode bridging and probes.
func installTools(executable, dir string) error {
	source, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, "octomus-agent")
	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, source) {
		return nil
	}
	temp := filepath.Join(dir, ".octomus-agent."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(temp, source, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(temp, 0o755); err != nil {
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
	if sha256.Sum256(installed) != sha256.Sum256(source) {
		return errors.New("installed helper does not match the broker executable")
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
		if err := b.leases.clear(); err != nil {
			errs = append(errs, fmt.Errorf("Clearing egress leases: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (b *Broker) probeVersions(ctx context.Context) (map[string]string, error) {
	p, err := b.cfg.plan(sandbox.Request{Kind: sandbox.KindProbe.String(), Mode: sandbox.ProbeVersions, Timeout: 120})
	if err != nil {
		return nil, err
	}
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
func (b *Broker) Info() sandbox.BrokerInfo {
	info := b.info
	b.mu.Lock()
	info.Live = len(b.live)
	b.mu.Unlock()
	return info
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
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(shutdown)
		cancel()
		if shutdownErr != nil {
			_ = server.Close()
		}
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		return errors.Join(shutdownErr, b.sweep(cleanup))
	case err := <-done:
		return err
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
	if !strings.EqualFold(r.Header.Get("Upgrade"), sandbox.UpgradeProtocol) {
		refuse(http.StatusUpgradeRequired, "Sandbox requests must upgrade to "+sandbox.UpgradeProtocol)
		return
	}
	var req sandbox.Request
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
	select {
	case b.slots <- struct{}{}:
	default:
		refuse(http.StatusTooManyRequests, fmt.Sprintf("All %d sandbox slots are in use", b.cfg.Max))
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-b.slots
		}
	}
	defer release()
	prepared, err := b.prepare(r.Context(), base, p)
	if err != nil {
		refuse(http.StatusInternalServerError, err.Error())
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		prepared.remove()
		refuse(http.StatusInternalServerError, "Sandbox stream cannot be hijacked")
		return
	}
	conn, stream, err := hijacker.Hijack()
	if err != nil {
		prepared.remove()
		return
	}
	defer conn.Close()
	if _, err := stream.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " +
		sandbox.UpgradeProtocol + "\r\n\r\n"); err != nil || stream.Flush() != nil {
		prepared.remove()
		return
	}
	b.stream(r.Context(), prepared, p, conn, stream.Reader)
}

// prepared is a created container with its attach stream already registered, not yet started.
type prepared struct {
	b      *Broker
	id     string
	name   string
	lease  string
	attach *engineapi.Attached
	once   sync.Once
}

const (
	// createTimeout matches how long the control plane waits for a sandbox to open.
	createTimeout = 2 * time.Minute
	attachTimeout = time.Minute
)

// prepare creates a sandbox's container and attaches to it. ctx is the request's; the create alone runs under base,
// the broker's lifetime, because a client that gives up mid-create must not leave behind a container the daemon
// still finishes: once created, it has an ID and is removed like any other.
func (b *Broker) prepare(ctx, base context.Context, p plan) (*prepared, error) {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := fmt.Sprintf("octomus-%s-%s-%s", b.cfg.Instance, p.kind, hex.EncodeToString(suffix[:]))
	var extraEnv []string
	lease := ""
	if b.leases != nil && (p.kind != sandbox.KindProbe || p.probe == sandbox.ProbeContainment) {
		// The containment probe proves what the gateway refuses a runner sandbox, so it holds a runner's lease.
		kind := p.kind
		if kind == sandbox.KindProbe {
			kind = sandbox.KindRunner
		}
		token, err := b.leases.grant(name, kind)
		if err != nil {
			return nil, fmt.Errorf("Granting the sandbox egress lease: %w", err)
		}
		lease, extraEnv = token, proxyEnv(b.cfg.EgressProxy, token)
	}
	spec := b.cfg.container(p, extraEnv)
	spec.Image = b.info.ImageID
	if spec.Image == "" {
		spec.Image = b.cfg.Image
	}
	createCtx, cancel := context.WithTimeout(base, createTimeout)
	id, warnings, err := b.engine.ContainerCreate(createCtx, name, spec)
	cancel()
	if err != nil {
		if lease != "" {
			b.leases.revoke(lease)
		}
		// A create cut short can still finish in the daemon; removing by name finds the container if it did.
		removeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = b.engine.ContainerRemove(removeCtx, name)
		cancel()
		return nil, fmt.Errorf("Creating the sandbox: %w", err)
	}
	b.mu.Lock()
	b.live[id] = p.rel
	b.mu.Unlock()
	s := &prepared{b: b, id: id, name: name, lease: lease}
	if len(warnings) > 0 {
		// Docker drops a limit the host cannot enforce and only warns; every limit in the spec is part of the boundary.
		s.remove()
		return nil, fmt.Errorf("Docker Engine would not enforce the sandbox spec: %s", strings.Join(warnings, " "))
	}
	if err := ctx.Err(); err != nil {
		s.remove()
		return nil, fmt.Errorf("Creating the sandbox: %w", err)
	}
	attachCtx, cancel := context.WithTimeout(ctx, attachTimeout)
	attach, err := b.engine.ContainerAttach(attachCtx, id, p.stdin)
	cancel()
	if err != nil {
		s.remove()
		return nil, fmt.Errorf("Attaching to the sandbox: %w", err)
	}
	s.attach = attach
	return s, nil
}

func (s *prepared) remove() {
	s.once.Do(func() {
		if s.attach != nil {
			s.attach.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.b.engine.ContainerRemove(ctx, s.id)
		if s.lease != "" {
			s.b.leases.revoke(s.lease)
		}
		s.b.mu.Lock()
		delete(s.b.live, s.id)
		s.b.mu.Unlock()
	})
}

type control struct {
	stdin  []byte
	eof    bool
	signal string
}

// execute starts a prepared container and pumps it until it exits, the time limit passes or the control source
// ends. It always removes the container.
func (s *prepared) execute(ctx context.Context, timeout time.Duration, stdout, stderr func([]byte) error, controls <-chan control) (sandbox.ExitReport, error) {
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
	defer func() {
		close(stopInput)
		s.remove() // Closing attach interrupts a blocked stdin write.
		<-inputDone
	}()
	var pendingInput []control
	pendingBytes := 0
	b := s.b
	attached := make(chan error, 1)
	go func() {
		attached <- engineapi.Demux(s.attach.Reader, stdout, stderr)
	}()
	if err := b.engine.ContainerStart(ctx, s.id); err != nil {
		return sandbox.ExitReport{}, fmt.Errorf("Starting the sandbox: %w", err)
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	// Waiting for not-running after start also observes an exit that happened before the wait request arrived.
	results, errs := b.engine.ContainerWait(waitCtx, s.id)
	limit := time.NewTimer(timeout)
	defer limit.Stop()
	killed := false
	reason := ""
	kill := func(signal string) {
		killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = b.engine.ContainerKill(killCtx, s.id, signal)
	}
	var result engineapi.WaitResult
wait:
	for {
		var sendInput chan control
		var nextInput control
		if len(pendingInput) > 0 {
			sendInput, nextInput = input, pendingInput[0]
		}
		select {
		case sendInput <- nextInput:
			pendingBytes -= len(nextInput.stdin)
			pendingInput[0] = control{}
			pendingInput = pendingInput[1:]
		case result = <-results:
			break wait
		case err := <-errs:
			return sandbox.ExitReport{}, fmt.Errorf("Waiting for the sandbox: %w", err)
		case <-limit.C:
			killed, reason = true, "Sandbox time limit reached"
			kill("SIGKILL")
		case <-ctx.Done():
			return sandbox.ExitReport{Killed: true, Error: "Sandbox stream closed"}, nil
		case msg, ok := <-controls:
			switch {
			case !ok:
				return sandbox.ExitReport{Killed: true, Error: "Sandbox stream closed"}, nil
			case msg.stdin != nil || msg.eof:
				// Keep input ordered and bounded without delaying cancellation, signals or the time limit.
				if len(pendingInput) >= 1024 || pendingBytes+len(msg.stdin) > 32<<20 {
					return sandbox.ExitReport{}, errors.New("Sandbox stdin backlog exceeded")
				}
				pendingInput = append(pendingInput, msg)
				pendingBytes += len(msg.stdin)
			case msg.signal == signalTerm:
				kill("SIGTERM")
			case msg.signal == signalKill:
				killed, reason = true, ""
				kill("SIGKILL")
			}
		}
	}
	select {
	case <-attached:
	case <-time.After(10 * time.Second):
	}
	report := sandbox.ExitReport{Code: result.StatusCode, Killed: killed, Error: reason}
	inspectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if state, err := b.engine.ContainerInspect(inspectCtx, s.id); err == nil {
		report.OOM = state.State.OOMKilled
	}
	report.Sandbox = b.evidence(s.name, report.OOM)
	return report, nil
}

// evidence records what one finished sandbox ran and, when the gateway is configured, where it reached out.
func (b *Broker) evidence(name string, oom bool) *model.SandboxRecord {
	record := &model.SandboxRecord{ImageID: b.info.ImageID, Runtime: b.cfg.Runtime, Runs: 1, OOM: oom,
		Egress: model.SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{}}}
	if b.cfg.EgressCollector == "" {
		return record
	}
	// The gateway counts accepted tunnels immediately, even when upstream connections are still closing.
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", b.cfg.EgressCollector)
		}}}
	resp, err := client.Get("http://egress/v1/summary?sandbox=" + url.QueryEscape(name))
	if err != nil {
		return record
	}
	defer resp.Body.Close()
	var summary struct {
		Allowed map[string]struct{ Count uint64 } `json:"allowed"`
		Denied  map[string]struct{ Count uint64 } `json:"denied"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&summary) != nil {
		return record
	}
	for host, count := range summary.Allowed {
		record.Egress.Allowed[host] = count.Count
	}
	for host, count := range summary.Denied {
		record.Egress.Denied[host] = count.Count
	}
	return model.MergeSandbox(nil, record)
}

const (
	signalTerm = sandbox.SignalTerminate
	signalKill = sandbox.SignalKill
)

// runSandbox runs a plan to completion without a control-plane stream, for the broker's own probes.
func (b *Broker) runSandbox(ctx context.Context, p plan, stdout, stderr func([]byte) error, controls <-chan control) (sandbox.ExitReport, error) {
	prepared, err := b.prepare(ctx, ctx, p)
	if err != nil {
		return sandbox.ExitReport{}, err
	}
	if controls == nil {
		controls = make(chan control)
	}
	return prepared.execute(ctx, p.timeout, stdout, stderr, controls)
}

// stream serves one sandbox over an upgraded connection. The connection is the sandbox's lifeline: if it closes,
// the container is killed and removed.
func (b *Broker) stream(ctx context.Context, s *prepared, p plan, conn net.Conn, reader *bufio.Reader) {
	out := sandbox.NewFrameWriter(conn)
	controls := make(chan control)
	lifeline, cut := context.WithCancel(ctx)
	defer cut()
	go func() {
		defer cut()
		for {
			kind, payload, err := sandbox.ReadFrame(reader)
			if err != nil {
				return
			}
			var msg control
			switch kind {
			case sandbox.FrameStdin:
				if !p.stdin {
					continue
				}
				msg.stdin = payload
			case sandbox.FrameStdinEOF:
				msg.eof = true
			case sandbox.FrameSignal:
				msg.signal = string(payload)
				if msg.signal != signalTerm && msg.signal != signalKill {
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
	report, err := s.execute(lifeline, p.timeout, forward(sandbox.FrameStdout), forward(sandbox.FrameStderr), controls)
	if err != nil {
		report = sandbox.ExitReport{Error: err.Error()}
	}
	if lifeline.Err() != nil && report.Error == "Sandbox stream closed" {
		return
	}
	payload, _ := json.Marshal(report)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = out.Frame(sandbox.FrameExit, payload)
}
