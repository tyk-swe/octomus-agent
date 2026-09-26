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
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const (
	// retentionInterval and observeInterval pace the housekeeping passes.
	retentionInterval = 15 * time.Minute
	observeInterval   = 5 * time.Minute
	// observationLifetime is how long a remote observation (open-PR inventory
	// or default-branch revision) stays fresh for the dashboard. Each is
	// stamped only after its pass's earlier steps (retention, the storage walk,
	// the PR refresh), so a lifetime equal to observeInterval let a healthy
	// service flip to stale while the next, slower pass was still running.
	// Dispatch authority is separate and shorter (prAdmissionLifetime).
	observationLifetime = 2 * observeInterval
)

type storageUsage struct {
	MeasuredAt        string         `json:"measured_at"`
	ApplicationBytes  uint64         `json:"application_bytes"`
	TaskBytes         uint64         `json:"task_bytes"`
	PlanningBytes     uint64         `json:"planning_bytes"`
	RunnerTranscripts map[string]any `json:"runner_transcripts"`
}

// maybeStartHousekeeping may be called while gate is held. It only starts an
// owned background job; all filesystem and remote work happens after return.
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
		// Once the service is stopping, the remaining steps are obsolete: the
		// pass ends at the next step boundary, and an interrupted step's
		// cancellation is not a housekeeping failure.
		report := func(err error) {
			if err != nil && a.ctx.Err() == nil {
				_ = a.Store.Event("system", "housekeeping_error", store.ErrorMessage(err))
			}
		}
		if cleanup {
			if err := a.retention(cfg); err != nil {
				report(err)
				return
			}
			if a.ctx.Err() != nil {
				return
			}
			if err := a.measureStorage(cfg); err != nil {
				report(err)
				return
			}
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
	if days > 36500 {
		days = 36500
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	checks, err := a.Store.BaselineCleanupCandidates()
	if err != nil {
		return err
	}
	for _, check := range checks {
		if a.ctx.Err() != nil {
			return nil
		}
		// The gate covers only the eligibility re-read; CleanupBaseline claims
		// the check and removes its clone without the scheduler gate.
		a.gate.Lock()
		current, loadErr := store.Get[model.BaselineCheck](a.Store, "baseline", check.ID)
		terminal := current != nil && current.Status != model.BaselineStatusRunning
		a.runtimeMu.Lock()
		active := a.runtime.baseline != nil && a.runtime.baseline.id == check.ID
		a.runtimeMu.Unlock()
		a.gate.Unlock()
		if loadErr == nil && terminal && !active && current != nil {
			loadErr = a.CleanupBaseline(current)
		}
		if loadErr != nil {
			if eventErr := a.Store.Event(check.ID, "cleanup_error", store.ErrorMessage(loadErr)); eventErr != nil {
				return eventErr
			}
		}
	}
	for _, kind := range []string{"task", "cycle"} {
		ids, err := a.Store.CleanupCandidates(kind, cutoff)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if a.ctx.Err() != nil {
				return nil
			}
			// The candidate list was read without the gate: the re-read under
			// it skips a record the operator discarded meanwhile, so its
			// discarded_at is never rewritten.
			a.gate.Lock()
			if kind == "task" {
				task, loadErr := store.Get[model.Task](a.Store, kind, id)
				if loadErr == nil && task != nil && task.Lifecycle.DiscardedAt == nil {
					a.runtimeMu.Lock()
					_, running := a.runtime.tasks[id]
					a.runtimeMu.Unlock()
					if !task.Status.Active() && !running {
						loadErr = a.DiscardTask(task)
					}
				}
				err = loadErr
			} else {
				cycle, loadErr := store.Get[model.Cycle](a.Store, kind, id)
				if loadErr == nil && cycle != nil && cycle.Lifecycle.DiscardedAt == nil && cycle.Status != model.CycleRunning {
					loadErr = a.DiscardCycle(cycle)
				}
				err = loadErr
			}
			a.gate.Unlock()
			// A conflict means another cleanup already owns this target —
			// success in progress, not a cleanup failure to report.
			if err != nil && !IsActionConflict(err) {
				if eventErr := a.Store.Event(id, "cleanup_error", store.ErrorMessage(err)); eventErr != nil {
					return eventErr
				}
			}
		}
	}
	return nil
}

