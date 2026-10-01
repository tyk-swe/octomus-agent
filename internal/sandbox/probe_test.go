package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	writableRoot = "1015 980 0:77 / / rw,relatime master:500 - overlay overlay rw,lowerdir=/l\n" +
		"1016 1015 0:80 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw\n"
	readOnlyRoot = "1015 980 0:77 / / ro,relatime master:500 - overlay overlay rw,lowerdir=/l\n" +
		"1017 1015 0:81 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw,size=65536k\n"
)

// A probe that runs as a non-root user cannot write to root-owned /usr, /etc or / on a writable image either. Only
// the root mount's ro option and EROFS prove the image is read-only.
func TestReadOnlyImageNeedsTheRootMountAndEROFS(t *testing.T) {
	refuse := func(errno syscall.Errno) func(string) error {
		return func(path string) error { return &fs.PathError{Op: "open", Path: path, Err: errno} }
	}
	cases := []struct {
		name      string
		mountinfo string
		write     func(string) error
		passed    bool
		detail    string
	}{
		{"read-only image", readOnlyRoot, refuse(syscall.EROFS), true, "read-only"},
		{"writable image, non-root probe", writableRoot, refuse(syscall.EACCES), false, "mount / rw,relatime"},
		{"ro mount without EROFS", readOnlyRoot, refuse(syscall.EACCES), false, "/usr refused without EROFS (permission denied)"},
		{"no mountinfo", "", refuse(syscall.EROFS), false, "no root mount in mountinfo"},
		{"writable directories", writableRoot, func(string) error { return nil }, false, "writable /usr, writable /etc, writable /"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			passed, detail := readOnlyImage([]byte(c.mountinfo), c.write)
			if passed != c.passed || !strings.Contains(detail, c.detail) {
				t.Fatalf("readOnlyImage = %v, %q; want %v with %q", passed, detail, c.passed, c.detail)
			}
		})
	}
	written := []string{}
	readOnlyImage([]byte(readOnlyRoot), func(path string) error { written = append(written, path); return syscall.EROFS })
	if strings.Join(written, " ") != "/usr/.octomus-probe /etc/.octomus-probe /.octomus-probe" {
		t.Fatalf("probe wrote %v", written)
	}
}

// A refused connection was answered by whoever refused it, so it is a route, not the absence of one.
func TestReachableCountsARefusalAsAnAnswer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	open := listener.Addr().String()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().String()
	closed.Close()
	defer listener.Close()
	silent := func(ctx context.Context, address string) error {
		if address == "192.0.2.1:443" {
			<-ctx.Done()
			return ctx.Err()
		}
		if address == "192.0.2.2:443" {
			return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
		}
		return dialTCP(ctx, address)
	}
	got := reachable([]string{"192.0.2.1:443", open, refused, "192.0.2.2:443"}, time.Second, silent)
	if strings.Join(got, " ") != open+" "+refused {
		t.Fatalf("reachable = %v; want the open and the refusing port only", got)
	}
}

