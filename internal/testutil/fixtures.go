package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var fixtureDirectory = fixtureDir()

// fixtureDir resolves tests/fixtures from this file's recorded source path. A
// -trimpath build records a module path instead of a filesystem path, so it
// then walks up from the initial working directory to this checkout's go.mod
// before any test can change the process directory. When neither resolves,
// the relative path is kept so InstallFixtureScript can explain the failure.
func fixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	directory := filepath.Join(filepath.Dir(file), "..", "..", "tests", "fixtures")
	if filepath.IsAbs(directory) {
		return directory
	}
	const module = "github.com/tyk-swe/octomus-agent"
	current, _ := os.Getwd()
	for current != "" {
		data, err := os.ReadFile(filepath.Join(current, "go.mod"))
		if err != nil {
			parent := filepath.Dir(current)
			if !os.IsNotExist(err) || parent == current {
				return directory
			}
			current = parent
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[0] != "module" || (fields[1] != module && fields[1] != `"`+module+`"`) {
				continue
			}
			fixtures := filepath.Join(current, "tests", "fixtures")
			if info, err := os.Stat(fixtures); err == nil && info.IsDir() {
				return fixtures
			}
			break
		}
		return directory // An unrelated module is not this checkout.
	}
	return directory
}

// FixturePath resolves a file in tests/fixtures independently of later working
// directory changes. A -trimpath test binary must start inside the source checkout.
func FixturePath(name string) string {
	return filepath.Join(fixtureDirectory, name)
}

// RepoRoot is the source checkout tests/fixtures belongs to, for tests that build or read other parts of it.
func RepoRoot() string {
	return filepath.Dir(filepath.Dir(fixtureDirectory))
}

// InstallFixtureScript copies a tests/fixtures file to dst as an executable.
func InstallFixtureScript(dst, name string) error {
	path := FixturePath(name)
	if !filepath.IsAbs(path) {
		return fmt.Errorf("cannot locate fixture %q: start the -trimpath test binary inside the Octomus source checkout", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
