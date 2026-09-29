package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// scratchDir holds disposable roots for runner checks that need no repository content.
const scratchDir = "system"

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
	return err
}
