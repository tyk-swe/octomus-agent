package testutil

import (
	"os"
	"path/filepath"
	"runtime"
)

// FixturePath resolves a file in tests/fixtures relative to this source file,
// independent of the calling package's working directory.
func FixturePath(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "tests", "fixtures", name)
}

// InstallFixtureScript copies a tests/fixtures file to dst as an executable.
func InstallFixtureScript(dst, name string) error {
	data, err := os.ReadFile(FixturePath(name))
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
