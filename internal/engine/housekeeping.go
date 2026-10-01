package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const (
	retentionInterval     = 15 * time.Minute
	observeInterval       = 5 * time.Minute
	observationLifetime   = 2 * observeInterval
	maxRetainDays         = 36500
	cleanupReportInterval = 24 * time.Hour
)

type storageUsage struct {
	MeasuredAt        string            `json:"measured_at"`
	ApplicationBytes  uint64            `json:"application_bytes"`
	TaskBytes         uint64            `json:"task_bytes"`
	PlanningBytes     uint64            `json:"planning_bytes"`
	RunnerTranscripts runnerTranscripts `json:"runner_transcripts"`
}

const runnerStorageMessage = "Runner storage reported separately. Application admission measures the data directory."

type runnerTranscripts struct {
	Bytes   *uint64                  `json:"bytes"`
	Message string                   `json:"message"`
	Runners map[string]runnerStorage `json:"runners"`
	Status  string                   `json:"status"`
}

type runnerStorage struct {
	Bytes  *uint64 `json:"bytes"`
	Status string  `json:"status"`
}

func (a *App) maybeStartHousekeeping(cfg config.Config) {
	now := time.Now()
	a.runtimeMu.Lock()
	if a.runtime.housekeeping {
		a.runtimeMu.Unlock()
		return
	}
	cleanup := a.runtime.lastRetention.IsZero() || now.Sub(a.runtime.lastRetention) >= retentionInterval
	observe := a.runtime.lastObserve.IsZero() || now.Sub(a.runtime.lastObserve) >= observeInterval
	if !cleanup && !observe {
		a.runtimeMu.Unlock()
		return
	}
	a.runtime.housekeeping = true
	if cleanup {
		a.runtime.lastRetention = now
	}
	if observe {
		a.runtime.lastObserve = now
	}
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer func() {
			a.runtimeMu.Lock()
			a.runtime.housekeeping = false
			a.runtimeMu.Unlock()
		}()
		report := func(err error) {
			if err != nil && a.ctx.Err() == nil {
				_ = a.Store.Event("system", "housekeeping_error", redact.Error(err))
			}
		}
		if cleanup {
			report(a.retention(cfg))
			if a.ctx.Err() != nil {
				return
			}
			report(a.measureStorage(cfg))
		}
		if a.ctx.Err() != nil {
			return
		}
		if stat, err := os.Stat(cfg.Repository); observe && cfg.GitHubRepo != "" && err == nil && stat.IsDir() {
			report(a.observeRemote(a.ctx, cfg))
		}
	}()
}

