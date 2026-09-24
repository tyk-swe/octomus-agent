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
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const (
	baselineCommandOutputLimit   = 16 * 1024
	baselineAggregateOutputLimit = 1024 * 1024
	observationFreshSeconds      = 300
)

// BaselineConflict is an operator-visible HTTP 409 conflict:
// ineligible starts, stale expected configurations and invalid cancellations.
type BaselineConflict struct{ message string }

func (e *BaselineConflict) Error() string { return e.message }

func baselineConflict(message string) error { return &BaselineConflict{message} }

// baselineJob is the live check's cancellation handle and identity; clearing it
// is the worker's last act (the guard in baselineWorker).
type baselineJob struct {
	id     string
	cancel context.CancelFunc
}

// BaselineFingerprint is the exact-configuration identity a check recorded at
// start; the view compares it against the live configuration's fingerprint.
func BaselineFingerprint(cfg config.Config) (string, error) { return cfg.Fingerprint() }

// boundedOutput shortens output to limit bytes on a UTF-8 boundary, appending
// the truncation marker inside the limit, and reports whether anything was cut.
func boundedOutput(text string, limit int, diagnosticTruncated bool) (string, bool) {
	if limit == 0 {
		return "", diagnosticTruncated || text != ""
	}
	const marker = "\n[output truncated]"
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

// commandOutput renders one captured command:
// bounded stdout, then a [stderr] section, then the exit status on failure.
// The middle return reports capture-level truncation only; boundedOutput
// measures over-limit text itself, in the same byte unit.
func commandOutput(output *process.ProcessOutput, err error) (string, bool, bool) {
	if err != nil {
		return err.Error(), false, false
	}
	var text strings.Builder
	text.WriteString(strings.ToValidUTF8(string(output.Stdout.Bytes), "�"))
	if len(output.Stderr.Bytes) > 0 {
		text.WriteString("\n[stderr]\n")
		text.WriteString(strings.ToValidUTF8(string(output.Stderr.Bytes), "�"))
	}
	if !output.Status.Success() {
		text.WriteString("\n" + output.Status.String())
	}
	return text.String(), output.Stdout.Truncated || output.Stderr.Truncated, output.Status.Success()
}

// runCheckCommand and checkOutcome are shared with task verification and live
// in execution.go.

// baselineCancelled reports the durable operator-cancel marker.
func (a *App) baselineCancelled(id string) (bool, error) {
	return a.Store.MarkerSet("baseline_cancel", id)
}

// observeDefaultBranch records a fresh remote default-branch observation under
// the gate after revalidating that the live configuration still describes the
// same remote.
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

// mergeDefaultObservationLocked updates the in-memory observation while the
// gate is held; a newer same-target observation already recorded wins.
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

// baselineEligibility reports whether a check may start and the operator-facing
// reason when not.
func (a *App) baselineEligibility() (bool, *string, error) {
	control, err := a.Control()
	if err != nil {
		return false, nil, err
	}
	a.runtimeMu.Lock()
	baseline := a.runtime.baseline != nil
	tasks := len(a.runtime.tasks)
	planning := a.runtime.cycle != nil || a.runtime.preflight
	reconciling := a.runtime.reconcilingPublication
	a.runtimeMu.Unlock()
	text := func(s string) *string { return &s }
	var reason *string
	switch {
	case baseline:
		reason = text("A baseline check is already running")
	case a.ctx.Err() != nil:
		reason = text("The service is shutting down")
	case !control.Paused:
		reason = text("Pause the service before running a baseline check")
	case tasks > 0:
		reason = text("Wait for active tasks before running a baseline check")
	case planning:
		reason = text("Wait for planning to finish before running a baseline check")
	case reconciling:
		reason = text("Wait for publication reconciliation before running a baseline check")
	default:
		if cfg, cfgErr := a.Config(); cfgErr != nil {
			return false, nil, cfgErr
		} else if err := cfg.ValidateBaseline(); err != nil {
			reason = text(store.ErrorMessage(err))
		}
	}
	return reason == nil, reason, nil
}

// StartBaseline validates the expected configuration against the live one,
// persists a running check and starts its worker. The whole eligibility check
// and launch serialize on the gate.
func (a *App) StartBaseline(expected config.Config) (*model.BaselineCheck, error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return nil, err
	}
	expectedJSON, err := expected.MarshalJSON()
	if err != nil {
		return nil, err
	}
	liveJSON, err := live.MarshalJSON()
	if err != nil {
		return nil, err
	}
	if string(liveJSON) != string(expectedJSON) {
		return nil, baselineConflict("The saved configuration changed; reload settings and check the current values")
	}
	if err := live.ValidateBaseline(); err != nil {
		return nil, err
	}
	eligible, reason, err := a.baselineEligibility()
	if err != nil {
		return nil, err
	}
	if !eligible {
		message := "Baseline check is not eligible"
		if reason != nil {
			message = *reason
		}
		return nil, baselineConflict(message)
	}
	check := model.BaselineCheck{
		ID:        model.ID(),
		Status:    model.BaselineStatusRunning,
		Config:    live.Clone(),
		StartedAt: model.Now(),
		Commands:  []model.BaselineCommand{},
	}
	fingerprint, err := BaselineFingerprint(live)
	if err != nil {
		return nil, err
	}
	check.ConfigFingerprint = fingerprint
	if err := a.Store.Put("baseline", check.ID, check); err != nil {
		return nil, err
	}
	if err := a.Store.Put("settings", "baseline_latest", check.ID); err != nil {
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
		a.baselineWorker(check.ID, ctx)
	}()
	return &check, nil
}

