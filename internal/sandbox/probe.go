package sandbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/process"
)

// ProbeCheck is one containment property observed from inside a real sandbox.
type ProbeCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// ProbeReport is everything the containment probe observed.
type ProbeReport struct {
	Checks []ProbeCheck `json:"checks"`
	Kernel string       `json:"kernel"`
}

func (r ProbeReport) Passed() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, check := range r.Checks {
		if !check.Passed {
			return false
		}
	}
	return true
}

// probeTargets are paths a sandbox must never see: orchestrator state, deployment secrets and the Docker daemon.
var probeTargets = []string{
	"/var/lib/octomus/data/state.db", "/run/secrets", "/var/run/docker.sock", "/run/docker.sock", "/run/octomus",
}

// Probe runs the containment probe in a fresh probe sandbox and returns its report.
func Probe(ctx context.Context, backend Backend) (ProbeReport, error) {
	if backend.Mode() != ModeDocker {
		return ProbeReport{}, errors.New("The containment probe needs the Docker sandbox")
	}
	child, err := backend.Start(ctx, Spec{Kind: KindProbe, Probe: ProbeContainment, Timeout: 120})
	if err != nil {
		return ProbeReport{}, err
	}
	out, err := process.CaptureStarted(ctx, child, child.Stdout(), child.Stderr(), 90, process.CaptureMachine)
	if err != nil {
		return ProbeReport{}, err
	}
	if !out.Status.Success() {
		return ProbeReport{}, fmt.Errorf("Containment probe failed with %s: %s", out.Status, out.Stderr.SafePreview())
	}
	var report ProbeReport
	if err := json.Unmarshal(out.Stdout.Bytes, &report); err != nil {
		return ProbeReport{}, fmt.Errorf("Containment probe answered unreadable output: %w", err)
	}
	for i := range report.Checks {
		report.Checks[i].Detail = strings.ToValidUTF8(report.Checks[i].Detail, "�")
	}
	return report, nil
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
	writable := []string{}
	for _, dir := range []string{"/usr", "/etc", "/"} {
		probe := dir + "/.octomus-probe"
		if err := os.WriteFile(strings.ReplaceAll(probe, "//", "/"), []byte("x"), 0o600); err == nil {
			writable = append(writable, dir)
			_ = os.Remove(probe)
		}
	}
	add("read_only_image", "The image filesystem is read-only", len(writable) == 0, detailList("writable", writable, "read-only"))
	visible := []string{}
	for _, path := range probeTargets {
		if _, err := os.Lstat(path); err == nil {
			visible = append(visible, path)
		}
	}
	add("no_orchestrator_state", "Cannot see Octomus state, secrets or the Docker socket", len(visible) == 0,
		detailList("visible", visible, "none visible"))
	reached := []string{}
	for _, address := range []string{"1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443"} {
		conn, err := net.DialTimeout("tcp", address, 3*time.Second)
		if err == nil {
			conn.Close()
			reached = append(reached, address)
		}
	}
	add("no_direct_egress", "Has no direct route to the internet", len(reached) == 0, detailList("reached", reached, "no route"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	resolved, err := net.DefaultResolver.LookupHost(ctx, "example.com")
	cancel()
	add("no_external_dns", "Cannot resolve internet names directly", err != nil, detailList("resolved", resolved, "lookup refused"))
	route := defaultRoute()
	add("no_host_route", "Has no gateway to the host or its neighbours", route == "", detailList("default route via", nonEmpty(route), "no default route"))
	add("resource_limits", "Runs under memory and process limits", limited("/sys/fs/cgroup/memory.max") && limited("/sys/fs/cgroup/pids.max"),
		fmt.Sprintf("memory.max %s, pids.max %s", readTrim("/sys/fs/cgroup/memory.max"), readTrim("/sys/fs/cgroup/pids.max")))
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		add("egress_gateway", "Egress is limited to the allowlist", true, "no egress gateway: sandboxes are offline")
	} else {
		denied := []string{}
		failures := []string{}
		for _, target := range []string{"example.com:443", "169.254.169.254:80", "localhost:4200"} {
			code, err := proxyConnect(proxy, target)
			switch {
			case err != nil:
				failures = append(failures, target+" ("+err.Error()+")")
			case code == http.StatusForbidden:
				denied = append(denied, target)
			default:
				failures = append(failures, fmt.Sprintf("%s (HTTP %d)", target, code))
			}
		}
		add("egress_gateway", "The egress gateway refuses unlisted, metadata and local targets", len(failures) == 0,
			detailList("not refused", failures, "refused "+strings.Join(denied, ", ")))
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

// defaultRoute reports the IPv4 default route's gateway, if the sandbox has one.
func defaultRoute() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) > 2 && fields[1] == "00000000" {
			return fields[0] + " " + fields[2]
		}
	}
	return ""
}

func readTrim(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return strings.TrimSpace(string(data))
}

func limited(path string) bool {
	value := readTrim(path)
	return value != "max" && value != "unreadable" && value != ""
}

func nonEmpty(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func detailList(prefix string, items []string, otherwise string) string {
	if len(items) == 0 {
		return otherwise
	}
	return prefix + " " + strings.Join(items, ", ")
}

// proxyConnect asks the configured egress gateway for a tunnel and returns its status.
func proxyConnect(proxy, target string) (int, error) {
	u, err := url.Parse(proxy)
	if err != nil || u.Host == "" {
		return 0, errors.New("unreadable proxy address")
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		return 0, errors.New("gateway unreachable")
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
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}
