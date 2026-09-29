package model

import (
	"sort"

	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// SandboxRecord is what the sandboxes behind one session or command did: the image they ran, how many there were,
// whether a memory limit stopped one, and which hosts the egress gateway let them reach or refused.
type SandboxRecord struct {
	ImageID string        `json:"image_id"`
	Runtime string        `json:"runtime"`
	Runs    uint64        `json:"runs"`
	OOM     bool          `json:"oom"`
	Egress  SandboxEgress `json:"egress"`
}

func (v *SandboxRecord) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v SandboxRecord) MarshalJSON() ([]byte, error) {
	type plain SandboxRecord
	return wirejson.Record(plain(v))
}

// SandboxEgress counts tunnels per host:port. Host names come from untrusted code, so they stay in private task
// records and the dashboard and are never exported as run evidence.
type SandboxEgress struct {
	Allowed map[string]uint64 `json:"allowed"`
	Denied  map[string]uint64 `json:"denied"`
}

func (v *SandboxEgress) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v SandboxEgress) MarshalJSON() ([]byte, error) {
	type plain SandboxEgress
	return wirejson.Record(plain(v))
}

// SandboxHostLimit bounds the hosts one record keeps per decision; the rest are counted under "other".
const SandboxHostLimit = 64

// MergeSandbox adds one run's record to what a session or command already recorded.
func MergeSandbox(into *SandboxRecord, run *SandboxRecord) *SandboxRecord {
	if run == nil {
		return into
	}
	merged := SandboxRecord{Egress: SandboxEgress{Allowed: map[string]uint64{}, Denied: map[string]uint64{}}}
	if into != nil {
		merged = wirejson.Clone(*into)
		if merged.Egress.Allowed == nil {
			merged.Egress.Allowed = map[string]uint64{}
		}
		if merged.Egress.Denied == nil {
			merged.Egress.Denied = map[string]uint64{}
		}
	}
	if into != nil && (into.ImageID == "mixed" || into.ImageID != run.ImageID || into.Runtime != run.Runtime) {
		// Once an aggregate spans images or runtimes, later runs cannot restore a single identity.
		merged.ImageID, merged.Runtime = "mixed", ""
	} else {
		merged.ImageID, merged.Runtime = run.ImageID, run.Runtime
	}
	merged.Runs += run.Runs
	merged.OOM = merged.OOM || run.OOM
	addHosts(merged.Egress.Allowed, run.Egress.Allowed)
	addHosts(merged.Egress.Denied, run.Egress.Denied)
	return &merged
}

func addHosts(into, from map[string]uint64) {
	hosts := make([]string, 0, len(from))
	for host := range from {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		key := host
		if _, known := into[key]; !known && len(into) >= SandboxHostLimit {
			key = "other"
		}
		into[key] += from[host]
	}
}
