package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// probeTargets are paths a sandbox must never see: orchestrator state, deployment secrets and the Docker daemon.
var probeTargets = []string{
	"/var/lib/octomus/data/state.db", "/run/secrets", "/var/run/docker.sock", "/run/docker.sock", "/run/octomus",
}

// runContainmentProbe observes, from inside the sandbox, every boundary the deployment promises.
func runContainmentProbe(stdout io.Writer) int {
	report := ProbeReport{}
	if kernel, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		report.Kernel = strings.TrimSpace(string(kernel))
	}
	add := func(id, label string, passed bool, detail string) {
		report.Checks = append(report.Checks, ProbeCheck{ID: id, Label: label, Passed: passed, Detail: detail})
	}
	status := procStatus()
	add("non_root", "Runs as an unprivileged user", os.Getuid() != 0 && os.Geteuid() != 0,
		fmt.Sprintf("uid %d", os.Getuid()))
	zero := "0000000000000000"
	add("no_capabilities", "Holds no Linux capabilities",
		status["CapEff"] == zero && status["CapPrm"] == zero && status["CapBnd"] == zero,
		fmt.Sprintf("effective %s, bounding %s", status["CapEff"], status["CapBnd"]))
	add("no_new_privileges", "Cannot gain privileges through setuid programs", status["NoNewPrivs"] == "1",
		"no_new_privs "+status["NoNewPrivs"])
	add("seccomp", "System calls are filtered by seccomp", status["Seccomp"] == "2", "seccomp mode "+status["Seccomp"])
	readOnly, detail := readOnlyImage(os.ReadFile, tryWrite)
	add("read_only_image", "The image filesystem is read-only", readOnly, detail)
	visible := []string{}
	for _, path := range probeTargets {
		if _, err := os.Lstat(path); err == nil {
			visible = append(visible, path)
		}
	}
	add("no_orchestrator_state", "Cannot see Octomus state, secrets or the Docker socket", len(visible) == 0,
		detailList("visible", visible, "none visible"))
	reached := reachable([]string{"1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443"}, 3*time.Second, dialTCP)
	add("no_direct_egress", "Has no direct route to the internet", len(reached) == 0, detailList("reached", reached, "no route"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	resolved, err := net.DefaultResolver.LookupHost(ctx, "example.com")
	cancel()
	add("no_external_dns", "Cannot resolve internet names directly", err != nil, detailList("resolved", resolved, "lookup refused"))
	isolated, detail := hostIsolation(os.ReadFile, ownAddresses(), dialTCP, containerName)
	add("no_host_route", "Has no gateway to the host or its neighbours", isolated, detail)
	report.Limits = ProbeLimits{Memory: readTrim("/sys/fs/cgroup/memory.max"), Pids: readTrim("/sys/fs/cgroup/pids.max")}
	add("resource_limits", "Runs under memory and process limits", limited(report.Limits.Memory) && limited(report.Limits.Pids),
		fmt.Sprintf("memory.max %s, pids.max %s", report.Limits.Memory, report.Limits.Pids))
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		add("egress_gateway", "Egress is limited to the allowlist", true, "no egress gateway: sandboxes are offline")
	} else {
		refused, detail := egressRefusals(proxy)
		add("egress_gateway", "The egress gateway refuses unlisted, metadata and local targets", refused, detail)
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return 1
	}
	return 0
}

func procStatus() map[string]string {
	fields := map[string]string{}
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return fields
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), ":"); ok {
			fields[key] = strings.TrimSpace(value)
		}
	}
	return fields
}

// readOnlyImage proves the image is mounted read-only: the root mount carries the ro option, and a write to `/`, `/usr`
// and `/etc` fails. The probe user cannot write to those root-owned directories on a writable image either, so a
// refusal counts only from a read-only mount: EROFS says so itself, and a permission refusal counts when the mount
// that holds the directory carries the ro option. gVisor checks permissions before the mount's flags, so there every
// write fails that way; a writable mount over /usr or /etc still fails the check.
func readOnlyImage(read func(string) ([]byte, error), write func(path string) error) (bool, string) {
	problems := []string{}
	mountinfo, err := read("/proc/self/mountinfo")
	if err != nil {
		problems = append(problems, "mountinfo unreadable ("+errnoText(err)+")")
	} else if _, options, ok := holdingMount(mountinfo, "/"); !ok {
		problems = append(problems, "no root mount in mountinfo")
	} else if !readOnlyMount(options) {
		problems = append(problems, "mount / "+options)
	}
	for _, dir := range []string{"/usr", "/etc", "/"} {
		path := strings.TrimSuffix(dir, "/") + "/.octomus-probe"
		err := write(path)
		point, options, held := holdingMount(mountinfo, path)
		switch {
		case err == nil:
			problems = append(problems, "writable "+dir)
		case errors.Is(err, syscall.EROFS):
		case !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) || !held:
			problems = append(problems, dir+" refused without EROFS ("+errnoText(err)+")")
		case !readOnlyMount(options):
			problems = append(problems, fmt.Sprintf("%s refused without EROFS (%s) under mount %s %s", dir, errnoText(err), point, options))
		}
	}
	return len(problems) == 0, detailList("not proven read-only:", problems, "read-only")
}

