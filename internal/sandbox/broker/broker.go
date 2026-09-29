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

// New checks the daemon, image, networks and volumes, installs the broker's executable for sandboxes and removes any
// sandbox a previous broker left behind. It refuses to serve a deployment that would weaken isolation.
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
	if !apiAtLeast(version.APIVersion, engineapi.APIVersion) {
		return nil, fmt.Errorf("Docker Engine API %s is older than %s; upgrade Docker Engine", version.APIVersion, engineapi.APIVersion)
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
		if cfg.RequireIsolatedGateway && network.Options[isolatedGateway] != "isolated" {
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
	if err := b.sweep(ctx); err != nil {
		return nil, err
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
// touched; everything else on the host is not the broker's to manage.
func (b *Broker) sweep(ctx context.Context) error {
	containers, err := b.engine.ContainerList(ctx, map[string]string{instanceLabel: b.cfg.Instance})
	if err != nil {
		return fmt.Errorf("Listing leftover sandboxes: %w", err)
	}
	for _, container := range containers {
		if container.Labels[instanceLabel] != b.cfg.Instance {
			continue
		}
		if err := b.engine.ContainerRemove(ctx, container.ID); err != nil {
			return fmt.Errorf("Removing leftover sandbox %s: %w", container.ID, err)
		}
	}
	if b.leases != nil {
		return b.leases.clear()
	}
	return nil
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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, b.Info())
	})
	mux.HandleFunc("POST /v1/sandboxes", b.handleSandbox)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = b.sweep(shutdown)
		return nil
	case err := <-done:
		return err
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (b *Broker) handleSandbox(w http.ResponseWriter, r *http.Request) {
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
	prepared, err := b.prepare(r.Context(), p)
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
	b.stream(prepared, p, conn, stream.Reader)
}

// prepared is a created container with its attach stream and exit wait already registered, not yet started.
type prepared struct {
	b       *Broker
	id      string
	name    string
	lease   string
	attach  *engineapi.Attached
	waitRes <-chan engineapi.WaitResult
	waitErr <-chan error
	cancel  context.CancelFunc
	once    sync.Once
}

func (b *Broker) prepare(ctx context.Context, p plan) (*prepared, error) {
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := fmt.Sprintf("octomus-%s-%s-%s", b.cfg.Instance, p.kind, hex.EncodeToString(suffix[:]))
	var extraEnv []string
	lease := ""
	if b.leases != nil && (p.kind != sandbox.KindProbe || p.probe == sandbox.ProbeContainment) {
		token, err := b.leases.grant(name, p.kind)
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
	fail := func(err error) (*prepared, error) {
		if lease != "" {
			b.leases.revoke(lease)
		}
		return nil, err
	}
	id, err := b.engine.ContainerCreate(ctx, name, spec)
	if err != nil {
		return fail(fmt.Errorf("Creating the sandbox: %w", err))
	}
	cleanup := &prepared{b: b, id: id, name: name, lease: lease}
	attach, err := b.engine.ContainerAttach(ctx, id, p.stdin)
	if err != nil {
		cleanup.remove()
		return nil, fmt.Errorf("Attaching to the sandbox: %w", err)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	results, errs := b.engine.ContainerWait(waitCtx, id)
	b.mu.Lock()
	b.live[id] = p.rel
	b.mu.Unlock()
	return &prepared{b: b, id: id, name: name, lease: lease, attach: attach, waitRes: results, waitErr: errs, cancel: cancel}, nil
}

func (s *prepared) remove() {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
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
	defer s.remove()
	b := s.b
	attached := make(chan error, 1)
	go func() {
		attached <- engineapi.Demux(s.attach.Reader, stdout, stderr)
	}()
	if err := b.engine.ContainerStart(ctx, s.id); err != nil {
		return sandbox.ExitReport{}, fmt.Errorf("Starting the sandbox: %w", err)
	}
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
		select {
		case result = <-s.waitRes:
			break wait
		case err := <-s.waitErr:
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
			case msg.stdin != nil:
				if _, err := s.attach.Conn.Write(msg.stdin); err != nil {
					_ = s.attach.CloseStdin()
				}
			case msg.eof:
				_ = s.attach.CloseStdin()
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
	// A tunnel is recorded when it closes, just after the container's processes exit.
	time.Sleep(300 * time.Millisecond)
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
	prepared, err := b.prepare(ctx, p)
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
func (b *Broker) stream(s *prepared, p plan, conn net.Conn, reader *bufio.Reader) {
	out := sandbox.NewFrameWriter(conn)
	controls := make(chan control)
	lifeline, cut := context.WithCancel(context.Background())
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
