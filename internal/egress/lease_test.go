package egress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRevokeConfirmsAbsenceAndReportsFailureWithoutCredential(t *testing.T) {
	leases := Leases{Dir: t.TempDir()}
	token, err := leases.Grant("fixture", "runner")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(leases.Dir, leaseFile(token))
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	// A nonempty directory cannot be unlinked even when tests run as root.
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, "obstruction"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := leases.Revoke(token); err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("failed revocation must be explicit and credential-safe: %v", err)
	}
	if err := os.RemoveAll(file); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := leases.Revoke(token); err != nil {
			t.Fatalf("absent lease must be successfully revoked: %v", err)
		}
	}
}

func TestRevokeLoginPreservesOtherLeaseOwners(t *testing.T) {
	leases := Leases{Dir: t.TempDir()}
	var logins, others []string
	for _, name := range []string{"login", "task-one", "login", "task-two"} {
		token, err := leases.Grant(name, "runner")
		if err != nil {
			t.Fatal(err)
		}
		if name == "login" {
			logins = append(logins, token)
		} else {
			others = append(others, token)
		}
	}
	for range 2 {
		if err := leases.RevokeLogin(); err != nil {
			t.Fatal(err)
		}
	}
	for _, token := range logins {
		if _, _, ok := leases.lookup(token); ok {
			t.Fatal("old login remained authorized")
		}
	}
	for _, token := range others {
		if _, _, ok := leases.lookup(token); !ok {
			t.Fatal("unrelated sandbox lost authorization")
		}
	}
}
