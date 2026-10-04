// The containment probe's checks from inside the sandbox: read-only image proof and egress refusals through the real gateway.

package sandbox

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/egress"
)

const (
	writableRoot = "1015 980 0:77 / / rw,relatime master:500 - overlay overlay rw,lowerdir=/l\n" +
		"1016 1015 0:80 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw\n"
	readOnlyRoot = "1015 980 0:77 / / ro,relatime master:500 - overlay overlay rw,lowerdir=/l\n" +
		"1017 1015 0:81 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw,size=65536k\n" +
		"1018 1015 8:1 /hosts /etc/hosts rw,relatime - ext4 /dev/sda1 rw\n"
	// gVisor's root mount, under which a write is refused for permission before the read-only mount is consulted.
	gVisorRoot = "1 0 0:1 / / ro - 9p none ro,trans=fd,rfdno=4,wfdno=4\n"
	// A writable mount over /usr hides under a read-only root from a probe that cannot write root-owned directories.
	writableUsr = readOnlyRoot + "1019 1015 0:90 / /usr rw,relatime - overlay overlay rw\n"
)

// A probe that runs as a non-root user cannot write to root-owned /usr, /etc or / on a writable image either. Only
// the ro option of the mount that holds each directory proves it read-only, and a refusal other than EROFS counts only
// under such a mount.
func TestReadOnlyImage(t *testing.T) {
	refuse := func(errno syscall.Errno) func(string) error {
		return func(path string) error { return &fs.PathError{Op: "open", Path: path, Err: errno} }
	}
	mountinfo := func(text string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if path != "/proc/self/mountinfo" {
				return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
			}
			return []byte(text), nil
		}
	}
	denied := func(path string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	cases := []struct {
		name   string
		read   func(string) ([]byte, error)
		write  func(string) error
		passed bool
		detail string
	}{
		{"read-only image", mountinfo(readOnlyRoot), refuse(syscall.EROFS), true, "read-only"},
		{"writable image, non-root probe", mountinfo(writableRoot), refuse(syscall.EACCES), false, "mount / rw,relatime"},
		{"gVisor refuses for permission first", mountinfo(gVisorRoot), refuse(syscall.EACCES), true, "read-only"},
		{"writable mount over /usr", mountinfo(writableUsr), refuse(syscall.EACCES), false, "not proven read-only: /usr refused without EROFS (permission denied) under mount /usr rw,relatime"},
		{"ro mount with another refusal", mountinfo(readOnlyRoot), refuse(syscall.EIO), false, "/usr refused without EROFS (input/output error)"},
		{"no root mount", mountinfo(""), refuse(syscall.EROFS), false, "no root mount in mountinfo"},
		{"unreadable mountinfo", denied, refuse(syscall.EROFS), false, "not proven read-only: mountinfo unreadable (permission denied)"},
		{"writable directories", mountinfo(writableRoot), func(string) error { return nil }, false, "writable /usr, writable /etc, writable /"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			passed, detail := readOnlyImage(c.read, c.write)
			if passed != c.passed || !strings.Contains(detail, c.detail) {
				t.Fatalf("readOnlyImage = %v, %q; want %v with %q", passed, detail, c.passed, c.detail)
			}
		})
	}
	written := []string{}
	readOnlyImage(mountinfo(readOnlyRoot), func(path string) error { written = append(written, path); return syscall.EROFS })
	if strings.Join(written, " ") != "/usr/.octomus-probe /etc/.octomus-probe /.octomus-probe" {
		t.Fatalf("probe wrote %v", written)
	}
}

// probeGateway serves the real egress gateway with one lease of the given kind and returns the proxy address a
// sandbox would be given. Nothing resolves or dials: a target that gets past the allowlist fails at resolution.
func probeGateway(t *testing.T, kind, model, build string) (string, string) {
	t.Helper()
	leases := t.TempDir()
	token, err := egress.Leases{Dir: leases}.Grant("octomus-test-probe", kind)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := egress.ParseRules(model)
	if err != nil {
		t.Fatal(err)
	}
	buildRules, err := egress.ParseRules(build)
	if err != nil {
		t.Fatal(err)
	}
	gateway := egress.New(egress.Policy{Model: rules, Build: buildRules}, leases, io.Discard).WithNetwork(noResolver{},
		func(context.Context, netip.AddrPort) (net.Conn, error) { return nil, errors.New("no network in tests") })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: gateway}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })
	return listener.Addr().String(), token
}

type noResolver struct{}

func (noResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("no DNS in tests")
}

// The probe sits on the runner network, so its refusals must come from the runner allowlist itself. A lease the
// gateway gives nothing, or a credential it does not accept, refuses everything and proves nothing about that list.
func TestProbeEgressRefusals(t *testing.T) {
	cases := []struct {
		name, kind, model, build, target string
		credential                       func(token string) string
		refused                          bool
		detail                           string
	}{
		{"runner lease", "runner", "api.openai.com", "", "example.com:443", func(token string) string { return "sandbox:" + token }, true,
			"refused example.com:443, 169.254.169.254:80, localhost:4200"},
		{"probe lease the gateway gives nothing", "probe", "api.openai.com", "", "example.com:443", func(token string) string { return "sandbox:" + token },
			false, "not proven refused: example.com:443 (HTTP 403 for another reason: Octomus egress blocked this connection: host is not on the probe allowlist)"},
		{"unknown credential", "runner", "api.openai.com", "", "example.com:443", func(string) string { return "sandbox:" + strings.Repeat("ef", 32) },
			false, "not proven refused: example.com:443 (the gateway refused the probe's credential, HTTP 407)"},
		{"model allows the old fixed target", "runner", "example.com", "", "octomus-probe-0.invalid:443", func(token string) string { return "sandbox:" + token },
			true, "refused octomus-probe-0.invalid:443, 169.254.169.254:80, localhost:4200"},
		{"build allows the old fixed target", "runner", "", "example.com", "octomus-probe-0.invalid:443", func(token string) string { return "sandbox:" + token },
			true, "refused octomus-probe-0.invalid:443, 169.254.169.254:80, localhost:4200"},
		{"allowlists include reserved names", "runner", "example.com,*.probe.invalid", "octomus-probe-0.invalid", "octomus-probe-1.invalid:443",
			func(token string) string { return "sandbox:" + token }, true,
			"refused octomus-probe-1.invalid:443, 169.254.169.254:80, localhost:4200"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			address, token := probeGateway(t, c.kind, c.model, c.build)
			refused, detail := egressRefusals("http://"+c.credential(token)+"@"+address, c.target)
			if refused != c.refused || !strings.Contains(detail, c.detail) {
				t.Fatalf("egress refusals = %v, %q; want %v with %q", refused, detail, c.refused, c.detail)
			}
		})
	}
}
