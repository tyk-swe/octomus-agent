package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

// ProbeCheck is one containment property observed from inside a real sandbox.
type ProbeCheck struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// ProbeReport is everything the containment probe observed.
type ProbeReport struct {
	Checks []ProbeCheck `json:"checks"`
	Kernel string       `json:"kernel"`
	// Limits are the raw cgroup limits the probe read. Only the broker knows what it configured, so Probe compares
	// them outside the sandbox.
	Limits ProbeLimits `json:"limits"`
}

// ProbeLimits are the cgroup limits in force inside the probe sandbox, as the kernel reports them: cgroup v2's
// memory.max and pids.max, or cgroup v1's memory.limit_in_bytes and pids.max where v2's are absent.
type ProbeLimits struct {
	Memory string `json:"memory_max"`
	Pids   string `json:"pids_max"`
}

func (r ProbeReport) Passed() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, check := range r.Checks {
		if !check.Passed {
			return false
		}
	}
	return true
}

// Probe runs the containment probe in a fresh probe sandbox and returns its report.
func Probe(ctx context.Context, backend Backend) (ProbeReport, error) {
	if backend.Mode() != ModeDocker {
		return ProbeReport{}, errors.New("The containment probe needs the Docker sandbox")
	}
	child, err := backend.Start(ctx, Spec{Kind: KindProbe, Probe: wire.ProbeContainment, Timeout: 120})
	if err != nil {
		return ProbeReport{}, err
	}
	out, err := process.CaptureStarted(ctx, child, child.Stdout(), child.Stderr(), 90, process.CaptureMachine)
	if err != nil {
		return ProbeReport{}, err
	}
	if !out.Status.Success() {
		return ProbeReport{}, fmt.Errorf("Containment probe failed with %s: %s", out.Status, out.Stderr.SafePreview())
	}
	var report ProbeReport
	if err := json.Unmarshal(out.Stdout.Bytes, &report); err != nil {
		return ProbeReport{}, fmt.Errorf("Containment probe answered unreadable output: %w", err)
	}
	var limits *wire.BrokerLimits
	var limitsErr error = errors.New("the sandbox backend reports no limits")
	if informed, ok := backend.(interface {
		Info(context.Context) (wire.BrokerInfo, error)
	}); ok {
		var info wire.BrokerInfo
		if info, limitsErr = informed.Info(ctx); limitsErr == nil {
			limits = &info.Limits
		}
	}
	report.confirmLimits(limits, limitsErr)
	for i := range report.Checks {
		report.Checks[i].Detail = strings.ToValidUTF8(report.Checks[i].Detail, "�")
	}
	return report, nil
}

// confirmLimits holds the resource_limits check to the limits the broker configured. The probe reads its own cgroup
// (its cgroup namespace makes that the root of /sys/fs/cgroup; on cgroup v1 the runtime mounts it under each
// controller's directory), which holds exactly what Docker set from the broker's spec; no ancestor's limit shows
// there. So a number alone proves nothing (systemd gives every scope a pids limit of its own), and a lower one is
// some other limit than the broker's: the check passes only when both match. cgroup v1 shows no memory limit as the
// largest page-aligned number, which matches no configured limit.
func (r *ProbeReport) confirmLimits(want *wire.BrokerLimits, unknown error) {
	for i := range r.Checks {
		check := &r.Checks[i]
		if check.ID != "resource_limits" {
			continue
		}
		if want == nil {
			check.Passed = false
			check.Detail += "; the broker's configured limits are unknown: " + unknown.Error()
			return
		}
		problems := []string{}
		// The kernel keeps memory.max in whole pages, rounding the configured bytes down. The probe shares the control
		// plane's kernel, so this page size is the sandbox's.
		if problem := limitMismatch("memory.max", r.Limits.Memory, want.Memory, int64(os.Getpagesize())); problem != "" {
			problems = append(problems, problem)
		}
		if problem := limitMismatch("pids.max", r.Limits.Pids, want.Pids, 1); problem != "" {
			problems = append(problems, problem)
		}
		if len(problems) > 0 {
			check.Passed, check.Detail = false, strings.Join(problems, ", ")
		}
		return
	}
}

// limitMismatch says how an observed cgroup limit differs from the configured one, or returns "" when it is the
// configured value rounded down to a whole granule.
func limitMismatch(file, observed string, limit, granule int64) string {
	value, err := strconv.ParseInt(observed, 10, 64)
	switch {
	case err != nil:
		return fmt.Sprintf("%s %s, not a numeric limit", file, observed)
	case limit <= 0:
		return fmt.Sprintf("%s %d, but the broker reports no configured limit", file, value)
	case value <= 0 || value > limit || value <= limit-granule:
		return fmt.Sprintf("%s %d, configured %d", file, value, limit)
	}
	return ""
}
