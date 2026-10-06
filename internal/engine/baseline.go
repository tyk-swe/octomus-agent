package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// A baseline check's output is capped at outputLimit bytes per command and baselineTotalLimit bytes per check.
const baselineTotalLimit = 1 << 20

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
	reason, err := a.baselineBlocker()
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
		_ = a.Store.Event(id, "baseline", baselineLabels[check.Status])
		return nil
	}
	cancel()
	return nil
}

// baselineBlocker is why the runtime or operating state keeps a baseline check from starting now, or nil.
func (a *App) baselineBlocker() (*string, error) {
	control, err := a.Control()
	if err != nil {
		return nil, err
	}
	a.runtimeMu.Lock()
	baseline := a.runtime.baseline != nil
	tasks := len(a.runtime.tasks)
	planning := a.runtime.planning()
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
	}
	return reason, nil
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
	if observation == nil || observation.Revision == "" {
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

var baselineLabels = map[model.BaselineStatus]string{
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
	limit := c.TaskTimeout()
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	executionDone := make(chan struct{})
	result := process.WithDeadline(ctx, workCancel, limit, func() model.BaselineStatus {
		defer close(executionDone)
		status, err := a.executeBaseline(workCtx, check)
		if err != nil {
			check.Error = new(redact.Error(err))
			switch {
			// A sandbox that could not run a command says nothing about the repository's baseline.
			case workCtx.Err() != nil, sandbox.Infrastructure(err):
				return model.BaselineStatusInterrupted
			case errors.Is(err, process.ErrDeadlineElapsed):
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
			check.Error = new(fmt.Sprintf("Baseline check exceeded the %d second overall limit", c.TaskTimeoutSeconds))
			status = model.BaselineStatusTimedOut
		}
	} else {
		status = result.Output
	}
	marked, markerErr := a.Store.Marked("baseline_cancel", id)
	switch {
	case markerErr != nil:
		status = model.BaselineStatusInterrupted
		check.Error = new(redact.Text("Cancel state unreadable, refusing a clean result: " + markerErr.Error()))
	case marked:
		status = model.BaselineStatusCancelled
		check.Error = new("Cancelled by the operator")
	case a.ctx.Err() != nil && status != model.BaselineStatusPassed:
		status = model.BaselineStatusInterrupted
	}
	check.Status = status
	check.CompletedAt = new(model.Now())
	if err := a.Store.Put("baseline", id, *check); err != nil {
		_ = a.Store.Event(id, "baseline_error", redact.Error(err))
	} else {
		_ = a.Store.Event(id, "baseline", baselineLabels[status])
	}
	a.gate.Unlock()
	if err := a.removeBaselineWorkspace(check); err != nil {
		_ = a.Store.Event(id, "cleanup_error", redact.Error(err))
	}
}

func (a *App) executeBaseline(ctx context.Context, check *model.BaselineCheck) (model.BaselineStatus, error) {
	c := check.Config
	measured, err := a.measure(filepath.Join("baselines", check.ID))
	if err != nil {
		return model.BaselineStatusRunning, err
	}
	if measured >= c.MaxWorkspaceBytes {
		return model.BaselineStatusRunning, store.StorageLimitError(measured)
	}
	if err := gitops.ValidateRemote(ctx, c); err != nil {
		return model.BaselineStatusRunning, err
	}
	revision, observedAt, err := a.defaultBranchSHA(ctx, c)
	if err != nil {
		return model.BaselineStatusRunning, err
	}
	check.Revision = &revision
	if err := a.Store.Put("baseline", check.ID, *check); err != nil {
		return model.BaselineStatusRunning, err
	}
	if err := a.observeDefaultBranch(c, revision, observedAt); err != nil {
		return model.BaselineStatusRunning, err
	}
	if err := gitops.Fetch(ctx, c); err != nil {
		return model.BaselineStatusRunning, err
	}
	workspaceDir := filepath.Join(a.dataDir, "baselines", check.ID, "workspace")
	if err := gitops.CloneAt(ctx, c, workspaceDir, revision); err != nil {
		return model.BaselineStatusRunning, err
	}
	intact, err := gitops.At(ctx, c, workspaceDir, revision)
	if err != nil {
		return model.BaselineStatusRunning, err
	}
	if !intact {
		return model.BaselineStatusRunning, errors.New("Cloned workspace does not match the identified revision")
	}
	allOK := true
	remaining := baselineTotalLimit
	for i, command := range c.VerificationCommands {
		if ctx.Err() != nil {
			return model.BaselineStatusRunning, process.ErrCancelled
		}
		outcome := runCheckCommand(ctx, a.sandbox, c, workspaceDir, command, revision, i == 0)
		if ctx.Err() == nil && outcome.sandboxFailed() {
			a.keepSandboxEvidence(check.ID, command, outcome.sandbox)
			return model.BaselineStatusRunning, sandboxFailure(command, outcome.capture)
		}
		timedOut := errors.Is(outcome.capture, process.ErrDeadlineElapsed)
		success := !outcome.failed()
		note := ""
		var failure error
		if ctx.Err() != nil {
			success = false
			failure = process.ErrCancelled
		} else {
			switch {
			case outcome.intactErr != nil:
				success = false
				note = "\n" + outcome.intactErr.Error()
				failure = fmt.Errorf("Workspace state check failed during verification: %w", outcome.intactErr)
			case !outcome.intact:
				success = false
				note = "\nWorkspace or HEAD changed during this verification command"
				failure = errors.New("Workspace or HEAD changed during verification")
			}
		}
		output, outputTruncated := commandOutput(outcome, note, min(remaining, outputLimit))
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

// commandOutput is a baseline command's evidence: stdout, then any stderr in a [stderr] section, then the exit status
// on failure and the workspace note, secret-scrubbed and bounded to limit bytes keeping the head. The flag reports a
// cut, including one the diagnostic capture made before this bound.
func commandOutput(outcome checkOutcome, note string, limit int) (string, bool) {
	var text strings.Builder
	captureTruncated := false
	if outcome.capture != nil {
		text.WriteString(outcome.capture.Error())
	} else {
		stdout, stderr := outcome.captured.SafeCaptures()
		text.WriteString(stdout.Head)
		if len(outcome.captured.Stderr.Bytes) > 0 {
			text.WriteString("\n[stderr]\n")
			text.WriteString(stderr.Head)
		}
		if !outcome.captured.Status.Success() {
			text.WriteString("\n" + outcome.captured.Status.String())
		}
		captureTruncated = outcome.captured.Stdout.Truncated || outcome.captured.Stderr.Truncated
	}
	text.WriteString(note)
	scrubbed := redact.Secrets(text.String())
	if captureTruncated {
		// The capture already dropped output, so the evidence is marked even when what remains fits the bound.
		scrubbed += "\n" + truncatedMarker
	}
	output, cut := bound(scrubbed, limit, false)
	return output, cut || captureTruncated
}

func (a *App) removeBaselineWorkspace(check *model.BaselineCheck) error {
	if _, err := uuid.Parse(check.ID); err != nil {
		return errors.New("Invalid baseline identity")
	}
	root := filepath.Join(a.dataDir, "baselines")
	a.gate.Lock()
	defer a.gate.Unlock()
	removeErr := a.removeOwnedRoot(cleanupBaseline, check.ID, root, filepath.Join(root, check.ID))
	var conflict *actionConflict
	if errors.As(removeErr, &conflict) {
		// Another cleanup owns the root and records its own outcome.
		return nil
	}
	var cleanupError *string
	if removeErr != nil {
		cleanupError = new(redact.Error(removeErr))
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