func (a *App) retention(cfg config.Config) error {
	if err := a.Store.PruneEvents(int64(cfg.RetainEvents)); err != nil {
		return err
	}
	days := cfg.RetainCompletedDays
	if days > maxRetainDays {
		days = maxRetainDays
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	checks, err := a.Store.BaselineCleanupCandidates(a.retentionCursor(cleanupBaseline))
	if err != nil {
		return err
	}
	for _, check := range checks {
		if a.ctx.Err() != nil {
			return nil
		}
		a.advanceRetentionCursor(cleanupBaseline, check.ID)
		a.gate.Lock()
		current, loadErr := store.Get[model.BaselineCheck](a.Store, "baseline", check.ID)
		terminal := current != nil && current.Status != model.BaselineStatusRunning
		a.runtimeMu.Lock()
		active := a.runtime.baseline != nil && a.runtime.baseline.id == check.ID
		a.runtimeMu.Unlock()
		a.gate.Unlock()
		if loadErr == nil && terminal && !active {
			loadErr = a.removeBaselineWorkspace(current)
		}
		if loadErr == nil {
			a.clearCleanupReport(cleanupBaseline, check.ID)
		} else if eventErr := a.reportCleanupFailure(cleanupBaseline, check.ID, loadErr); eventErr != nil {
			return eventErr
		}
	}
	for _, kind := range []cleanupKind{cleanupTask, cleanupCycle} {
		ids, err := a.Store.CleanupCandidates(string(kind), cutoff, a.retentionCursor(kind))
		if err != nil {
			return err
		}
		for _, id := range ids {
			if a.ctx.Err() != nil {
				return nil
			}
			a.advanceRetentionCursor(kind, id)
			a.gate.Lock()
			err := a.retainCandidateLocked(kind, id)
			a.gate.Unlock()
			if err == nil {
				a.clearCleanupReport(kind, id)
			} else if !IsActionConflict(err) {
				if eventErr := a.reportCleanupFailure(kind, id, err); eventErr != nil {
					return eventErr
				}
			}
		}
	}
	return nil
}

func (a *App) retentionCursor(kind cleanupKind) string {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	return a.runtime.retentionCursors[kind]
}

func (a *App) advanceRetentionCursor(kind cleanupKind, id string) {
	a.runtimeMu.Lock()
	a.runtime.retentionCursors[kind] = id
	a.runtimeMu.Unlock()
}

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

func (a *App) retainCandidateLocked(kind cleanupKind, id string) error {
	if kind == cleanupTask {
		task, err := store.Get[model.Task](a.Store, "task", id)
		if err != nil || task == nil || task.Lifecycle.DiscardedAt != nil {
			return err
		}
		a.runtimeMu.Lock()
		_, running := a.runtime.tasks[id]
		a.runtimeMu.Unlock()
		if task.Status.Active() || running {
			return nil
		}
		return a.discardTask(task)
	}
	cycle, err := store.Get[model.Cycle](a.Store, "cycle", id)
	if err != nil || cycle == nil || cycle.Lifecycle.DiscardedAt != nil || cycle.Status == model.CycleRunning {
		return err
	}
	return a.discardCycle(cycle)
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
	a.runtimeMu.Unlock()
}

func (a *App) discardTask(task *model.Task) error {
	if task.Status.Active() || task.Status == model.StatusQueued {
		return errors.New("Active or queued workspaces cannot be discarded")
	}
	if task.Lifecycle.DiscardedAt != nil {
		return conflictError("The task workspace was already discarded")
	}
	owner := ""
	if task.Workspace != "" {
		owner = filepath.Dir(filepath.Clean(task.Workspace))
		expected := filepath.Join(a.DataDir, "tasks", task.ID)
		if owner != expected {
			return errors.New("Cleanup path does not belong to this task")
		}
	}
	if !a.claimCleanup(cleanupTask, task.ID) {
		return conflictError("Workspace cleanup is already in progress for this task")
	}
	defer a.releaseCleanup(cleanupTask, task.ID)
	if owner != "" {
		var removeErr error
		a.withoutGate(func() { removeErr = a.removeDir(filepath.Join(a.DataDir, "tasks"), owner) })
		if removeErr != nil {
			return removeErr
		}
	}
	current, err := store.Get[model.Task](a.Store, "task", task.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.Status.Active() || current.Status == model.StatusQueued {
		return conflictError("Task resumed work during workspace cleanup; inspect it before discarding")
	}
	now := model.Now()
	current.Lifecycle.DiscardedAt = &now
	if err := a.Store.Put("task", current.ID, *current); err != nil {
		return err
	}
	task.Lifecycle.DiscardedAt = current.Lifecycle.DiscardedAt
	a.clearCleanupReport(cleanupTask, task.ID)
	return nil
}

func (a *App) discardCycle(cycle *model.Cycle) error {
	if cycle.Status == model.CycleRunning {
		return errors.New("Running planning work cannot be discarded")
	}
	if cycle.Lifecycle.DiscardedAt != nil {
		return conflictError("The cycle workspaces were already discarded")
	}
	if _, err := uuid.Parse(cycle.ID); err != nil {
		return errors.New("Invalid cycle workspace identity")
	}
	if !a.claimCleanup(cleanupCycle, cycle.ID) {
		return conflictError("Workspace cleanup is already in progress for this cycle")
	}
	defer a.releaseCleanup(cleanupCycle, cycle.ID)
	root := filepath.Join(a.DataDir, "cycles")
	var removeErr error
	a.withoutGate(func() { removeErr = a.removeDir(root, filepath.Join(root, cycle.ID)) })
	if removeErr != nil {
		return removeErr
	}
	current, err := store.Get[model.Cycle](a.Store, "cycle", cycle.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.Status == model.CycleRunning {
		return conflictError("Planning work restarted during workspace cleanup; inspect it before discarding")
	}
	now := model.Now()
	current.Lifecycle.DiscardedAt = &now
	if err := a.Store.Put("cycle", current.ID, *current); err != nil {
		return err
	}
	cycle.Lifecycle.DiscardedAt = current.Lifecycle.DiscardedAt
	a.clearCleanupReport(cleanupCycle, cycle.ID)
	return nil
}

// measuredBytes is what the dashboard shows: the bytes a walk could reach. Admission accounts for unmeasured subtrees
// itself (measureFor).
func measuredBytes(path string) (uint64, error) {
	usage, err := workspace.Measure(path, 0)
	return usage.Bytes, err
}

func (a *App) measureStorage(cfg config.Config) error {
	application, err := measuredBytes(a.DataDir)
	if err != nil {
		return err
	}
	tasks, err := measuredBytes(filepath.Join(a.DataDir, "tasks"))
	if err != nil {
		return err
	}
	planning, err := measuredBytes(filepath.Join(a.DataDir, "cycles"))
	if err != nil {
		return err
	}
	backends := []config.Backend{config.BackendCodex, config.BackendOpencode}
	runners := make(map[string]runnerStorage, len(backends))
	var total uint64
	measured := 0
	for _, backend := range backends {
		name := backend.Slug()
		path, configured := cfg.RunnerStoragePaths[name]
		entry := runnerStorage{Status: "unconfigured"}
		if configured {
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				entry.Status = "unavailable"
			} else if bytes, sizeErr := measuredBytes(path); sizeErr != nil {
				entry.Status = "error"
			} else {
				entry.Bytes = &bytes
				entry.Status = "measured"
				total += bytes
				measured++
			}
		}
		runners[name] = entry
	}
	transcripts := runnerTranscripts{Bytes: &total, Message: runnerStorageMessage, Runners: runners, Status: "partial"}
	if measured == 0 {
		transcripts.Bytes, transcripts.Status = nil, "unavailable"
	} else if measured == len(backends) {
		transcripts.Status = "measured"
	}
	usage := storageUsage{MeasuredAt: model.Now(), ApplicationBytes: application, TaskBytes: tasks, PlanningBytes: planning, RunnerTranscripts: transcripts}
	return a.Store.Put("settings", "storage", usage)
}

func (a *App) observeRemote(ctx context.Context, cfg config.Config) error {
	if err := gitops.ValidateRemote(ctx, cfg); err != nil {
		return err
	}
	if err := a.refreshPRs(ctx, cfg); errors.Is(err, errPrPolicyChanged) {
		return nil
	} else if err != nil && !errors.Is(err, errPrInventorySuperseded) {
		return err
	}
	inventory, err := a.Store.OpenPrInventory()
	if err != nil || inventory == nil {
		if err == nil {
			err = errors.New("PR refresh did not persist an inventory")
		}
		return err
	}
	open := map[uint64]struct{}{}
	for _, pr := range inventory.PRs {
		open[pr.Number] = struct{}{}
	}
	before := (*int64)(nil)
	status := "open"
	limit := 100
	closed := []model.PullRequest{}
	for {
		page, err := a.Store.HistoryPage("pr", store.HistoryQuery{Before: before, Status: &status, Limit: &limit})
		if err != nil {
			return err
		}
		for _, raw := range page.Items {
			if ctx.Err() != nil {
				return nil
			}
			var summary struct {
				Repository string `json:"repository"`
				PR         struct {
					Number uint64 `json:"number"`
				} `json:"pr"`
			}
			if json.Unmarshal(raw, &summary) != nil || !config.EqualASCII(summary.Repository, cfg.GitHubRepo) {
				continue
			}
			if _, present := open[summary.PR.Number]; !present {
				pr, err := gitops.PR(ctx, cfg, summary.PR.Number)
				if err != nil {
					return err
				}
				closed = append(closed, pr)
			}
		}
		before = page.NextCursor
		if before == nil {
			break
		}
	}
	observedAt := model.Now()
	revision, err := gitops.RemoteRevision(ctx, cfg, cfg.DefaultBranch)
	if err != nil {
		return err
	}
	revisionValue := ""
	if revision != nil {
		revisionValue = *revision
	}
	fingerprint := contextFingerprint(revisionValue, inventory.PRs)
	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return err
	}
	if !live.SameRemoteIdentity(cfg) {
		return nil
	}
	if revisionValue != "" {
		if err := a.mergeDefaultObservationLocked(cfg, revisionValue, observedAt); err != nil {
			return err
		}
	}
	for _, pr := range closed {
		if err := a.Store.RecordPrObservation(cfg.GitHubRepo, pr, false); err != nil {
			return err
		}
	}
	control, err := a.Control()
	if err != nil {
		return err
	}
	applyContextFingerprint(&control, fingerprint, time.Now(), cfg.CycleIntervalSeconds)
	return a.Store.SaveControl(control)
}

func applyContextFingerprint(control *model.Control, fingerprint string, now time.Time, interval uint64) {
	if control.ContextFingerprint != "" && control.ContextFingerprint != fingerprint {
		if control.IdleStreak > 1 {
			ordinary := now.Unix() + int64(interval)
			if control.NextCycleAt > ordinary {
				control.NextCycleAt = ordinary
			}
		}
		control.IdleStreak = 0
	}
	control.ContextFingerprint = fingerprint
}

func contextFingerprint(revision string, prs []model.PullRequest) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		parts = append(parts, fmt.Sprintf("%d:%s:%s:%s", pr.Number, pr.Head, pr.Base, pr.State))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(revision+"\n"+strings.Join(parts, "\n"))))
}
