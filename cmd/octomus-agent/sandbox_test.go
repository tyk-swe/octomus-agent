package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/egress"
)

func loginLeaseEnvironment(dir, file string) func(string) (string, bool) {
	values := map[string]string{"OCTOMUS_EGRESS_LEASES": dir, "OCTOMUS_EGRESS_PROXY": "egress:3128", "OCTOMUS_LOGIN_PROXY_FILE": file}
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func TestLoginRotationRequiresConfirmedRevocation(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "blocked"}[blocked], func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(t.TempDir(), "proxy")
			leases := egress.Leases{Dir: dir}
			token, err := leases.Grant("login", "runner")
			if err != nil {
				t.Fatal(err)
			}
			previous := egress.ProxyURL("egress:3128", token) + "\n"
			if err := os.WriteFile(file, []byte(previous), 0o600); err != nil {
				t.Fatal(err)
			}
			old := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(token))))
			if blocked {
				if err := os.Remove(old); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(old, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(old, "obstruction"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = loginLease(loginLeaseEnvironment(dir, file))
			current, readErr := os.ReadFile(file)
			entries, listErr := os.ReadDir(dir)
			if readErr != nil || listErr != nil || len(entries) != 1 {
				t.Fatalf("rotation changed lease count unexpectedly: %v, %v, %v", entries, readErr, listErr)
			}
			if blocked {
				if err == nil || strings.Contains(err.Error(), token) || string(current) != previous {
					t.Fatalf("failed rotation must retain previous identity and grant nothing: %v", err)
				}
			} else if err != nil || string(current) == previous {
				t.Fatalf("confirmed rotation did not replace the previous credential: %v", err)
			} else if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatal("previous credential retained its lease")
			}
		})
	}
}

func TestLoginLeaseDoesNotLeaveAuthorizationAfterProxyWriteFailure(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "missing", "proxy")
	if err := loginLease(loginLeaseEnvironment(dir, file)); err == nil {
		t.Fatal("expected failed proxy file write")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("failed proxy write retained a lease: %v, %v", entries, err)
	}
}

func TestLoginLeaseMetadataRecoversMissingOrMalformedProxyIdentity(t *testing.T) {
	for _, previous := range []string{"directory", "http://sandbox:fixture-secret%invalid@egress:3128", "http://egress:3128"} {
		t.Run(previous, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(t.TempDir(), "proxy")
			if previous == "directory" {
				if err := os.Mkdir(file, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(file, []byte(previous), 0o600); err != nil {
				t.Fatal(err)
			}
			leases := egress.Leases{Dir: dir}
			orphan, err := leases.Grant("login", "runner")
			if err != nil {
				t.Fatal(err)
			}
			err = loginLease(loginLeaseEnvironment(dir, file))
			if strings.Contains(fmt.Sprint(err), "fixture-secret") || (previous == "directory") != (err != nil) {
				t.Fatalf("proxy file must not own old authorization: %v", err)
			}
			old := filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(orphan))))
			if _, err := os.Stat(old); !os.IsNotExist(err) {
				t.Fatal("interrupted login grant retained its authorization")
			}
			count := 1
			if previous == "directory" {
				count = 0
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != count {
				t.Fatalf("proxy write outcome retained the wrong authorizations: %v, %v", entries, err)
			}
		})
	}
}

func TestLoginRotationRecoversInterruptedGrantAndPreservesSandbox(t *testing.T) {
	dir, file := t.TempDir(), filepath.Join(t.TempDir(), "proxy")
	leases := egress.Leases{Dir: dir}
	old, err := leases.Grant("login", "runner")
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := leases.Grant("task-owned", "runner")
	if err != nil {
		t.Fatal(err)
	}
	if err := loginLease(loginLeaseEnvironment(dir, file)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(old))))); !os.IsNotExist(err) {
		t.Fatal("orphan login authorization survived rotation")
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%x.json", sha256.Sum256([]byte(sandbox))))); err != nil {
		t.Fatal("unrelated live sandbox lost its lease")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("expected one sandbox and one current login lease: %v, %v", entries, err)
	}
}

func TestLoginRotationRefusesUnclassifiableMetadata(t *testing.T) {
	for name, data := range map[string]string{
		"missing kind":        `{"sandbox":"login"}`,
		"null":                `null`,
		"duplicate ownership": `{"sandbox":"task","kind":"runner","sandbox":"login"}`,
		"oversized":           strings.Repeat(" ", 8192) + `{"sandbox":"login","kind":"runner"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, file := t.TempDir(), filepath.Join(t.TempDir(), "proxy")
			bad := filepath.Join(dir, "broken.json")
			if err := os.WriteFile(bad, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := loginLease(loginLeaseEnvironment(dir, file)); err == nil {
				t.Fatal("unclassifiable metadata granted replacement")
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
				t.Fatalf("unknown pending owner was silently removed: %v, %v", entries, err)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("refused replacement wrote proxy identity")
			}
		})
	}
}
