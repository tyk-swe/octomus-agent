package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
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

// scratchWorkspace makes an empty owned root for route validation, doctor checks and model catalogs, so a sandboxed
// runner started for them can see no repository, workspace or state. Discarding it removes the whole root.
func (a *App) scratchWorkspace() (string, func(), error) {
	parent := filepath.Join(a.dataDir, scratchDir)
	root := filepath.Join(parent, model.ID())
	dir := filepath.Join(root, wire.WorkspaceDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	return dir, func() { _ = workspace.RemoveOwnedDir(parent, root) }, nil
}

// sandboxReady refuses to schedule work while the broker cannot isolate it. The scheduler turns this into an error
// pause with an attention notice; nothing ever falls back to running unsandboxed.
func (a *App) sandboxReady() error {
	remote, ok := a.sandbox.(*sandbox.Remote)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	if _, err := remote.Info(ctx); err != nil {
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
	err = errors.Join(err, clients.Release())
	// Catalog checks belong to no session, so their sandboxes are not attributed to the next turn.
	clients.TakeEvidence()
	return err
}