// CancelBaseline records the durable cancel intent and interrupts the worker.
func (a *App) CancelBaseline(id string) error {
	a.gate.Lock()
	defer a.gate.Unlock()
	check, err := store.Get[model.BaselineCheck](a.Store, "baseline", id)
	if err != nil {
		return err
	}
	if check == nil {
		return baselineConflict("Baseline check not found")
	}
	if check.Status != model.BaselineStatusRunning {
		return baselineConflict("The baseline check already finished")
	}
	a.runtimeMu.Lock()
	var cancel context.CancelFunc
	if a.runtime.baseline != nil && a.runtime.baseline.id == id {
		cancel = a.runtime.baseline.cancel
	}
	a.runtimeMu.Unlock()
	if cancel == nil {
		return baselineConflict("The baseline check is no longer running")
	}
	if err := a.Store.Put("baseline_cancel", id, model.Now()); err != nil {
		return err
	}
	cancel()
	return nil
}

// BaselineConfigMatches reports whether the live configuration still matches
// the fingerprint a check recorded at start.
func (a *App) BaselineConfigMatches(check *model.BaselineCheck, live config.Config) bool {
	fingerprint, err := BaselineFingerprint(live)
	return err == nil && fingerprint == check.ConfigFingerprint
}

// BaselineRevisionStatus compares a check's recorded revision to the freshest
// in-memory default-branch observation of the same remote.
func (a *App) BaselineRevisionStatus(check *model.BaselineCheck, live config.Config) string {
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
		age := time.Since(at).Seconds()
		fresh = age >= 0 && age <= observationFreshSeconds
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

// BaselineView assembles the dashboard payload for one check (or the latest).
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
	revisionStatus := "unknown"
	if check != nil {
		configMatches = a.BaselineConfigMatches(check, live)
		revisionStatus = a.BaselineRevisionStatus(check, live)
	}
	a.runtimeMu.Lock()
	observation := a.runtime.defaultObservation
	a.runtimeMu.Unlock()
	var reasonValue any
	if reason != nil {
		reasonValue = *reason
	}
	return map[string]any{
		"check":               check,
		"eligible":            eligible,
		"reason":              reasonValue,
		"config_matches":      configMatches,
		"revision_status":     revisionStatus,
		"default_observation": observation,
		"caveat":              baselineCaveat,
	}, nil
}

// recoverBaselines turns checks left running by a stop into durable terminal
// records: operator-cancelled when the marker exists, interrupted otherwise.
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

// abandonBaseline records that a running check was abandoned, with the
// operator-cancelled and worker-interrupted wording each caller supplies.
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

// CleanupBaseline removes the check's owned clone directory and records the
// outcome; a refusal is evidence, not a worker failure.
func (a *App) CleanupBaseline(check *model.BaselineCheck) error {
	root := filepath.Join(a.DataDir, "baselines")
	if _, err := uuid.Parse(check.ID); err != nil {
		return errors.New("Invalid baseline identity")
	}
	path := filepath.Join(root, check.ID)
	if err := workspace.RemoveOwnedDir(root, path); err != nil {
		message := store.ErrorMessage(err)
		check.CleanupError = &message
	} else {
		check.WorkspaceRemoved = true
		check.CleanupError = nil
	}
	return a.Store.Put("baseline", check.ID, *check)
}

