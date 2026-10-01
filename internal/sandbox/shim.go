package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// RunInit is the helper the broker installs into every sandbox (octomus-agent --sandbox-init). It runs inside the
// container, as the sandbox user, and only relays: it holds no credential and trusts nothing it reads.
func RunInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Error: --sandbox-init needs a mode")
		return 2
	}
	switch args[0] {
	case wire.RunnerModeOpenCode:
		return runOpenCodeBridge(args[1:], stdin, stdout, stderr)
	case wire.ProbeVersions:
		return printVersions(stdout, stderr)
	case wire.ProbeContainment:
		return runContainmentProbe(stdout)
	}
	fmt.Fprintf(stderr, "Error: unknown --sandbox-init mode %q\n", args[0])
	return 2
}

// runOpenCodeBridge starts OpenCode on the sandbox's loopback, reports one handshake line, then serves the OpenCode
// API as HTTP/2 over this process's stdin and stdout. Closing stdin ends the bridge and the server.
func runOpenCodeBridge(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 3 || args[1] != "--" {
		fmt.Fprintln(stderr, "Error: usage: --sandbox-init opencode SECONDS -- PROGRAM [ARGS...]")
		return 2
	}
	seconds, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil || seconds == 0 {
		fmt.Fprintln(stderr, "Error: invalid readiness seconds")
		return 2
	}
	verdict := func(line string) { _, _ = io.WriteString(stdout, line+"\n") }
	cmd := exec.Command(args[2], args[3:]...)
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	serverOut, err := cmd.StdoutPipe()
	if err != nil {
		verdict(handshakeFailed + "Could not start OpenCode: " + err.Error())
		return 3
	}
	if err := cmd.Start(); err != nil {
		verdict(handshakeFailed + "Could not start OpenCode: " + err.Error())
		return 3
	}
	stop := func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.AfterFunc(2*time.Second, func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
		_ = cmd.Wait()
		timer.Stop()
	}
	lines := bufio.NewReaderSize(serverOut, readyLineLimit+1)
	found := make(chan error, 1)
	var base string
	go func() {
		var err error
		base, err = awaitReadyLine(lines)
		found <- err
	}()
	select {
	case err = <-found:
	case <-time.After(time.Duration(seconds) * time.Second):
		err = errors.New("OpenCode startup timed out")
	}
	if err != nil {
		verdict(handshakeFailed + strings.ReplaceAll(err.Error(), "\n", " "))
		stop()
		return 3
	}
	go func() { _, _ = io.Copy(io.Discard, lines) }()
	target, err := url.Parse(base)
	if err != nil {
		verdict(handshakeFailed + err.Error())
		stop()
		return 3
	}
	verdict(handshakeReady)
	proxy := &httputil.ReverseProxy{
		Rewrite:       func(r *httputil.ProxyRequest) { r.SetURL(target) },
		Transport:     LoopbackTransport(),
		FlushInterval: -1,
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	served := make(chan struct{})
	go func() {
		defer close(served)
		serveStream(&stdioConn{r: stdin, w: stdout}, proxy)
	}()
	select {
	case <-served:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		return 0
	case err := <-exited:
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			return exit.ExitCode()
		}
		if err != nil {
			return 1
		}
		return 0
	}
}

// serveStream answers HTTP/2 without TLS on one already-open connection and returns when that connection closes.
func serveStream(conn net.Conn, handler http.Handler) {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	listener := &singleListener{conns: make(chan net.Conn, 1), done: make(chan struct{})}
	listener.conns <- conn
	server := &http.Server{
		Handler:   handler,
		Protocols: protocols,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				listener.Close()
			}
		},
	}
	_ = server.Serve(listener)
}

type singleListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *singleListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *singleListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *singleListener) Addr() net.Addr { return &net.UnixAddr{Name: "stdio", Net: "unix"} }

// stdioConn is the sandbox side of the one connection the control plane has into it.
type stdioConn struct {
	r io.Reader
	w io.Writer
}

func (c *stdioConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error)      { return c.w.Write(p) }
func (c *stdioConn) Close() error                     { return nil }
func (c *stdioConn) LocalAddr() net.Addr              { return &net.UnixAddr{Name: "sandbox", Net: "unix"} }
func (c *stdioConn) RemoteAddr() net.Addr             { return &net.UnixAddr{Name: "control", Net: "unix"} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

// versionTimeout bounds each runner's --version. Both runners together stay well inside the broker's 120 second limit
// on the version probe.
var versionTimeout = 30 * time.Second

// printVersions reports each runner's --version output from inside the sandbox image. A runner that is not installed
// is left out; one that is installed but fails is also left out, and the failure is written to stderr for the broker
// to surface. The broker does not read it yet: it drops a successful probe's stderr, so such a runner still shows as
// not installed.
func printVersions(stdout, stderr io.Writer) int {
	versions := map[string]string{}
	for _, name := range wire.Runners {
		version, err := runnerVersion(name, versionTimeout)
		switch {
		case err == nil:
			versions[name] = version
		case !errors.Is(err, exec.ErrNotFound):
			fmt.Fprintf(stderr, "%s --version failed: %s\n", name, err)
		}
	}
	if err := json.NewEncoder(stdout).Encode(versions); err != nil {
		return 1
	}
	return 0
}

// runnerVersion runs one runner's --version in a process group of its own. The runners are launchers that start a
// native binary sharing their stdout, so the timeout kills the whole group, and WaitDelay bounds how long anything
// that escaped it may hold the output open.
func runnerVersion(name string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if cmd.Process != nil {
		// Whatever the launcher left behind in its group goes with it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if detail := strings.Join(strings.Fields(string(exit.Stderr)), " "); detail != "" {
				if len(detail) > 200 {
					detail = detail[len(detail)-200:]
				}
				return "", fmt.Errorf("%w: %s", err, strings.ToValidUTF8(detail, "�"))
			}
		}
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if len(version) > 200 {
		version = version[:200]
	}
	return version, nil
}
