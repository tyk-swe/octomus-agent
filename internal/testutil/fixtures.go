package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var fixtureDirectory = locateFixtureDirectory()

func locateFixtureDirectory() string {
	_, file, _, _ := runtime.Caller(0)
	var cwd string
	if !filepath.IsAbs(file) {
		cwd, _ = os.Getwd()
	}
	return fixtureDirectoryFromSource(file, cwd)
}

func fixtureDirectoryFromSource(file, initialDirectory string) string {
	directory := filepath.Join(filepath.Dir(file), "..", "..", "tests", "fixtures")
	if filepath.IsAbs(directory) {
		return directory
	}
	// -trimpath records a module path instead of a filesystem path. Resolve
	// the source checkout before any test can change the process directory.
	if initialDirectory != "" {
		if found := findFixtureDirectory(initialDirectory); found != "" {
			return found
		}
	}
	return directory
}

func findFixtureDirectory(directory string) string {
	const module = "github.com/tyk-swe/octomus-agent"
	for {
		data, err := os.ReadFile(filepath.Join(directory, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 || fields[0] != "module" || (fields[1] != module && fields[1] != `"`+module+`"`) {
					continue
				}
				fixtures := filepath.Join(directory, "tests", "fixtures")
				if info, err := os.Stat(fixtures); err == nil && info.IsDir() {
					return fixtures
				}
				return ""
			}
			return "" // An unrelated module is not this checkout.
		} else if !os.IsNotExist(err) {
			return ""
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}

// FixturePath resolves a file in tests/fixtures independently of later working
// directory changes. A -trimpath test binary must start inside the source checkout.
func FixturePath(name string) string {
	return filepath.Join(fixtureDirectory, name)
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
