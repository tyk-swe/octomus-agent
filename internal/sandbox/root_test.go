package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// A runner turn owns its home and can chmod any directory in it, including the mount points of the runner volume.
// The next turn of the same task must still start.
func TestPrepareRootRestoresDirectoriesASandboxLocked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := ownedWorkspace(t)
	root := filepath.Dir(dir)
	home := filepath.Join(root, RunnerHome)
	t.Cleanup(func() {
		for _, path := range []string{home, filepath.Join(home, ".cache"), filepath.Join(home, ".config"), filepath.Join(root, VerifyHome)} {
			_ = os.Chmod(path, 0o700)
		}
	})
	prepare := func() {
		t.Helper()
		if err := PrepareRoot(Spec{Kind: KindRunner, Dir: dir}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{".", ".cache", ".local", ".local/share", ".config", ".codex", ".cache/opencode"} {
			info, err := os.Lstat(filepath.Join(home, path))
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("%s = %v, %v; want a 0700 directory", path, info, err)
			}
		}
	}
	prepare()
	locks := []struct {
		path   string
		mode   os.FileMode
		remove string
	}{
		{".cache", 0, ""},
		{".", 0, ""},
		{".cache", 0o500, ".cache/opencode"},
		{".config", 0o777, ""},
	}
	for _, lock := range locks {
		if lock.remove != "" {
			if err := os.Remove(filepath.Join(home, lock.remove)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chmod(filepath.Join(home, lock.path), lock.mode); err != nil {
			t.Fatal(err)
		}
		prepare()
	}
	verifyHome := filepath.Join(root, VerifyHome)
	if err := PrepareRoot(Spec{Kind: KindVerify, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(verifyHome, 0); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRoot(Spec{Kind: KindVerify, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(verifyHome); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("verification home = %v, %v; want mode 0700", info, err)
	}
}