var baselineStatusDebug = map[model.BaselineStatus]string{
	model.BaselineStatusRunning: "Running", model.BaselineStatusPassed: "Passed",
	model.BaselineStatusFailed: "Failed", model.BaselineStatusCancelled: "Cancelled",
	model.BaselineStatusTimedOut: "TimedOut", model.BaselineStatusInterrupted: "Interrupted",
}

// baselineWorker runs the check under its overall deadline, resolves the final
// status under the gate and always clears the runtime slot via the guard. The
// guard also abandons a still-running record if the worker exits unexpectedly.
func (a *App) baselineWorker(id string, ctx context.Context) {
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
	executionDone := make(chan struct{})
	result := process.WithDeadline(ctx, workCancel, limit, func() model.BaselineStatus {
		defer close(executionDone)
		status, err := a.executeBaseline(workCtx, check)
		if err != nil {
			check.Error = stringPointer(store.ErrorMessage(err))
			switch {
			case workCtx.Err() != nil:
				return model.BaselineStatusInterrupted
			case process.IsDeadlineElapsed(err):
				return model.BaselineStatusTimedOut
			default:
				return model.BaselineStatusFailed
			}
		}
		return status
	})
	// WithDeadline's cleanup grace is bounded, but the callback owns the check
	// record and its incremental writes. Join it before the terminal write.
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
		check.Error = stringPointer(store.Redact("Cancel state unreadable, refusing a clean result: " + markerErr.Error()))
	case marked:
		status = model.BaselineStatusCancelled
		check.Error = stringPointer("Cancelled by the operator")
	case a.ctx.Err() != nil && status != model.BaselineStatusPassed:
		status = model.BaselineStatusInterrupted
	}
	check.Status = status
	check.CompletedAt = stringPointer(model.Now())
	if err := a.Store.Put("baseline", id, *check); err != nil {
		_ = a.Store.Event(id, "baseline_error", store.ErrorMessage(err))
	}
	_ = a.Store.Event(id, "baseline", baselineStatusDebug[status])
	a.gate.Unlock()
	if err := a.CleanupBaseline(check); err != nil {
		_ = a.Store.Event(id, "cleanup_error", store.ErrorMessage(err))
	}
}

// executeBaseline verifies the saved commands on a disposable clone made at the
// remote default branch revision recorded at start.
func (a *App) executeBaseline(ctx context.Context, check *model.BaselineCheck) (model.BaselineStatus, error) {
	c := check.Config
	measured, err := workspace.DirectorySize(a.DataDir)
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
	for _, command := range c.VerificationCommands {
		if err := ctx.Err(); err != nil {
			return model.BaselineStatusRunning, errors.New("Operation cancelled")
		}
		if marked, err := a.baselineCancelled(check.ID); err != nil {
			return model.BaselineStatusRunning, err
		} else if marked {
			return model.BaselineStatusRunning, errors.New("Operation cancelled")
		}
		outcome := runCheckCommand(ctx, c, workspaceDir, command, *revision)
		timedOut := process.IsDeadlineElapsed(outcome.capture)
		text, diagnosticTruncated, success := commandOutput(outcome.captured, outcome.capture)
		var failure error
		if ctx.Err() != nil {
			success = false
			failure = errors.New("Operation cancelled")
		} else if marked, err := a.baselineCancelled(check.ID); err != nil {
			return model.BaselineStatusRunning, err
		} else if marked {
			success = false
			failure = errors.New("Operation cancelled")
		} else {
			switch intact, intactErr := outcome.intactResult(); {
			case intactErr != nil:
				success = false
				text += "\n" + intactErr.Error()
				failure = fmt.Errorf("Workspace state check failed during verification: %w", intactErr)
			case !intact:
				success = false
				text += "\nWorkspace or HEAD changed during this verification command"
				failure = errors.New("Workspace or HEAD changed during verification")
			}
		}
		// Scrub secrets without the persistence cap so a long command's output
		// reaches boundedOutput at its true length: truncating here first would
		// silently drop the tail while marking it complete.
		limit := remaining
		if limit > baselineCommandOutputLimit {
			limit = baselineCommandOutputLimit
		}
		output, outputTruncated := boundedOutput(store.RedactSecrets(text), limit, diagnosticTruncated)
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
