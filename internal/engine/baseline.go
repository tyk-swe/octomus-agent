package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	baselineCommandOutputLimit   = 16 * 1024
	baselineAggregateOutputLimit = 1024 * 1024
)

type baselineJob struct {
	id     string
	cancel context.CancelFunc
}

func boundedOutput(text string, limit int, diagnosticTruncated bool) (string, bool) {
	if limit == 0 {
		return "", diagnosticTruncated || text != ""
	}
	const marker = "\n" + outputTruncatedMarker
	if !diagnosticTruncated && len(text) <= limit {
		return text, false
	}
	if limit < len(marker) {
		return marker[:limit], true
	}
	keep := limit - len(marker)
	if keep > len(text) {
		keep = len(text)
	}
	for keep > 0 && keep < len(text) && !utf8.RuneStart(text[keep]) {
		keep--
	}
	return text[:keep] + marker, true
}

func commandOutput(output *process.ProcessOutput, err error) (string, bool, bool) {
	if err != nil {
		return err.Error(), false, false
	}
	stdout, stderr := output.SafeCaptures()
	var text strings.Builder
	text.WriteString(stdout.Head)
	if len(output.Stderr.Bytes) > 0 {
		text.WriteString("\n[stderr]\n")
		text.WriteString(stderr.Head)
	}
	if !output.Status.Success() {
		text.WriteString("\n" + output.Status.String())
	}
	return text.String(), output.Stdout.Truncated || output.Stderr.Truncated, output.Status.Success()
}

func (a *App) baselineCancelled(id string) (bool, error) {
	return a.Store.MarkerSet("baseline_cancel", id)
}

func (a *App) observeDefaultBranch(cfg config.Config, revision, observedAt string) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return err
	}
	if !live.SameRemoteIdentity(cfg) {
		return errors.New("Configuration identity changed during remote observation")
	}
	return a.mergeDefaultObservationLocked(cfg, revision, observedAt)
}

func (a *App) mergeDefaultObservationLocked(cfg config.Config, revision, observedAt string) error {
	observed, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		return err
	}
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if existing := a.runtime.defaultObservation; existing != nil {
		sameTarget := existing.Describes(cfg)
		newer := true
		if at, parseErr := time.Parse(time.RFC3339Nano, existing.ObservedAt); parseErr == nil {
			newer = !at.Before(observed)
		}
		if sameTarget && newer {
			return nil
		}
	}
	a.runtime.defaultObservation = &model.DefaultBranchObservation{
		Repository: cfg.GitHubRepo, DefaultBranch: cfg.DefaultBranch, Revision: revision, ObservedAt: observedAt,
	}
	return nil
}

func (a *App) baselineRuntimeIneligibility() (*string, error) {
	control, err := a.Control()
	if err != nil {
		return nil, err
	}
	a.runtimeMu.Lock()
	baseline := a.runtime.baseline != nil
	tasks := len(a.runtime.tasks)
	planning := a.runtime.planning()
	reconciling := a.runtime.reconcilingPublication
	a.runtimeMu.Unlock()
	var reason *string
	switch {
	case baseline:
		reason = new("A baseline check is already running")
	case a.ctx.Err() != nil:
		reason = new("The service is shutting down")
	case !control.Paused:
		reason = new("Pause the service before running a baseline check")
	case tasks > 0:
		reason = new("Wait for active tasks before running a baseline check")
	case planning:
		reason = new("Wait for planning to finish before running a baseline check")
	case reconciling:
		reason = new("Wait for publication reconciliation before running a baseline check")
	}
	return reason, nil
}

func (a *App) baselineEligibility() (bool, *string, error) {
	reason, err := a.baselineRuntimeIneligibility()
	if err != nil || reason != nil {
		return false, reason, err
	}
	cfg, err := a.Config()
	if err != nil {
		return false, nil, err
	}
	if err := cfg.ValidateBaseline(); err != nil {
		message := redact.Error(err)
		return false, &message, nil
	}
	return true, nil, nil
}

