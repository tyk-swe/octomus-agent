package egress

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLeasesGrantWhatTheGatewayLooksUpUntilRevoked(t *testing.T) {
	leases := Leases{Dir: t.TempDir()}
	token, err := leases.Grant("octomus-test-runner", "runner")
	if err != nil {
		t.Fatal(err)
	}
	lease, file, ok := leases.lookup(token)
	if !ok || lease != (Lease{Sandbox: "octomus-test-runner", Kind: "runner"}) || file != leaseFile(token) {
		t.Fatalf("lookup = %+v, %q, %v", lease, file, ok)
	}
	info, err := os.Stat(filepath.Join(leases.Dir, file))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("lease file = %v, %v; want it private", info, err)
	}
	if other, _ := leases.Grant("octomus-test-verify", "verify"); other == token {
		t.Fatal("two grants share a credential")
	}
	leases.Revoke(token)
	if _, _, ok := leases.lookup(token); ok {
		t.Fatal("a revoked lease is still live")
	}
	if want := "http://sandbox:" + token + "@egress:3128"; ProxyURL("egress:3128", token) != want {
		t.Fatalf("proxy URL = %q; want %q", ProxyURL("egress:3128", token), want)
	}
}