// holdingMount returns the mount point and per-mount options of the mount that holds path in /proc/self/mountinfo:
// the deepest mount point at or above it, the last one listed when several are stacked.
func holdingMount(mountinfo []byte, path string) (string, string, bool) {
	point, options, found := "", "", false
	for _, line := range strings.Split(string(mountinfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) <= 5 {
			continue
		}
		mount := fields[4]
		holds := mount == "/" || path == mount || strings.HasPrefix(path, mount+"/")
		if holds && len(mount) >= len(point) {
			point, options, found = mount, fields[5], true
		}
	}
	return point, options, found
}

func readOnlyMount(options string) bool { return slices.Contains(strings.Split(options, ","), "ro") }

func tryWrite(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	file.Close()
	_ = os.Remove(path)
	return nil
}

func errnoText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}

// dialFunc opens and closes one TCP connection.
type dialFunc func(ctx context.Context, address string) error

func dialTCP(ctx context.Context, address string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err == nil {
		conn.Close()
	}
	return err
}

// answered reports whether a connection attempt reached something. A refusal is an answer: some host received the
// packet and replied. Only silence, an unreachable host or network, or a timeout shows there is no way through.
func answered(err error) bool {
	return err == nil || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}

// reachable dials every address at once, within timeout, and returns the ones that answered in the order given.
func reachable(addresses []string, timeout time.Duration, dial dialFunc) []string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	answers := make([]bool, len(addresses))
	var wg sync.WaitGroup
	for i, address := range addresses {
		wg.Go(func() { answers[i] = answered(dial(ctx, address)) })
	}
	wg.Wait()
	reached := []string{}
	for i, address := range addresses {
		if answers[i] {
			reached = append(reached, address)
		}
	}
	return reached
}

// hostPorts are where host services commonly listen: SSH, DNS, the web, the Docker API and the Octomus dashboard.
var hostPorts = []uint16{22, 53, 80, 443, 2375, 2376, 4200}

// hostIsolation proves the sandbox cannot reach the host. It needs no default route, and the first address of each
// on-link subnet, where Docker puts a bridge network's gateway on the host, must answer no connection on hostPorts.
// A host firewall that drops those ports still answers ARP, which the dials set off, so an IPv4 address with a
// complete ARP entry has answered too. An isolated gateway leaves that address free for a container, so an answer from
// an address Docker's DNS names as a container on the network is a neighbour, which sandboxes may reach, not the host.
// An unreadable routing or ARP table fails.
func hostIsolation(read func(string) ([]byte, error), own []netip.Addr, dial dialFunc, container func(netip.Addr) string) (bool, string) {
	route, err := read("/proc/net/route")
	if err != nil {
		return false, "routing table unreadable: " + errnoText(err)
	}
	route6, err := read("/proc/net/ipv6_route")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, "IPv6 routing table unreadable: " + errnoText(err)
	}
	defaults, gateways, err := parseRoutes(route, route6)
	if err != nil {
		return false, "routing table unreadable: " + err.Error()
	}
	if len(defaults) > 0 {
		return false, "default route via " + strings.Join(defaults, ", ")
	}
	targets, notes := []netip.Addr{}, []string{}
	for _, gateway := range gateways {
		switch {
		case slices.Contains(targets, gateway):
		case slices.Contains(own, gateway.WithZone("")):
			notes = append(notes, "the sandbox itself holds "+gateway.String())
		default:
			targets = append(targets, gateway)
		}
	}
	addresses := []string{}
	for _, target := range targets {
		for _, port := range hostPorts {
			addresses = append(addresses, netip.AddrPortFrom(target, port).String())
		}
	}
	answers := reachable(addresses, 3*time.Second, dial)
	arpAnswered := map[netip.Addr]bool{}
	if slices.ContainsFunc(targets, netip.Addr.Is4) {
		arp, err := read("/proc/net/arp")
		if err != nil {
			return false, "ARP table unreadable: " + errnoText(err)
		}
		if arpAnswered, err = completeARP(arp); err != nil {
			return false, "ARP table unreadable: " + err.Error()
		}
	}
	reached := []string{}
	for _, target := range targets {
		ports := []string{}
		for _, address := range answers {
			if parsed, err := netip.ParseAddrPort(address); err == nil && parsed.Addr() == target {
				ports = append(ports, address)
			}
		}
		if len(ports) == 0 && !arpAnswered[target] {
			notes = append(notes, "no answer from "+target.String())
		} else if name := container(target); name != "" {
			notes = append(notes, target.String()+" is the container "+name)
		} else if len(ports) > 0 {
			reached = append(reached, ports...)
		} else {
			reached = append(reached, target.String()+" (answered ARP)")
		}
	}
	if len(reached) > 0 {
		return false, "reached " + strings.Join(reached, ", ")
	}
	if len(notes) == 0 {
		return true, "no default route and no on-link subnet"
	}
	return true, "no default route; " + strings.Join(notes, "; ")
}

