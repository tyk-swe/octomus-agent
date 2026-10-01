package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	whatwg "github.com/nlnwa/whatwg-url/url"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// Host runs untrusted children directly on this host with the service user's permissions. It isolates nothing; it
// exists for the explicit --sandbox off mode on a dedicated VM and for tests.
type Host struct{}

func (Host) Mode() Mode { return ModeOff }

const stderrWaitDelay = 2 * time.Second

func (Host) Start(ctx context.Context, spec Spec) (Child, error) {
	if ctx.Err() != nil {
		return nil, process.ErrSessionCancelled
	}
	binary, args := spec.Binary, wire.RunnerArgs(runnerName(spec.Runner))
	switch spec.Kind {
	case KindRunner:
		if args == nil {
			return nil, errors.New("Invalid backend")
		}
	case KindVerify:
		binary, args = wire.VerifyProgram(spec.Command)
	default:
		return nil, fmt.Errorf("The host backend cannot run %s sandboxes", spec.Kind)
	}
	started, err := process.StartHost(binary, args, spec.Dir, spec.Env, spec.Stdin)
	if err != nil {
		if spec.Kind == KindVerify {
			return nil, fmt.Errorf("Could not start %s: %w", binary, err)
		}
		return nil, &StartError{err}
	}
	child := &hostChild{HostChild: started, copied: make(chan struct{})}
	if spec.Stderr == nil {
		close(child.copied)
		return child, nil
	}
	go func() {
		defer close(child.copied)
		defer started.Stderr().Close()
		_, _ = io.Copy(spec.Stderr, started.Stderr())
	}()
	return child, nil
}

type hostChild struct {
	*process.HostChild
	copied chan struct{}
}

func (h *hostChild) Stdin() DeadlineWriter {
	if f := h.HostChild.Stdin(); f != nil {
		return f
	}
	return nil
}

// Wait also bounds how long an escaped grandchild may hold a copied stderr open, as exec's WaitDelay does.
func (h *hostChild) Wait() (process.Status, error) {
	status, err := h.HostChild.Wait()
	select {
	case <-h.copied:
	case <-time.After(stderrWaitDelay):
		h.HostChild.Stderr().Close()
	}
	return status, err
}

func (h Host) StartOpenCode(ctx context.Context, spec Spec, readinessSeconds uint64) (*OpenCodeServer, error) {
	child, err := h.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	stdout := child.Stdout()
	lines := bufio.NewReaderSize(stdout, readyLineLimit+1)
	found := make(chan error, 1)
	var base string
	go func() {
		var err error
		base, err = awaitReadyLine(lines)
		found <- err
	}()
	_, err = process.Bounded(ctx, readinessSeconds, "OpenCode startup timed out", func(wctx context.Context) (struct{}, error) {
		select {
		case err := <-found:
			return struct{}{}, err
		case <-wctx.Done():
			return struct{}{}, wctx.Err()
		}
	})
	if err != nil {
		child.Kill()
		stdout.Close()
		_, _ = child.Wait()
		return nil, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, lines)
	}()
	return &OpenCodeServer{Base: base, Transport: LoopbackTransport(), Child: child, Drained: drained}, nil
}

const (
	readyLineLimit = 16_384
	readyLineCount = 1000
	readyPrefix    = "opencode server listening on "
)

func awaitReadyLine(r *bufio.Reader) (string, error) {
	for range readyLineCount {
		line, err := readBoundedLine(r)
		if len(line) > 0 || err == nil {
			if endpoint, ok := strings.CutPrefix(string(line), readyPrefix); ok {
				return ParseLoopbackURL(strings.TrimSpace(endpoint))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", errors.New("OpenCode exited before server readiness")
			}
			return "", err
		}
	}
	return "", errors.New("OpenCode exceeded the startup output limit")
}

func readBoundedLine(r *bufio.Reader) ([]byte, error) {
	chunk, err := r.ReadSlice('\n')
	line := bytes.TrimSuffix(chunk, []byte("\n"))
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > readyLineLimit {
		return nil, fmt.Errorf("line exceeds the %d byte protocol limit", readyLineLimit)
	}
	return bytes.Clone(line), err
}

// ParseLoopbackURL accepts only the plain loopback address a runner server reports once it is listening.
func ParseLoopbackURL(endpoint string) (string, error) {
	u, err := whatwg.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("Invalid OpenCode server address: %w", err)
	}
	port, perr := strconv.Atoi(u.Port())
	authority := endpoint
	if i := strings.Index(authority, "://"); i >= 0 {
		authority = authority[i+3:]
	}
	if i := strings.IndexByte(authority, '/'); i >= 0 {
		authority = authority[:i]
	}
	if u.Scheme() != "http" || u.Hostname() != "127.0.0.1" || perr != nil || port <= 0 ||
		u.Username() != "" || u.Password() != "" || strings.Contains(authority, "@") ||
		u.Pathname() != "/" || strings.ContainsAny(endpoint, "?#") {
		return "", fmt.Errorf("OpenCode did not bind to a local server address")
	}
	return fmt.Sprintf("http://127.0.0.1:%s", u.Port()), nil
}

var loopback = sync.OnceValue(func() *http.Transport {
	return &http.Transport{
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	}
})

// LoopbackTransport reaches servers on this host's loopback and never consults a proxy.
func LoopbackTransport() http.RoundTripper { return loopback().Clone() }

func (Host) RunnerVersion(ctx context.Context, spec Spec, seconds uint64) (string, error) {
	return process.RunMachine(ctx, spec.Binary, []string{"--version"}, spec.Dir, seconds)
}
