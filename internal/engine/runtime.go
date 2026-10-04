package engine

import (
	"context"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
)

// App lock protocol:
//   - gate: operator and scheduler admission. Hold it across a tick or control
//     action; never across a runner turn (releaseGate / withoutGate).
//   - runtimeMu: in-memory jobs only. Copy what you need, then unlock before any
//     store I/O, filesystem work, or acquiring gate.
//   - fsLock: planning clones versus the storage walk. Never hold it across a
//     store call, gate, or runner turn.
// Acquire gate before runtimeMu when both are needed. Recovery barrier helpers
// in this file must run before tick starts new work.

type cycleJob struct {
	id     string
	mode   model.CycleMode
	cancel context.CancelFunc
}

type taskJob struct {
	branch string
	cancel context.CancelFunc
}

type baselineJob struct {
	id     string
	cancel context.CancelFunc
}

type prRefreshJob struct {
	cancel context.CancelFunc
}

type runtimeState struct {
	cycle               *cycleJob
	preflight           *model.CycleMode
	tasks               map[string]taskJob
	checkedCycles       map[string]struct{}
	prRefresh           *prRefreshJob
	prObservation       *freshPRs
	prRefreshError      string
	lastPRAttempt       time.Time
	prAdmissionRefused  bool
	housekeeping        bool
	lastRetention       time.Time
	lastObserve         time.Time
	reconciling         bool
	baseline            *baselineJob
	defaultObservation  *model.DefaultBranchObservation
	cleanups            map[cleanupKey]struct{}
	cleanupReports      map[cleanupKey]cleanupReport
	retentionCursors    map[cleanupKind]string
	activeRecoveryError *string

	cancelScanDone bool
}

func (r *runtimeState) idle() bool {
	return !r.planning() && len(r.tasks) == 0 && r.baseline == nil
}

func (r *runtimeState) planning() bool { return r.cycle != nil || r.preflight != nil }

func (r *runtimeState) auditActive() bool {
	return r.cycle != nil && r.cycle.mode == model.CycleModeAudit ||
		r.preflight != nil && *r.preflight == model.CycleModeAudit
}

func (r *runtimeState) startPreflight(mode model.CycleMode) { r.preflight = &mode }

func (a *App) endPreflight() {
	a.runtimeMu.Lock()
	a.runtime.preflight = nil
	a.runtimeMu.Unlock()
	a.notify()
}

// Caller holds runtimeMu. The copy must complete before any store call, matching
// the recovery scans that exclude in-flight workers and claimed cleanups.
func (a *App) ownedTaskIDs() []string {
	excluded := make([]string, 0, len(a.runtime.tasks)+len(a.runtime.cleanups))
	for id := range a.runtime.tasks {
		excluded = append(excluded, id)
	}
	for key := range a.runtime.cleanups {
		if key.kind == cleanupTask {
			excluded = append(excluded, key.id)
		}
	}
	return excluded
}

// recoveryError blocks the current scheduling pass without changing saved
// operating policy while a background recovery write is being retried.
type recoveryError struct{ err error }

// Keep the first eight recorded causes for the whole episode, without eviction.
// After saturation, a single overflow notice bounds activity even if causes keep
// changing. Only successful event writes consume a cause slot or the notice.
type recoveryActivity struct {
	causes           [8]string
	count            int
	overflowRecorded bool
}

func (e *recoveryError) Error() string { return e.err.Error() }
func (e *recoveryError) Unwrap() error { return e.err }

// blockRecovery establishes the admission barrier before tick releases the gate.
// Run may report the failure later; operator controls and preflight completions
// must already be blocked during that gap.
func (a *App) blockRecovery(err error) error {
	a.setRecoveryError(err)
	return &recoveryError{err: err}
}

func (a *App) setRecoveryError(err error) string {
	// The redactor may return a substring; retain only its bounded display text.
	message := strings.Clone(redact.Error(err))
	a.runtimeMu.Lock()
	a.runtime.activeRecoveryError = &message
	a.runtimeMu.Unlock()
	return message
}

func (a *App) recoveryConflict() error {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if a.runtime.activeRecoveryError != nil {
		return errRecoveryBlocked
	}
	return nil
}

type cleanupKind string

const (
	cleanupTask     cleanupKind = "task"
	cleanupCycle    cleanupKind = "cycle"
	cleanupBaseline cleanupKind = "baseline"
)

type cleanupKey struct {
	kind cleanupKind
	id   string
}

func (a *App) claimCleanup(kind cleanupKind, id string) bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	key := cleanupKey{kind: kind, id: id}
	if _, owned := a.runtime.cleanups[key]; owned {
		return false
	}
	a.runtime.cleanups[key] = struct{}{}
	return true
}

func (a *App) cleanupClaimed(kind cleanupKind, id string) bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	_, owned := a.runtime.cleanups[cleanupKey{kind: kind, id: id}]
	return owned
}

func (a *App) releaseCleanup(kind cleanupKind, id string) {
	a.runtimeMu.Lock()
	delete(a.runtime.cleanups, cleanupKey{kind: kind, id: id})
	if kind == cleanupTask {
		a.runtime.cancelScanDone = false
	}
	a.runtimeMu.Unlock()
}

const cleanupReportInterval = 24 * time.Hour

type cleanupReport struct {
	message string
	at      time.Time
}

func (a *App) reportCleanupFailure(kind cleanupKind, id string, err error) error {
	message := redact.Error(err)
	key := cleanupKey{kind: kind, id: id}
	now := time.Now()
	a.runtimeMu.Lock()
	last, reported := a.runtime.cleanupReports[key]
	if reported && last.message == message && now.Sub(last.at) < cleanupReportInterval {
		a.runtimeMu.Unlock()
		return nil
	}
	a.runtime.cleanupReports[key] = cleanupReport{message: message, at: now}
	a.runtimeMu.Unlock()
	if eventErr := a.Store.Event(id, "cleanup_error", message); eventErr != nil {
		a.clearCleanupReport(kind, id)
		return eventErr
	}
	return nil
}

func (a *App) clearCleanupReport(kind cleanupKind, id string) {
	a.runtimeMu.Lock()
	delete(a.runtime.cleanupReports, cleanupKey{kind: kind, id: id})
	a.runtimeMu.Unlock()
}