// Route flags from the kernel's route.h and ipv6_route.h.
const (
	routeUp      = 0x1
	routeGateway = 0x2
	routeReject  = 0x200
	routeLocal   = 0x80000000
)

// parseRoutes reads /proc/net/route and /proc/net/ipv6_route. It returns every default route and the addresses that
// could lead out of the sandbox: each route's next hop, and the first address of each on-link subnet. Loopback, local,
// link-local and multicast routes lead nowhere else and are skipped.
func parseRoutes(route, route6 []byte) ([]string, []netip.Addr, error) {
	defaults, gateways := []string{}, []netip.Addr{}
	lines := strings.Split(strings.TrimSpace(string(route)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Iface") {
		return nil, nil, errors.New("unexpected /proc/net/route format")
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			return nil, nil, fmt.Errorf("unexpected route %q", line)
		}
		destination, err1 := routeAddr4(fields[1])
		gateway, err2 := routeAddr4(fields[2])
		flags, err3 := strconv.ParseUint(fields[3], 16, 32)
		mask, err4 := routeAddr4(fields[7])
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return nil, nil, fmt.Errorf("unexpected route %q", line)
		}
		if flags&routeUp == 0 || fields[0] == "lo" {
			continue
		}
		ones, bits := net.IPMask(mask.AsSlice()).Size()
		if bits == 0 {
			return nil, nil, fmt.Errorf("non-contiguous route mask %s", mask)
		}
		if ones == 0 {
			defaults = append(defaults, fields[0]+" "+gateway.String())
		}
		if flags&routeGateway != 0 || !gateway.IsUnspecified() {
			gateways = append(gateways, gateway)
			continue
		}
		if ones > 0 {
			gateways = append(gateways, firstAddress(netip.PrefixFrom(destination, ones)))
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(route6)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 10 {
			return nil, nil, fmt.Errorf("unexpected IPv6 route %q", line)
		}
		destination, err1 := routeAddr6(fields[0])
		length, err2 := strconv.ParseUint(fields[1], 16, 8)
		next, err3 := routeAddr6(fields[4])
		flags, err4 := strconv.ParseUint(fields[8], 16, 32)
		if err := errors.Join(err1, err2, err3, err4); err != nil || length > 128 {
			return nil, nil, fmt.Errorf("unexpected IPv6 route %q", line)
		}
		iface := fields[9]
		if flags&routeUp == 0 || flags&(routeReject|routeLocal) != 0 || iface == "lo" {
			continue
		}
		if length == 0 {
			defaults = append(defaults, iface+" "+next.String())
		}
		if !next.IsUnspecified() {
			if next.IsLinkLocalUnicast() {
				next = next.WithZone(iface)
			}
			gateways = append(gateways, next)
			continue
		}
		if length > 0 && !destination.IsLinkLocalUnicast() && !destination.IsMulticast() && !destination.IsLoopback() {
			gateways = append(gateways, firstAddress(netip.PrefixFrom(destination, int(length))))
		}
	}
	return defaults, gateways, nil
}

// arpComplete is ATF_COM from the kernel's if_arp.h: the neighbour answered and its hardware address is known.
const arpComplete = 0x2

// completeARP reads /proc/net/arp and returns the IPv4 addresses whose entries are complete. An address that never
// answered stays incomplete until the kernel drops it.
func completeARP(table []byte) (map[netip.Addr]bool, error) {
	lines := strings.Split(strings.TrimSpace(string(table)), "\n")
	if !strings.HasPrefix(lines[0], "IP address") {
		return nil, errors.New("unexpected /proc/net/arp format")
	}
	complete := map[netip.Addr]bool{}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, fmt.Errorf("unexpected ARP entry %q", line)
		}
		addr, err1 := netip.ParseAddr(fields[0])
		flags, err2 := strconv.ParseUint(fields[2], 0, 32)
		if err := errors.Join(err1, err2); err != nil {
			return nil, fmt.Errorf("unexpected ARP entry %q", line)
		}
		if flags&arpComplete != 0 {
			complete[addr] = true
		}
	}
	return complete, nil
}

