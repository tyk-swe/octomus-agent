package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"strconv"
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
		{"ro mount without EROFS", mountinfo(readOnlyRoot), refuse(syscall.EACCES), false, "/usr refused without EROFS (permission denied)"},
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

func hostTables(route, route6, arp string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		switch {
		case path == "/proc/net/route" && route != "":
			return []byte(route), nil
		case path == "/proc/net/ipv6_route" && route6 != "":
			return []byte(route6), nil
		case path == "/proc/net/arp" && arp != "":
			return []byte(arp), nil
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
	arpHeader = "IP address       HW type     Flags       HW address            Mask     Device\n"
	// The dials ask who holds 172.19.0.1. The host's bridge answers even when its firewall drops every port; nobody
	// answers for an isolated gateway's free address.
	arpAnswered     = arpHeader + "172.19.0.1       0x1         0x2         02:42:ac:13:00:01     *        eth0\n"
	arpUnanswered   = arpHeader + "172.19.0.1       0x1         0x0         00:00:00:00:00:00     *        eth0\n"
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
		{"plain internal network: host SSH answers", hostTables(internalRoute, loopbackRoutes6, arpAnswered),
			dialScript{"172.19.0.1:22": nil, "172.19.0.1:80": refusedErr}, noContainer, false, "reached 172.19.0.1:22, 172.19.0.1:80"},
		{"plain internal network: every host port refuses", hostTables(internalRoute, "", arpAnswered),
			dialScript{"172.19.0.1:4200": refusedErr}, noContainer, false, "reached 172.19.0.1:4200"},
		{"plain internal network: the host's firewall drops every port", hostTables(internalRoute, "", arpAnswered),
			dialScript{}, noContainer, false, "reached 172.19.0.1 (answered ARP)"},
		{"isolated gateway: nothing answers", hostTables(internalRoute, loopbackRoutes6, arpUnanswered), dialScript{}, noContainer,
			true, "no default route; no answer from 172.19.0.1"},
		{"isolated gateway: the kernel gave up on the free address", hostTables(internalRoute, "", arpHeader), dialScript{},
			noContainer, true, "no default route; no answer from 172.19.0.1"},
		{"isolated gateway: a neighbouring container holds the first address", hostTables(internalRoute, "", arpAnswered),
			dialScript{"172.19.0.1:443": refusedErr}, func(netip.Addr) string { return "egress.octomus-sandbox-runner" },
			true, "172.19.0.1 is the container egress.octomus-sandbox-runner"},
		{"isolated gateway: a silent neighbouring container holds the first address", hostTables(internalRoute, "", arpAnswered),
			dialScript{}, func(netip.Addr) string { return "egress.octomus-sandbox-runner" },
			true, "172.19.0.1 is the container egress.octomus-sandbox-runner"},
		{"unreadable ARP table", hostTables(internalRoute, "", ""), dialScript{}, noContainer, false, "ARP table unreadable"},
		{"unexpected ARP table", hostTables(internalRoute, "", "garbage\n"), dialScript{}, noContainer, false, "ARP table unreadable"},
		{"default route", hostTables(defaultRoute, "", ""), dialScript{}, noContainer, false, "default route via eth0 172.19.0.1"},
		{"unreadable routing table", hostTables("", "", arpAnswered), dialScript{}, noContainer, false, "routing table unreadable"},
		{"unexpected routing table", hostTables("garbage\n", "", arpAnswered), dialScript{}, noContainer, false, "routing table unreadable"},
		{"IPv6 default route", hostTables(internalRoute,
			"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0\n",
			arpUnanswered), dialScript{}, noContainer, false, "default route via eth0 fe80::1"},
		{"IPv6 on-link subnet", hostTables(internalRoute,
			"20010db8000100000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     eth0\n",
			arpUnanswered), dialScript{"[2001:db8:1::1]:22": nil}, noContainer, false, "reached [2001:db8:1::1]:22"},
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
	passed, detail := hostIsolation(hostTables(internalRoute, "", ""), first, dialScript{"172.19.0.1:22": nil}.dial, noContainer)
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

// pids.max is a number on every systemd host whether or not the broker set a limit, and the probe's own cgroup holds
// exactly what Docker set, so only a match with the broker's configured limits confirms the check.
func TestConfirmLimitsHoldsTheProbeToTheBrokersLimits(t *testing.T) {
	report := func(memory, pids string) ProbeReport {
		return ProbeReport{
			Checks: []ProbeCheck{{ID: "non_root", Passed: true}, {ID: "resource_limits", Passed: limited(memory) && limited(pids),
				Detail: "memory.max " + memory + ", pids.max " + pids}},
			Limits: ProbeLimits{Memory: memory, Pids: pids},
		}
	}
	want := &BrokerLimits{Memory: 256 << 20, Pids: 256}
	page := int64(os.Getpagesize())
	unaligned := &BrokerLimits{Memory: 256<<20 + page/2, Pids: 256}
	large := &BrokerLimits{Memory: 256 << 20, Pids: 32768}
	cases := []struct {
		name         string
		memory, pids string
		want         *BrokerLimits
		passed       bool
		detail       string
	}{
		{"broker limits in force", "268435456", "256", want, true, "memory.max 268435456, pids.max 256"},
		{"memory rounded down to whole pages", "268435456", "256", unaligned, true, "memory.max 268435456"},
		{"systemd's pids limit only", "268435456", "25519", want, false, "pids.max 25519, configured 256"},
		{"systemd's pids limit under a higher configured one", "268435456", "25519", large, false, "pids.max 25519, configured 32768"},
		{"a lower memory limit than the broker's", strconv.FormatInt(256<<20-page, 10), "256", want, false,
			"memory.max " + strconv.FormatInt(256<<20-page, 10) + ", configured 268435456"},
		{"no memory limit", "max", "256", want, false, "memory.max max, not a numeric limit"},
		{"unreadable pids limit", "268435456", "unreadable", want, false, "pids.max unreadable, not a numeric limit"},
		{"no configured pids limit", "268435456", "256", &BrokerLimits{Memory: 256 << 20}, false,
			"pids.max 256, but the broker reports no configured limit"},
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
