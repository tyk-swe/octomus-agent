package wire

// BrokerInfo is what a sandbox broker reports about the isolation it enforces.
type BrokerInfo struct {
	Version       string            `json:"version"`
	DockerVersion string            `json:"docker_version"`
	APIVersion    string            `json:"api_version"`
	Image         string            `json:"image"`
	ImageID       string            `json:"image_id"`
	ImageDigests  []string          `json:"image_digests"`
	Runtime       string            `json:"runtime"`
	Runners       map[string]string `json:"runners"`
	// RunnerErrors says why each runner that is installed in the image did not report its version, so it is not
	// mistaken for a missing one.
	RunnerErrors map[string]string `json:"runner_errors"`
	Limits       BrokerLimits      `json:"limits"`
	Networks     BrokerNetworks    `json:"networks"`
	Egress       bool              `json:"egress"`
	Live         int               `json:"live"`
}

type BrokerLimits struct {
	NanoCPUs   int64  `json:"nano_cpus"`
	Memory     int64  `json:"memory_bytes"`
	Pids       int64  `json:"pids"`
	Tmpfs      int64  `json:"tmpfs_bytes"`
	Max        int    `json:"max_sandboxes"`
	MaxSeconds uint64 `json:"max_seconds"`
}

type BrokerNetworks struct {
	Runner string `json:"runner"`
	Verify string `json:"verify"`
}