// firstAddress is the first host address of a subnet, the one Docker's default IPAM gives the bridge gateway. A /31,
// /32, /127 or /128 has no network address to skip.
func firstAddress(prefix netip.Prefix) netip.Addr {
	network := prefix.Masked().Addr()
	if prefix.Addr().BitLen()-prefix.Bits() < 2 {
		return prefix.Addr()
	}
	return network.Next()
}

// routeAddr4 decodes an address as /proc/net/route prints it: the network-order bytes read as a native integer.
func routeAddr4(text string) (netip.Addr, error) {
	value, err := strconv.ParseUint(text, 16, 32)
	if err != nil {
		return netip.Addr{}, err
	}
	var raw [4]byte
	binary.NativeEndian.PutUint32(raw[:], uint32(value))
	return netip.AddrFrom4(raw), nil
}

func routeAddr6(text string) (netip.Addr, error) {
	raw, err := hex.DecodeString(text)
	if err != nil || len(raw) != 16 {
		return netip.Addr{}, errors.New("invalid IPv6 route address")
	}
	return netip.AddrFrom16([16]byte(raw)), nil
}

// ownAddresses are the sandbox's own interface addresses, which never lead to the host.
func ownAddresses() []netip.Addr {
	own := []netip.Addr{}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return own
	}
	for _, address := range addresses {
		if prefix, err := netip.ParsePrefix(address.String()); err == nil {
			own = append(own, prefix.Addr().Unmap())
		}
	}
	return own
}

// containerName asks Docker's embedded DNS which container holds an address on the sandbox's network. The host's
// bridge address has no such name; an unanswered lookup counts as the host.
func containerName(addr netip.Addr) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, addr.String())
	if err != nil || len(names) == 0 {
		return ""
	}
	name := strings.TrimSuffix(names[0], ".")
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return strings.TrimSpace(string(data))
}

func limited(value string) bool {
	return value != "max" && value != "unreadable" && value != ""
}

func detailList(prefix string, items []string, otherwise string) string {
	if len(items) == 0 {
		return otherwise
	}
	return prefix + " " + strings.Join(items, ", ")
}

// egressRefusalTargets are what the gateway must refuse a runner sandbox, each with the reason it must give: a public
// name that is on no allowlist, refused by the runner allowlist itself rather than for some other cause, and the cloud
// metadata service and a local port, refused because neither is a host name an allowlist could hold.
var egressRefusalTargets = []struct{ target, reason string }{
	{"example.com:443", "host is not on the " + KindRunner.String() + " allowlist"},
	{"169.254.169.254:80", "target is not an allowlisted host name"},
	{"localhost:4200", "target is not an allowlisted host name"},
}

// egressRefusals asks the gateway for each refusal target. Only a 403 with the expected reason counts: a 407 shows
// the gateway did not accept the probe's lease, so its allowlist was never consulted.
func egressRefusals(proxy string) (bool, string) {
	denied, failures := []string{}, []string{}
	for _, refusal := range egressRefusalTargets {
		code, reason, err := proxyConnect(proxy, refusal.target)
		switch {
		case err != nil:
			failures = append(failures, refusal.target+" ("+err.Error()+")")
		case code == http.StatusProxyAuthRequired:
			failures = append(failures, refusal.target+" (the gateway refused the probe's credential, HTTP 407)")
		case code == http.StatusForbidden && strings.Contains(reason, refusal.reason):
			denied = append(denied, refusal.target)
		case code == http.StatusForbidden:
			failures = append(failures, fmt.Sprintf("%s (HTTP 403 for another reason: %s)", refusal.target, reason))
		default:
			failures = append(failures, fmt.Sprintf("%s (HTTP %d)", refusal.target, code))
		}
	}
	return len(failures) == 0, detailList("not proven refused:", failures, "refused "+strings.Join(denied, ", "))
}

// proxyConnect asks the configured egress gateway for a tunnel and returns its status and the first line of its
// explanation.
func proxyConnect(proxy, target string) (int, string, error) {
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return 0, "", errors.New("unreadable proxy address")
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return 0, "", errors.New("gateway unreachable")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if u.User != nil {
		password, _ := u.User.Password()
		credential := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
		request += "Proxy-Authorization: Basic " + credential + "\r\n"
	}
	if _, err := io.WriteString(conn, request+"\r\n"); err != nil {
		return 0, "", err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	reason, _, _ := bytes.Cut(body, []byte("\n"))
	return resp.StatusCode, strings.ToValidUTF8(strings.TrimSpace(string(reason)), "�"), nil
}
