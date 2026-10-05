package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// GoldenState copies the checked-in state-v<version>.db golden database to dst, so
// a test can open or upgrade a copy while the testdata file is never opened in place.
func GoldenState(t testing.TB, version, dst string) string {
	t.Helper()
	src := filepath.Join(RepoRoot(), "internal", "store", "testdata", "state-v"+version+".db")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}
