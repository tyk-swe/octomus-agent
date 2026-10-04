package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const (
	retentionInterval = 15 * time.Minute
	maxRetainDays     = 36500
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

func (a *App) startHousekeeping(cfg config.Config) {
	now := time.Now()
	a.runtimeMu.Lock()
	if a.ctx.Err() != nil || a.runtime.housekeeping {
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
	checks, err := a.Store.StaleBaselines(a.retentionCursor(cleanupBaseline))
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
			err := a.retainCandidateLocked(kind, id, cutoff)
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

func (a *App) retainCandidateLocked(kind cleanupKind, id, cutoff string) error {
	eligible, err := a.Store.CleanupEligible(string(kind), id, cutoff)
	if err != nil || !eligible {
		return err
	}
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

var errStorageIncomplete = errors.New("Storage measurement is incomplete")

// measuredBytes returns only complete observations. An unknown subtree must not turn a partial byte count into a
// fresh measured total; application snapshots retain their previous timestamp and runner observations report error.
func measuredBytes(path string) (uint64, error) {
	usage, err := workspace.Measure(path, 0)
	if err != nil {
		return 0, err
	}
	if len(usage.Unmeasured) != 0 {
		return 0, fmt.Errorf("%w: %s contains unreadable, changed, or traversal-limited entries", errStorageIncomplete, path)
	}
	return usage.Bytes, nil
}

func (a *App) measureStorage(cfg config.Config) error {
	application, err := measuredBytes(a.dataDir)
	if err != nil {
		return err
	}
	tasks, err := measuredBytes(filepath.Join(a.dataDir, "tasks"))
	if err != nil {
		return err
	}
	planning, err := measuredBytes(filepath.Join(a.dataDir, "cycles"))
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
