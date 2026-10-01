//go:build sandboxfixture

package broker

import (
	"os"
	"path/filepath"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// Integration tests run deterministic runner fixtures inside real sandboxes. They read and write a fixture directory
// the test owns, mounted at the same absolute path it has on the host.
func init() {
	fixtureMounts = func(p plan) ([]engineapi.Mount, []string) {
		volume, path := os.Getenv("OCTOMUS_SANDBOX_FIXTURE_VOLUME"), os.Getenv("OCTOMUS_SANDBOX_FIXTURE_PATH")
		if volume == "" || !filepath.IsAbs(path) || p.kind == wire.KindProbe && p.probe != wire.ProbeVersions {
			return nil, nil
		}
		return []engineapi.Mount{{Type: "volume", Source: volume, Target: path, VolumeOptions: &engineapi.VolumeOptions{NoCopy: true}}},
			[]string{"OCTOMUS_FIXTURE=" + path}
	}
}
