package testutil

import (
	"os"
	"strings"
)

// IsolateGitEnvironment removes inherited Git settings from a fixture test
// process. Call it at the start of TestMain, before any test deliberately sets
// its own environment. Repository-local configuration remains available.
//
// Personal signing settings and hooks can break fixture commits; repository
// location variables can redirect fixture commands outside their temporary
// directory. Test processes use only synthetic repositories and local remotes.
func IsolateGitEnvironment() error {
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			if err := os.Unsetenv(key); err != nil {
				return err
			}
		}
	}
	if err := os.Setenv("GIT_CONFIG_NOSYSTEM", "1"); err != nil {
		return err
	}
	return os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
}
