package sandbox

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// Docker Engine still runs on cgroup v1 hosts, and gVisor offers only cgroup v1; there the runtime mounts the
// container's own cgroup under each controller's directory.
func TestCgroupLimitsFallBackToCgroupV1(t *testing.T) {
	files := func(contents map[string]string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if text, ok := contents[path]; ok {
				return []byte(text), nil
			}
			return nil, os.ErrNotExist
		}
	}
	unified := cgroupLimits(files(map[string]string{"/sys/fs/cgroup/memory.max": "268435456\n", "/sys/fs/cgroup/pids.max": "256\n",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes": "4096\n"}))
	if unified != (ProbeLimits{Memory: "268435456", Pids: "256"}) {
		t.Fatalf("cgroup v2 limits = %+v", unified)
	}
	legacy := cgroupLimits(files(map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "268435456\n",
		"/sys/fs/cgroup/pids/pids.max": "256\n"}))
	if legacy != (ProbeLimits{Memory: "268435456", Pids: "256"}) {
		t.Fatalf("cgroup v1 limits = %+v", legacy)
	}
	if none := cgroupLimits(files(nil)); none != (ProbeLimits{Memory: "unreadable", Pids: "unreadable"}) {
		t.Fatalf("no cgroup files = %+v", none)
	}
	// cgroup v1 shows no memory limit as the largest page-aligned number, which no configured limit matches.
	r := ProbeReport{Checks: []ProbeCheck{{ID: "resource_limits", Passed: true}},
		Limits: ProbeLimits{Memory: "9223372036854771712", Pids: "256"}}
	r.confirmLimits(&wire.BrokerLimits{Memory: 256 << 20, Pids: 256}, nil)
	if r.Passed() || !strings.Contains(r.Checks[0].Detail, "configured 268435456") {
		t.Fatalf("unlimited cgroup v1 memory = %+v", r.Checks[0])
	}
}

// pids.max is a number on every systemd host whether or not the broker set a limit, and the probe's own cgroup holds
// exactly what Docker set, so only a match with the broker's configured limits confirms the check.
func TestConfirmLimitsHoldsTheProbeToTheBrokersLimits(t *testing.T) {
	report := func(memory, pids string) ProbeReport {
		return ProbeReport{
			Checks: []ProbeCheck{{ID: "non_root", Passed: true}, {ID: "resource_limits", Passed: limited(memory) && limited(pids),
				Detail: "memory.max " + memory + ", pids.max " + pids}},
			Limits: ProbeLimits{Memory: memory, Pids: pids},
		}
	}
	want := &wire.BrokerLimits{Memory: 256 << 20, Pids: 256}
	page := int64(os.Getpagesize())
	unaligned := &wire.BrokerLimits{Memory: 256<<20 + page/2, Pids: 256}
	large := &wire.BrokerLimits{Memory: 256 << 20, Pids: 32768}
	cases := []struct {
		name         string
		memory, pids string
		want         *wire.BrokerLimits
		passed       bool
		detail       string
	}{
		{"broker limits in force", "268435456", "256", want, true, "memory.max 268435456, pids.max 256"},
		{"memory rounded down to whole pages", "268435456", "256", unaligned, true, "memory.max 268435456"},
		{"systemd's pids limit only", "268435456", "25519", want, false, "pids.max 25519, configured 256"},
		{"systemd's pids limit under a higher configured one", "268435456", "25519", large, false, "pids.max 25519, configured 32768"},
		{"a lower memory limit than the broker's", strconv.FormatInt(256<<20-page, 10), "256", want, false,
			"memory.max " + strconv.FormatInt(256<<20-page, 10) + ", configured 268435456"},
		{"no memory limit", "max", "256", want, false, "memory.max max, not a numeric limit"},
		{"unreadable pids limit", "268435456", "unreadable", want, false, "pids.max unreadable, not a numeric limit"},
		{"no configured pids limit", "268435456", "256", &wire.BrokerLimits{Memory: 256 << 20}, false,
			"pids.max 256, but the broker reports no configured limit"},
		{"unknown broker limits", "268435456", "256", nil, false, "the broker's configured limits are unknown: broker gone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := report(c.memory, c.pids)
			r.confirmLimits(c.want, errors.New("broker gone"))
			check := r.Checks[1]
			if check.Passed != c.passed || !strings.Contains(check.Detail, c.detail) || r.Passed() != c.passed {
				t.Fatalf("resource_limits = %v, %q; want %v with %q", check.Passed, check.Detail, c.passed, c.detail)
			}
		})
	}
}