func (a *App) StartBaseline(expectedRevision string) (*model.BaselineCheck, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return nil, err
	}
	fingerprint, err := live.Fingerprint()
	if err != nil {
		return nil, err
	}
	if fingerprint != expectedRevision {
		return nil, conflictError("The saved configuration changed; reload settings and check the current values")
	}
	reason, err := a.baselineRuntimeIneligibility()
	if err != nil {
		return nil, err
	}
	if reason != nil {
		return nil, conflictError(*reason)
	}
	if err := live.ValidateBaseline(); err != nil {
		return nil, err
	}
	check := model.BaselineCheck{
		ID:                model.ID(),
		Status:            model.BaselineStatusRunning,
		Config:            live.Clone(),
		ConfigFingerprint: fingerprint,
		StartedAt:         model.Now(),
		Commands:          []model.BaselineCommand{},
	}
	if err := a.Store.BeginBaseline(check); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.runtimeMu.Lock()
	a.runtime.baseline = &baselineJob{id: check.ID, cancel: cancel}
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		a.baselineWorker(ctx, check.ID)
	}()
	return &check, nil
}

func (a *App) CancelBaseline(id string) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	check, err := store.Get[model.BaselineCheck](a.Store, "baseline", id)
	if err != nil {
		return err
	}
	if check == nil {
		return conflictError("Baseline check not found")
	}
	if check.Status != model.BaselineStatusRunning {
		return conflictError("The baseline check already finished")
	}
	a.runtimeMu.Lock()
	var cancel context.CancelFunc
	if a.runtime.baseline != nil && a.runtime.baseline.id == id {
		cancel = a.runtime.baseline.cancel
	}
	a.runtimeMu.Unlock()
	if err := a.Store.Put("baseline_cancel", id, model.Now()); err != nil {
		return err
	}
	if cancel == nil {
		// A refused terminal write can leave a running record after its worker exits.
		// Let the operator settle it once storage recovers, preserving its evidence.
		if err := a.abandonBaseline(check,
			"Cancelled by the operator after the check worker exited",
			"Baseline check worker exited unexpectedly"); err != nil {
			return err
		}
		// Like the worker path, activity is best-effort after terminal state commits.
		_ = a.Store.Event(id, "baseline", baselineStatusDebug[check.Status])
		return nil
	}
	cancel()
	return nil
}

func baselineConfigMatches(check *model.BaselineCheck, live config.Config) bool {
	fingerprint, err := live.Fingerprint()
	return err == nil && fingerprint == check.ConfigFingerprint
}

func (a *App) baselineRevisionStatus(check *model.BaselineCheck, live config.Config) string {
	if !live.SameRemoteIdentity(check.Config) {
		return "unknown"
	}
	a.runtimeMu.Lock()
	observation := a.runtime.defaultObservation
	a.runtimeMu.Unlock()
	if observation == nil {
		return "unknown"
	}
	fresh := false
	if at, err := time.Parse(time.RFC3339Nano, observation.ObservedAt); err == nil {
		age := time.Since(at)
		fresh = age >= 0 && age <= observationLifetime
	}
	sameTarget := observation.Describes(check.Config)
	switch {
	case check.Revision != nil && fresh && sameTarget && *check.Revision == observation.Revision:
		return "matches_last_observation"
	case check.Revision != nil && fresh && sameTarget:
		return "stale"
	default:
		return "unknown"
	}
}

const baselineCaveat = "A baseline check verifies the saved commands on a clone made at its start time; it does not prove later host, tool or remote health and is not publication evidence."

func (a *App) BaselineView(id *string) (map[string]any, error) {
	var check *model.BaselineCheck
	var err error
	if id != nil {
		check, err = store.Get[model.BaselineCheck](a.Store, "baseline", *id)
	} else {
		check, err = a.Store.LatestBaseline()
	}
	if err != nil {
		return nil, err
	}
	eligible, reason, err := a.baselineEligibility()
	if err != nil {
		return nil, err
	}
	live, err := a.Config()
	if err != nil {
		return nil, err
	}
	var configMatches any
	var configRevision any
	revisionStatus := "unknown"
	if check != nil {
		configMatches = baselineConfigMatches(check, live)
		configRevision = check.ConfigFingerprint
		revisionStatus = a.baselineRevisionStatus(check, live)
	}
	a.runtimeMu.Lock()
	observation := a.runtime.defaultObservation
	a.runtimeMu.Unlock()
	if observation != nil && !observation.Describes(live) {
		observation = nil
	}
	var reasonValue any
	if reason != nil {
		reasonValue = *reason
	}
	return map[string]any{
		"check":               check,
		"eligible":            eligible,
		"reason":              reasonValue,
		"config_matches":      configMatches,
		"config_revision":     configRevision,
		"revision_status":     revisionStatus,
		"default_observation": observation,
		"caveat":              baselineCaveat,
	}, nil
}

