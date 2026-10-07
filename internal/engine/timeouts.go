package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// sandboxTimeoutLimit reads deployment policy before taking the control gate. Zero means host mode, whose
// children are bounded only by their caller. A Docker broker must report a positive hard limit.
func (a *App) sandboxTimeoutLimit(ctx context.Context) (uint64, error) {
	if a.sandbox.Mode() == sandbox.ModeOff {
		return 0, nil
	}
	remote, ok := a.sandbox.(brokerReporter)
	if !ok {
		return 0, errors.New("Sandbox broker cannot report its timeout limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info, err := remote.RefreshInfo(ctx)
	if err != nil {
		return 0, fmt.Errorf("Reading sandbox timeout policy: %w", err)
	}
	if info.Limits.MaxSeconds == 0 {
		return 0, errors.New("Sandbox broker reports no hard timeout; update or reconfigure the broker")
	}
	return info.Limits.MaxSeconds, nil
}

func validateSandboxTimeouts(cfg config.Config, limit uint64) error {
	if limit == 0 {
		return nil
	}
	if cfg.SessionTimeoutSeconds > limit {
		return fmt.Errorf("Session timeout is %d seconds but the sandbox hard limit is %d; lower session_timeout_seconds or raise OCTOMUS_SANDBOX_MAX_SECONDS and restart the broker", cfg.SessionTimeoutSeconds, limit)
	}
	// Subtract only after checking the minimum, so even an invalid uint64 configuration cannot wrap the sum.
	if limit <= sandbox.VerificationGraceSeconds || cfg.CommandTimeoutSeconds > limit-sandbox.VerificationGraceSeconds {
		return fmt.Errorf("Command timeout is %d seconds plus %d seconds of verification shutdown grace, exceeding the sandbox hard limit of %d; lower command_timeout_seconds or raise OCTOMUS_SANDBOX_MAX_SECONDS and restart the broker", cfg.CommandTimeoutSeconds, sandbox.VerificationGraceSeconds, limit)
	}
	return nil
}

func (a *App) checkSandboxTimeouts(ctx context.Context, cfg config.Config) error {
	limit, err := a.sandboxTimeoutLimit(ctx)
	if err != nil {
		return err
	}
	return validateSandboxTimeouts(cfg, limit)
}
