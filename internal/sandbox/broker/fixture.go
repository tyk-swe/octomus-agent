package broker

import "github.com/tyk-swe/octomus-agent/internal/sandbox/engineapi"

// fixtureMounts lets integration-test builds (tag sandboxfixture) share the fixture directory with runner and
// verification sandboxes. Production builds leave it nil, so no request can add a mount.
var fixtureMounts func(plan) ([]engineapi.Mount, []string)