// cleanupKind names the durable entity kind a cleanup claim owns. Each kind
// maps to one managed root, so the kind is part of the ownership unit.
type cleanupKind string

const (
	cleanupTask     cleanupKind = "task"
	cleanupCycle    cleanupKind = "cycle"
	cleanupBaseline cleanupKind = "baseline"
)

// cleanupKey identifies one managed-directory cleanup claim by durable entity
// kind and ID — each kind owns a distinct root, so the pair is the honest
// ownership unit (a path alone cannot distinguish a task from a cycle).
type cleanupKey struct {
	kind cleanupKind
	id   string
}

// claimCleanup takes exclusive cleanup ownership of (kind, id) and reports
// whether it was free. Callers claim while the scheduler gate is held so
// eligibility and ownership are one atomic admission — except CleanupBaseline,
// which claims first because its callers already serialized eligibility:
// retention re-reads the record under the gate and the finished check's worker
// owns its record. The map itself sits under runtimeMu.
func (a *App) claimCleanup(kind cleanupKind, id string) bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	if a.runtime.cleanups == nil {
		a.runtime.cleanups = map[cleanupKey]struct{}{}
	}
	key := cleanupKey{kind: kind, id: id}
	if _, owned := a.runtime.cleanups[key]; owned {
		return false
	}
	a.runtime.cleanups[key] = struct{}{}
	return true
}

// cleanupClaimed reports whether (kind, id) is owned by an in-flight cleanup.
// Callers hold the scheduler gate; a claimed kind may be working gate-free
// under the baseline entry point (see claimCleanup).
func (a *App) cleanupClaimed(kind cleanupKind, id string) bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	_, owned := a.runtime.cleanups[cleanupKey{kind: kind, id: id}]
	return owned
}

// releaseCleanup drops the claim. Releasing under the gate hold that wrote
// the final durable state leaves no gap between "removal finished" and
// "record marked" that a conflicting action could slip through.
func (a *App) releaseCleanup(kind cleanupKind, id string) {
	a.runtimeMu.Lock()
	delete(a.runtime.cleanups, cleanupKey{kind: kind, id: id})
	a.runtimeMu.Unlock()
}

