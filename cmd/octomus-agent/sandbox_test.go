package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// connectAs asks the gateway at address for a tunnel to an unlisted host with proxy's credential and returns the
// status and body: 407 for a credential without a live lease, 403 naming the lease's allowlist otherwise.
func connectAs(t *testing.T, address, proxy string) (int, string) {
	t.Helper()
	parsed, err := url.Parse(proxy)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	credential := base64.StdEncoding.EncodeToString([]byte(parsed.User.String()))
	if _, err := io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Basic "+credential+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestLoginLeaseGrantsARunnerLeaseAndRevokesThePreviousLogin(t *testing.T) {
	leases, login := t.TempDir(), t.TempDir()
	proxyFile := filepath.Join(login, "proxy")
	values := map[string]string{
		"OCTOMUS_EGRESS_LEASES":    leases,
		"OCTOMUS_EGRESS_PROXY":     "egress:3128",
		"OCTOMUS_LOGIN_PROXY_FILE": proxyFile,
	}
	env := func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
	log := &testutil.SyncBuffer{}
	gateway := egress.New(egress.Policy{}, leases, log)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: gateway}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Close() })

	issue := func() string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--login-lease"}, env, &stdout, &stderr); code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("--login-lease: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		data, err := os.ReadFile(proxyFile)
		if err != nil {
			t.Fatal(err)
		}
		proxy, found := strings.CutSuffix(string(data), "\n")
		if !found || !strings.HasPrefix(proxy, "http://"+egress.ProxyUser+":") || !strings.HasSuffix(proxy, "@egress:3128") {
			t.Fatalf("proxy file = %q", data)
		}
		for _, path := range append(leaseFiles(t, leases), proxyFile) {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("%s = %v, %v; want it private", path, info, err)
			}
		}
		return proxy
	}
	first := issue()
	if status, body := connectAs(t, listener.Addr().String(), first); status != http.StatusForbidden || !strings.Contains(body, "runner allowlist") {
		t.Fatalf("login credential = %d %q; want the runner allowlist's refusal", status, body)
	}
	if !strings.Contains(log.String(), `"sandbox":"login","kind":"runner"`) {
		t.Fatalf("gateway log = %s; want the login named", log.String())
	}
	second := issue()
	if second == first {
		t.Fatal("a new login reused the previous credential")
	}
	if status, _ := connectAs(t, listener.Addr().String(), first); status != http.StatusProxyAuthRequired {
		t.Fatalf("previous login's credential = %d; want it revoked", status)
	}
	if status, _ := connectAs(t, listener.Addr().String(), second); status != http.StatusForbidden {
		t.Fatalf("new login's credential = %d; want it live", status)
	}
	if files := leaseFiles(t, leases); len(files) != 1 {
		t.Fatalf("leases = %v; want only the latest login's", files)
	}

	delete(values, "OCTOMUS_LOGIN_PROXY_FILE")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--login-lease"}, env, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "OCTOMUS_LOGIN_PROXY_FILE is required") {
		t.Fatalf("without a proxy file: code=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"--login-lease", "extra"}, env, &stdout, &stderr); code != 2 || stderr.Len() == 0 {
		t.Fatalf("with an argument: code=%d stderr=%q", code, stderr.String())
	}
}

func leaseFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}
