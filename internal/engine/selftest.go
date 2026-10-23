package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// SandboxPosture is what the dashboard shows about isolation: the mode, the broker's live report, the egress
// allowlists, the pinned repository and the latest self-test.
type SandboxPosture struct {
	Mode             string              `json:"mode"`
	Healthy          bool                `json:"healthy"`
	Error            *string             `json:"error"`
	Broker           *wire.BrokerInfo    `json:"broker"`
	Egress           map[string][]string `json:"egress"`
	PinnedRepository *string             `json:"pinned_repository"`
	SelfTest         *SandboxSelfTest    `json:"self_test"`
}

func (a *App) SandboxPosture() SandboxPosture {
	posture := SandboxPosture{Mode: a.sandbox.Mode().String(), Healthy: a.sandbox.Mode() == sandbox.ModeDocker, Egress: a.deployment.Egress}
	if a.deployment.pinned() {
		posture.PinnedRepository = new(a.deployment.GitHubRepo)
	}
	if remote, ok := a.sandbox.(brokerReporter); ok {
		ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
		info, err := remote.Info(ctx)
		cancel()
		if err != nil {
			posture.Healthy, posture.Error = false, new(redact.Error(err))
		} else {
			posture.Broker = &info
			if _, err := postureFingerprint(info, a.deployment); err != nil {
				posture.Healthy, posture.Error = false, new(redact.Error(err))
			}
		}
	}
	if selfTest, err := store.Get(a.store, a.selfTestKey()); err == nil && selfTest != nil {
		if posture.Broker != nil && selfTest.matchesPosture(*posture.Broker, a.deployment) {
			posture.SelfTest = selfTest
		}
	}
	return posture
}

// SandboxSelfTest is the latest containment probe, kept for the dashboard.
type SandboxSelfTest struct {
	At                 string               `json:"at"`
	Passed             bool                 `json:"passed"`
	Checks             []sandbox.ProbeCheck `json:"checks"`
	Kernel             string               `json:"kernel"`
	ImageID            string               `json:"image_id"`
	Runtime            string               `json:"runtime,omitempty"`
	PostureFingerprint *string              `json:"posture_fingerprint,omitempty"`
	Error              *string              `json:"error"`
}

func (a *App) selfTestKey() store.Key[SandboxSelfTest] {
	return store.ScopedSetting[SandboxSelfTest](a.store, "sandbox_self_test")
}

// SelfTest runs the containment probe in a real sandbox and records what it observed. It proves the boundary
// rather than describing it: every check is made from inside.
func (a *App) SelfTest(ctx context.Context) (SandboxSelfTest, error) {
	record := SandboxSelfTest{At: model.Now(), Checks: []sandbox.ProbeCheck{}}
	if err := a.admitDiagnostic(); err != nil {
		return record, err
	}
	defer a.wg.Done()
	// A direct HTTP request owns its caller context, but the service still owns
	// the child and must cancel and join it before shutdown closes the store.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	defer stop()
	defer cancel()
	cancellation := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// AfterFunc propagates service cancellation asynchronously.
		return a.ctx.Err()
	}
	if err := cancellation(); err != nil {
		return record, err
	}
	remote, ok := a.sandbox.(*sandbox.Remote)
	if !ok || a.sandbox.Mode() != sandbox.ModeDocker {
		return record, conflictError("The sandbox is off; there is no containment to test")
	}
	before, err := remote.RefreshInfo(ctx)
	if err != nil {
		return record, err
	}
	if _, err := postureFingerprint(before, a.deployment); err != nil {
		return record, err
	}
	record.ImageID, record.Runtime = before.ImageID, before.Runtime
	report, err := sandbox.Probe(ctx, remote)
	if cancelErr := cancellation(); cancelErr != nil {
		// A probe canceled by its caller or shutdown observed nothing about containment; the last result stands.
		return record, cancelErr
	}
	if err != nil {
		record.Error = new(redact.Error(err))
	} else {
		record.Checks, record.Kernel, record.Passed = report.Checks, report.Kernel, report.Passed()
		if report.Sandbox != nil {
			// The probe's own request can move the broker to an image rebuilt under the same tag since the info above
			// was read: the proof belongs to the image it ran on.
			record.ImageID, record.Runtime = report.Sandbox.ImageID, report.Sandbox.Runtime
		}
		if report.Sandbox == nil || report.Sandbox.ImageID == "" {
			record.Passed = false
			record.Error = new("Sandbox probe reported no image identity in its exit evidence")
		}
	}
	after, postureErr := remote.RefreshInfo(ctx)
	if postureErr == nil {
		postureErr = record.bindPosture(before, after, a.deployment)
	}
	if postureErr != nil {
		record.Passed = false
		if record.Error != nil {
			postureErr = errors.Join(errors.New(*record.Error), postureErr)
		}
		record.Error = new(redact.Error(postureErr))
	}
	if err := cancellation(); err != nil {
		return record, err
	}
	if err := store.Put(a.store, a.selfTestKey(), record); err != nil {
		return record, err
	}
	return record, nil
}

func (r SandboxSelfTest) failure() error {
	if r.Error != nil {
		return fmt.Errorf("Sandbox self-test could not run: %s", *r.Error)
	}
	if r.Passed {
		return nil
	}
	failed := []string{}
	for _, check := range r.Checks {
		if !check.Passed {
			failed = append(failed, check.Label+" ("+check.Detail+")")
		}
	}
	return fmt.Errorf("Sandbox self-test failed: %s", strings.Join(failed, "; "))
}
