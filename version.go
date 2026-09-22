// Package octomus is the module-root package holding the single
// application-version source. The VERSION file sits at the repository root so
// Make, shell and Python release tooling read it verbatim; go:embed compiles
// the same bytes into the binary, keeping --version, /healthz, runner
// clientInfo, release tag validation and archive names consistent.
package octomus

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var version string

// Version is the application version recorded in the root VERSION file.
var Version = strings.TrimSpace(version)