func hostTables(route, route6 string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		switch {
		case path == "/proc/net/route" && route != "":
			return []byte(route), nil
		case path == "/proc/net/ipv6_route" && route6 != "":
			return []byte(route6), nil
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
}

const (
	routeHeader = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	// Docker's internal network 172.19.0.0/16: one on-link route and no default route, isolated gateway or not.
	internalRoute = routeHeader + "eth0\t000013AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	defaultRoute  = routeHeader + "eth0\t00000000\t010013AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t000013AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	loopbackRoutes6 = "00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n" +
		"00000000000000000000000000000001 80 00000000000000000000000000000000 00 00000000000000000000000000000000 00000000 00000003 00000000 80200001       lo\n"
)

type dialScript map[string]error

func (s dialScript) dial(ctx context.Context, address string) error {
	if err, ok := s[address]; ok {
		return err
	}
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
}

var refusedErr = &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

func noContainer(netip.Addr) string { return "" }

// Docker never installs a default route on an internal network, so its absence proves nothing. A plain internal
// network leaves the host's bridge address on-link, and the probe must reach for it to know.
func TestHostIsolationReachesForTheBridgeGateway(t *testing.T) {
	own := []netip.Addr{netip.MustParseAddr("172.19.0.3")}
	cases := []struct {
		name      string
		read      func(string) ([]byte, error)
		dial      dialScript
		container func(netip.Addr) string
		passed    bool
		detail    string
	}{
		{"plain internal network: host SSH answers", hostTables(internalRoute, loopbackRoutes6),
			dialScript{"172.19.0.1:22": nil, "172.19.0.1:80": refusedErr}, noContainer, false, "reached 172.19.0.1:22, 172.19.0.1:80"},
		{"plain internal network: every host port refuses", hostTables(internalRoute, ""),
			dialScript{"172.19.0.1:4200": refusedErr}, noContainer, false, "reached 172.19.0.1:4200"},
		{"isolated gateway: nothing answers", hostTables(internalRoute, loopbackRoutes6), dialScript{}, noContainer,
			true, "no default route; no answer from 172.19.0.1"},
		{"isolated gateway: a neighbouring container holds the first address", hostTables(internalRoute, ""),
			dialScript{"172.19.0.1:443": refusedErr}, func(netip.Addr) string { return "egress.octomus-sandbox-runner" },
			true, "172.19.0.1 is the container egress.octomus-sandbox-runner"},
		{"default route", hostTables(defaultRoute, ""), dialScript{}, noContainer, false, "default route via eth0 172.19.0.1"},
		{"unreadable routing table", hostTables("", ""), dialScript{}, noContainer, false, "routing table unreadable"},
		{"unexpected routing table", hostTables("garbage\n", ""), dialScript{}, noContainer, false, "routing table unreadable"},
		{"IPv6 default route", hostTables(internalRoute,
			"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0\n"),
			dialScript{}, noContainer, false, "default route via eth0 fe80::1"},
		{"IPv6 on-link subnet", hostTables(internalRoute,
			"20010db8000100000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0\n"),
			dialScript{"[2001:db8:1::1]:22": nil}, noContainer, false, "reached [2001:db8:1::1]:22"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			passed, detail := hostIsolation(c.read, own, c.dial.dial, c.container)
			if passed != c.passed || !strings.Contains(detail, c.detail) {
				t.Fatalf("hostIsolation = %v, %q; want %v with %q", passed, detail, c.passed, c.detail)
			}
		})
	}
	// The sandbox's own address is never the host's, even when it is the first one in its subnet.
	first := []netip.Addr{netip.MustParseAddr("172.19.0.1")}
	passed, detail := hostIsolation(hostTables(internalRoute, ""), first, dialScript{"172.19.0.1:22": nil}.dial, noContainer)
	if !passed || detail != "no default route; the sandbox itself holds 172.19.0.1" {
		t.Fatalf("hostIsolation with its own address first = %v, %q", passed, detail)
	}
	denied := func(path string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
	}
	if passed, detail := hostIsolation(denied, own, dialScript{}.dial, noContainer); passed || !strings.Contains(detail, "permission denied") {
		t.Fatalf("hostIsolation without a routing table = %v, %q; want a failure", passed, detail)
	}
}

// pids.max is a number on every systemd host whether or not the broker set a limit, so only the broker's own
// configured limits can confirm the check.
func TestConfirmLimitsHoldsTheProbeToTheBrokersLimits(t *testing.T) {
	report := func(memory, pids string) ProbeReport {
		return ProbeReport{
			Checks: []ProbeCheck{{ID: "non_root", Passed: true}, {ID: "resource_limits", Passed: limited(memory) && limited(pids),
				Detail: "memory.max " + memory + ", pids.max " + pids}},
			Limits: ProbeLimits{Memory: memory, Pids: pids},
		}
	}
	want := &BrokerLimits{Memory: 256 << 20, Pids: 256}
	cases := []struct {
		name         string
		memory, pids string
		want         *BrokerLimits
		passed       bool
		detail       string
	}{
		{"broker limits in force", "268435456", "256", want, true, "memory.max 268435456, pids.max 256"},
		{"a stricter outer cgroup", "268431360", "100", want, true, "pids.max 100"},
		{"systemd's pids limit only", "268435456", "25519", want, false, "pids.max 25519 exceeds the configured 256"},
		{"no memory limit", "max", "256", want, false, "memory.max max exceeds the configured 268435456"},
		{"unknown broker limits", "268435456", "256", nil, false, "the broker's configured limits are unknown: broker gone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := report(c.memory, c.pids)
			r.confirmLimits(c.want, errors.New("broker gone"))
			check := r.Checks[1]
			if check.Passed != c.passed || !strings.Contains(check.Detail, c.detail) || r.Passed() != c.passed {
				t.Fatalf("resource_limits = %v, %q; want %v with %q", check.Passed, check.Detail, c.passed, c.detail)
			}
		})
	}
}