// DiscardTask removes only the task's owned direct-child directory and marks
// the durable record after successful removal. Callers hold a.gate and get it
// back held: eligibility and the cleanup claim are checked under the gate,
// the recursive deletion runs with it released so unrelated controls stay
// responsive, and finalization re-reads the durable record so only the
// cleanup-owned field changes.
func (a *App) DiscardTask(task *model.Task) error {
	if task.Status.Active() || task.Status == model.StatusQueued {
		return errors.New("Active or queued workspaces cannot be discarded")
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
	// Released on every exit, including removal failure and finalization
	// error, so a claim can never strand the record.
	defer a.releaseCleanup(cleanupTask, task.ID)
	if owner != "" {
		a.gate.Unlock()
		removeErr := a.removeDir(filepath.Join(a.DataDir, "tasks"), owner)
		a.gate.Lock()
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
	// A record that resumed work while the gate was released is not marked;
	// the claim normally prevents this, so treat it as an operator conflict.
	if current.Status.Active() || current.Status == model.StatusQueued {
		return conflictError("Task resumed work during workspace cleanup; inspect it before discarding")
	}
	now := model.Now()
	current.Lifecycle.DiscardedAt = &now
	if err := a.Store.Put("task", current.ID, *current); err != nil {
		return err
	}
	task.Lifecycle.DiscardedAt = current.Lifecycle.DiscardedAt
	return nil
}

// DiscardCycle removes a UUID-named planning directory and records disposal,
// under the same gate contract as DiscardTask: callers hold a.gate and the
// filesystem removal runs with it released.
func (a *App) DiscardCycle(cycle *model.Cycle) error {
	if cycle.Status == model.CycleRunning {
		return errors.New("Running planning work cannot be discarded")
	}
	if _, err := uuid.Parse(cycle.ID); err != nil {
		return errors.New("Invalid cycle workspace identity")
	}
	if !a.claimCleanup(cleanupCycle, cycle.ID) {
		return conflictError("Workspace cleanup is already in progress for this cycle")
	}
	defer a.releaseCleanup(cleanupCycle, cycle.ID)
	root := filepath.Join(a.DataDir, "cycles")
	a.gate.Unlock()
	removeErr := a.removeDir(root, filepath.Join(root, cycle.ID))
	a.gate.Lock()
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
	return nil
}

func (a *App) measureStorage(cfg config.Config) error {
	application, err := workspace.DirectorySize(a.DataDir)
	if err != nil {
		return err
	}
	tasks, err := workspace.DirectorySize(filepath.Join(a.DataDir, "tasks"))
	if err != nil {
		return err
	}
	planning, err := workspace.DirectorySize(filepath.Join(a.DataDir, "cycles"))
	if err != nil {
		return err
	}
	runners := map[string]any{}
	var total uint64
	measured := 0
	for _, backend := range []string{"codex", "opencode"} {
		path, configured := cfg.RunnerStoragePaths[backend]
		entry := map[string]any{"bytes": nil, "status": "unconfigured"}
		if configured {
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				entry["status"] = "unavailable"
			} else {
				bytes, sizeErr := workspace.DirectorySize(path)
				if sizeErr != nil {
					entry["status"] = "error"
				} else {
					entry["bytes"] = bytes
					entry["status"] = "measured"
					total += bytes
					measured++
				}
			}
		}
		runners[backend] = entry
	}
	status := "partial"
	var runnerBytes any = total
	if measured == 0 {
		status, runnerBytes = "unavailable", nil
	} else if measured == 2 {
		status = "measured"
	}
	usage := storageUsage{MeasuredAt: model.Now(), ApplicationBytes: application, TaskBytes: tasks, PlanningBytes: planning, RunnerTranscripts: map[string]any{"bytes": runnerBytes, "status": status, "runners": runners, "message": "Runner storage reported separately. Application admission measures the data directory."}}
	return a.Store.Put("settings", "storage", usage)
}

func (a *App) observeRemote(ctx context.Context, cfg config.Config) error {
	if err := gitops.ValidateRemote(ctx, cfg); err != nil {
		return err
	}
	if err := a.refreshPRs(ctx, cfg); err != nil {
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
			if json.Unmarshal(raw, &summary) != nil || !sameRepository(summary.Repository, cfg.GitHubRepo) {
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
	fingerprint := ContextFingerprint(revisionValue, inventory.PRs)
	if revisionValue != "" {
		if err := a.observeDefaultBranch(cfg, revisionValue, observedAt); err != nil {
			return err
		}
	}
	a.gate.Lock()
	defer a.gate.Unlock()
	live, err := a.Config()
	if err != nil {
		return err
	}
	if !live.SameRemoteIdentity(cfg) {
		return nil
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
	if control.ContextFingerprint != "" && control.ContextFingerprint != fingerprint {
		if control.IdleStreak > 1 {
			ordinary := time.Now().Unix() + int64(cfg.CycleIntervalSeconds)
			if control.NextCycleAt > ordinary {
				control.NextCycleAt = ordinary
			}
		}
		control.IdleStreak = 0
	}
	control.ContextFingerprint = fingerprint
	return a.Store.SaveControl(control)
}

func ContextFingerprint(revision string, prs []model.PullRequest) string {
	parts := make([]string, 0, len(prs))
	for _, pr := range prs {
		parts = append(parts, fmt.Sprintf("%d:%s:%s:%s", pr.Number, pr.Head, pr.Base, pr.State))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(revision+"\n"+strings.Join(parts, "\n"))))
}
