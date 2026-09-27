package octomus

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var version string

var Version = strings.TrimSpace(version)