func (a *App) recoverBaselines() error {
	checks, err := a.Store.RunningBaselines()
	if err != nil {
		return err
	}
	for i := range checks {
		if err := a.abandonBaseline(&checks[i],
			"The operator cancelled this check before the service stopped",
			"The service stopped while the baseline check was running"); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) abandonBaseline(check *model.BaselineCheck, cancelled, interrupted string) error {
	marked, err := a.baselineCancelled(check.ID)
	if err != nil {
		return err
	}
	check.Status = model.BaselineStatusInterrupted
	message := interrupted
	if marked {
		check.Status = model.BaselineStatusCancelled
		message = cancelled
	}
	check.CompletedAt = stringPointer(model.Now())
	check.Error = &message
	return a.Store.Put("baseline", check.ID, *check)
}

func (a *App) removeBaselineWorkspace(check *model.BaselineCheck) error {
	if _, err := uuid.Parse(check.ID); err != nil {
		return errors.New("Invalid baseline identity")
	}
	if !a.claimCleanup(cleanupBaseline, check.ID) {
		return nil
	}
	root := filepath.Join(a.DataDir, "baselines")
	removeErr := a.removeDir(root, filepath.Join(root, check.ID))
	a.gate.Lock()
	defer func() {
		a.releaseCleanup(cleanupBaseline, check.ID)
		a.gate.Unlock()
	}()
	var cleanupError *string
	if removeErr != nil {
		message := redact.Error(removeErr)
		cleanupError = &message
	}
	if removeErr == nil {
		check.WorkspaceRemoved = true
	}
	check.CleanupError = cleanupError
	current, err := store.Get[model.BaselineCheck](a.Store, "baseline", check.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if removeErr == nil {
		current.WorkspaceRemoved = true
	}
	current.CleanupError = cleanupError
	return a.Store.Put("baseline", check.ID, *current)
}

var baselineStatusDebug = map[model.BaselineStatus]string{
	model.BaselineStatusRunning: "Running", model.BaselineStatusPassed: "Passed",
	model.BaselineStatusFailed: "Failed", model.BaselineStatusCancelled: "Cancelled",
	model.BaselineStatusTimedOut: "TimedOut", model.BaselineStatusInterrupted: "Interrupted",
}

func (a *App) baselineWorker(ctx context.Context, id string) {
	defer func() {
		if check, err := store.Get[model.BaselineCheck](a.Store, "baseline", id); err == nil && check != nil && check.Status == model.BaselineStatusRunning {
			_ = a.abandonBaseline(check,
				"Cancelled by the operator; the check worker exited unexpectedly",
				"Baseline check worker exited unexpectedly")
		}
		a.runtimeMu.Lock()
		if a.runtime.baseline != nil && a.runtime.baseline.id == id {
			a.runtime.baseline = nil
		}
		a.runtimeMu.Unlock()
		a.notify()
	}()
	check, err := store.Get[model.BaselineCheck](a.Store, "baseline", id)
	if err != nil || check == nil || check.Status != model.BaselineStatusRunning {
		return
	}
	c := check.Config
	limit := time.Duration(c.TaskTimeoutSeconds) * time.Second
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	executionDone := make(chan struct{})
	result := process.WithDeadline(ctx, workCancel, limit, func() model.BaselineStatus {
		defer close(executionDone)
		status, err := a.executeBaseline(workCtx, check)
		if err != nil {
			check.Error = stringPointer(redact.Error(err))
			switch {
			// A sandbox that could not run a command says nothing about the repository's baseline.
			case workCtx.Err() != nil, sandbox.Infrastructure(err):
				return model.BaselineStatusInterrupted
			case process.IsDeadlineElapsed(err):
				return model.BaselineStatusTimedOut
			default:
				return model.BaselineStatusFailed
			}
		}
		return status
	})
	<-executionDone
	var status model.BaselineStatus
	a.gate.Lock()
	if result.Expired {
		if result.AlreadyCancelled {
			status = model.BaselineStatusInterrupted
		} else {
			check.Error = stringPointer(fmt.Sprintf("Baseline check exceeded the %d second overall limit", c.TaskTimeoutSeconds))
			status = model.BaselineStatusTimedOut
		}
	} else {
		status = result.Output
	}
	marked, markerErr := a.baselineCancelled(id)
	switch {
	case markerErr != nil:
		status = model.BaselineStatusInterrupted
		check.Error = stringPointer(redact.Text("Cancel state unreadable, refusing a clean result: " + markerErr.Error()))
	case marked:
		status = model.BaselineStatusCancelled
		check.Error = stringPointer("Cancelled by the operator")
	case a.ctx.Err() != nil && status != model.BaselineStatusPassed:
		status = model.BaselineStatusInterrupted
	}
	check.Status = status
	check.CompletedAt = stringPointer(model.Now())
	if err := a.Store.Put("baseline", id, *check); err != nil {
		_ = a.Store.Event(id, "baseline_error", redact.Error(err))
	} else {
		_ = a.Store.Event(id, "baseline", baselineStatusDebug[status])
	}
	a.gate.Unlock()
	if err := a.removeBaselineWorkspace(check); err != nil {
		_ = a.Store.Event(id, "cleanup_error", redact.Error(err))
	}
}

func (a *App) executeBaseline(ctx context.Context, check *model.BaselineCheck) (model.BaselineStatus, error) {
	c := check.Config
	measured, err := a.measureFor(filepath.Join("baselines", check.ID))
	if err != nil {
		return model.BaselineStatusRunning, err
	}
	if measured >= c.MaxWorkspaceBytes {
		return model.BaselineStatusRunning, store.StorageLimitError(measured)
	}
	if err := gitops.ValidateRemote(ctx, c); err != nil {
		return model.BaselineStatusRunning, err
	}
	observedAt := model.Now()
	revision, err := gitops.RemoteRevision(ctx, c, c.DefaultBranch)
	if err != nil {
		return model.BaselineStatusRunning, err
	}
	if revision == nil {
		return model.BaselineStatusRunning, errors.New("Default branch missing on remote")
	}
	check.Revision = revision
	if err := a.Store.Put("baseline", check.ID, *check); err != nil {
		return model.BaselineStatusRunning, err
	}
	if err := a.observeDefaultBranch(c, *revision, observedAt); err != nil {
		return model.BaselineStatusRunning, err
	}
	if err := gitops.Fetch(ctx, c); err != nil {
		return model.BaselineStatusRunning, err
	}
	workspaceDir := filepath.Join(a.DataDir, "baselines", check.ID, "workspace")
	if err := gitops.CloneAt(ctx, c, workspaceDir, *revision); err != nil {
		return model.BaselineStatusRunning, err
	}
	intact, err := gitops.At(ctx, c, workspaceDir, *revision)
	if err != nil {
		return model.BaselineStatusRunning, err
	}
	if !intact {
		return model.BaselineStatusRunning, errors.New("Cloned workspace does not match the identified revision")
	}
	allOK := true
	remaining := baselineAggregateOutputLimit
	for i, command := range c.VerificationCommands {
		if ctx.Err() != nil {
			return model.BaselineStatusRunning, process.ErrCancelled
		}
		outcome := runCheckCommand(ctx, a.sandbox, c, workspaceDir, command, *revision, i == 0)
		if ctx.Err() == nil && outcome.sandboxFailed() {
			a.keepSandboxEvidence(check.ID, command, outcome.sandbox)
			return model.BaselineStatusRunning, sandboxFailure(command, outcome.capture)
		}
		timedOut := process.IsDeadlineElapsed(outcome.capture)
		text, diagnosticTruncated, success := commandOutput(outcome.captured, outcome.capture)
		var failure error
		if ctx.Err() != nil {
			success = false
			failure = process.ErrCancelled
		} else {
			switch {
			case outcome.intactErr != nil:
				success = false
				text += "\n" + outcome.intactErr.Error()
				failure = fmt.Errorf("Workspace state check failed during verification: %w", outcome.intactErr)
			case !outcome.intact:
				success = false
				text += "\nWorkspace or HEAD changed during this verification command"
				failure = errors.New("Workspace or HEAD changed during verification")
			}
		}
		limit := remaining
		if limit > baselineCommandOutputLimit {
			limit = baselineCommandOutputLimit
		}
		output, outputTruncated := boundedOutput(redact.Secrets(text), limit, diagnosticTruncated)
		remaining -= len(output)
		if remaining < 0 {
			remaining = 0
		}
		check.Commands = append(check.Commands, model.BaselineCommand{
			Command:         command,
			Success:         success,
			Output:          output,
			OutputTruncated: outputTruncated,
			CreatedAt:       model.Now(),
			Sandbox:         outcome.sandbox,
		})
		if err := a.Store.Put("baseline", check.ID, *check); err != nil {
			return model.BaselineStatusRunning, err
		}
		if timedOut {
			return model.BaselineStatusTimedOut, nil
		}
		if failure != nil {
			return model.BaselineStatusRunning, failure
		}
		if !success {
			allOK = false
		}
	}
	if allOK {
		return model.BaselineStatusPassed, nil
	}
	return model.BaselineStatusFailed, nil
}
