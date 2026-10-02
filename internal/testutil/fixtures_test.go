package testutil_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestFixtureScriptsStayAnchoredAfterChdir(t *testing.T) {
	// A different checkout-shaped working directory must not redirect fixtures.
	decoy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(decoy, "tests", "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "go.mod"), []byte("module github.com/tyk-swe/octomus-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "tests", "fixtures", "git.sh"), []byte("wrong fixture\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(decoy)
	// This test's first fixture lookup happens only after the directory changes.
	path := testutil.FixturePath("git.sh")
	if !filepath.IsAbs(path) || strings.HasPrefix(path, decoy+string(os.PathSeparator)) {
		t.Fatalf("fixture lookup followed the changed working directory: %q", path)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "git")
	if err := testutil.InstallFixtureScript(dest, "git.sh"); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(installed, original) {
		t.Fatalf("installed fixture differs from the original: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed fixture lost its executable mode: %v, %v", info, err)
	}
}
