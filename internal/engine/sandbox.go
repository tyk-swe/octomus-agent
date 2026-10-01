package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// scratchDir holds disposable roots for runner checks that need no repository content.
const scratchDir = "system"

// Deployment is what the host deployment fixes regardless of saved policy. In Docker mode it pins the trusted checkout
// and repository identity, so the operator token cannot point the orchestrator's own git at a tree a sandbox can
// write, and it carries the egress allowlists the gateway enforces, for display.
type Deployment struct {
	Repository string
	GitHubRepo string
	Egress     map[string][]string
}

// WithDeployment applies the host deployment's fixed settings.
func WithDeployment(deployment Deployment) Option {
	return func(a *App) { a.deployment = deployment }
}

func (d Deployment) pinned() bool { return d.Repository != "" }

func (d Deployment) pin(cfg config.Config) config.Config {
	if d.pinned() {
		cfg.Repository, cfg.GitHubRepo = d.Repository, d.GitHubRepo
	}
	return cfg
}

func (d Deployment) check(cfg config.Config) error {
	if d.pinned() && (!config.SamePath(cfg.Repository, d.Repository) || !config.EqualASCII(cfg.GitHubRepo, d.GitHubRepo)) {
		return errors.New("The repository is fixed by this deployment (OCTOMUS_GITHUB_REPO); change it there and restart")
	}
	return nil
}

// Deployment reports the host deployment's fixed settings.
func (a *App) Deployment() Deployment { return a.deployment }

// Sandbox is the backend every untrusted child of this engine starts in.
func (a *App) Sandbox() sandbox.Backend { return a.sandbox }

// scratchWorkspace makes an empty owned root for route validation, doctor checks and model catalogs, so a sandboxed
// runner started for them can see no repository, workspace or state. Discarding it removes the whole root.
func (a *App) scratchWorkspace() (string, func(), error) {
	parent := filepath.Join(a.DataDir, scratchDir)
	root := filepath.Join(parent, uuid.NewString())
	dir := filepath.Join(root, "workspace")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	return dir, func() { _ = workspace.RemoveOwnedDir(parent, root) }, nil
}

// healthChecker is a sandbox backend that can be unavailable, as a broker can.
type healthChecker interface {
	Healthy(ctx context.Context) error
}

// sandboxReady refuses to schedule work while the sandbox backend cannot isolate it. The scheduler turns this into an
// error pause with an attention notice; nothing ever falls back to running unsandboxed.
func (a *App) sandboxReady() error {
	checker, ok := a.sandbox.(healthChecker)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	if err := checker.Healthy(ctx); err != nil {
		return fmt.Errorf("Sandbox unavailable; no work starts without it: %w", err)
	}
	return nil
}

// validateRoutes checks every route's catalog from a scratch root and stops the runners it started.
func (a *App) validateRoutes(clients *runner.Runners, cfg config.Config, audit bool) error {
	scratch, discard, err := a.scratchWorkspace()
	if err != nil {
		return err
	}
	defer discard()
	err = clients.ValidateRoutes(cfg, scratch, audit)
	if released := clients.Release(); err == nil {
		err = released
	}
	// Catalog checks belong to no session, so their sandboxes are not attributed to the next turn.
	clients.TakeEvidence()
	return err
}

// SandboxSelfTest is the latest containment probe, kept for the dashboard.
type SandboxSelfTest struct {
	At      string               `json:"at"`
	Passed  bool                 `json:"passed"`
	Checks  []sandbox.ProbeCheck `json:"checks"`
	Kernel  string               `json:"kernel"`
	ImageID string               `json:"image_id"`
	Runtime string               `json:"runtime,omitempty" wire:"default"`
	Error   *string              `json:"error"`
}

const selfTestRecord = "sandbox_self_test"

// SelfTest runs the containment probe in a real sandbox and records what it observed. It proves the boundary
// rather than describing it: every check is made from inside.
func (a *App) SelfTest(ctx context.Context) (SandboxSelfTest, error) {
	record := SandboxSelfTest{At: model.Now(), Checks: []sandbox.ProbeCheck{}}
	if a.sandbox.Mode() != sandbox.ModeDocker {
		return record, conflictError("The sandbox is off; there is no containment to test")
	}
	if remote, ok := a.sandbox.(*sandbox.Remote); ok {
		if info, err := remote.Info(ctx); err == nil {
			record.ImageID, record.Runtime = info.ImageID, info.Runtime
		}
	}
	report, err := sandbox.Probe(ctx, a.sandbox)
	if err != nil && ctx.Err() != nil {
		// A probe its caller abandoned observed nothing about containment; the last result stands.
		return record, err
	}
	if err != nil {
		message := redact.Error(err)
		record.Error = &message
	} else {
		record.Checks, record.Kernel, record.Passed = report.Checks, report.Kernel, report.Passed()
		if report.Sandbox != nil {
			// The probe's own request can move the broker to an image rebuilt under the same tag since the info above
			// was read: the proof belongs to the image it ran on.
			record.ImageID, record.Runtime = report.Sandbox.ImageID, report.Sandbox.Runtime
		}
	}
	if err := a.Store.Put("settings", selfTestRecord, record); err != nil {
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
		repo := a.deployment.GitHubRepo
		posture.PinnedRepository = &repo
	}
	if remote, ok := a.sandbox.(*sandbox.Remote); ok {
		ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
		info, err := remote.Info(ctx)
		cancel()
		if err != nil {
			message := redact.Error(err)
			posture.Healthy, posture.Error = false, &message
		} else {
			posture.Broker = &info
		}
	}
	if selfTest, err := store.Get[SandboxSelfTest](a.Store, "settings", selfTestRecord); err == nil && selfTest != nil {
		// A saved proof only counts while it names the broker's current image and runtime; anything else predates
		// a posture change and needs a fresh self-test.
		if posture.Broker != nil && selfTest.ImageID == posture.Broker.ImageID && selfTest.Runtime == posture.Broker.Runtime {
			posture.SelfTest = selfTest
		}
	}
	return posture
}
