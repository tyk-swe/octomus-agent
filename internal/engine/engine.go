package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const schedulerInterval = time.Second

type TaskRunner interface {
	RunTask(context.Context, model.Task) error
}

type TaskRunnerFunc func(context.Context, model.Task) error

func (f TaskRunnerFunc) RunTask(ctx context.Context, task model.Task) error { return f(ctx, task) }

type Option func(*App)

func WithTaskRunner(runner TaskRunner) Option {
	return func(a *App) {
		if runner != nil {
			a.taskRunner = runner
		}
	}
}

func WithRunnerConnector(connect runner.Connector) Option {
	return func(a *App) { a.connector = connect }
}

// WithSandbox selects where untrusted children run. Without it the engine runs them directly on the host.
func WithSandbox(backend sandbox.Backend) Option {
	return func(a *App) {
		if backend != nil {
			a.sandbox = backend
		}
	}
}

func WithWorkspaceRemoval(remove func(root, path string) error) Option {
	return func(a *App) {
		if remove != nil {
			a.removeDir = remove
		}
	}
}

type cycleJob struct {
	id     string
	mode   model.CycleMode
	cancel context.CancelFunc
}

type taskJob struct {
	branch string
	cancel context.CancelFunc
}

type runtimeState struct {
	cycle                  *cycleJob
	preflight              *model.CycleMode
	tasks                  map[string]taskJob
	checkedCycles          map[string]struct{}
	prRefresh              *prRefreshJob
	prObservation          *freshPrObservation
	prRefreshError         string
	lastPrAttempt          time.Time
	prAdmissionRefused     bool
	housekeeping           bool
	lastRetention          time.Time
	lastObserve            time.Time
	reconcilingPublication bool
	baseline               *baselineJob
	defaultObservation     *model.DefaultBranchObservation
	cleanups               map[cleanupKey]struct{}
	cleanupReports         map[cleanupKey]cleanupReport
	retentionCursors       map[cleanupKind]string
	activeRecoveryError    *string
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

type App struct {
	Store      *store.Store
	DataDir    string
	gate       sync.Mutex
	runtimeMu  sync.Mutex
	runtime    runtimeState
	ctx        context.Context
	cancel     context.CancelFunc
	wake       chan struct{}
	taskRunner TaskRunner
	connector  runner.Connector
	sandbox    sandbox.Backend
	deployment Deployment
	removeDir  func(root, path string) error
	wg         sync.WaitGroup

	// Guarded by gate; remember only successfully recorded recovery activity.
	recordedRecoveryActivity recoveryActivity

	// planningStorage excludes admission scans from trusted planning filesystem changes. Hold it only during
	// filesystem work, never across store calls, gate acquisition, runner work, or another acquisition of this lock.
	planningStorage sync.RWMutex
}

func (a *App) withoutGate(fn func()) {
	a.gate.Unlock()
	defer a.gate.Lock()
	fn()
}

func (a *App) runners(ctx context.Context, cfg config.Config, entity string) *runner.Runners {
	return runner.New(ctx, cfg, a.connect(entity))
}

func (a *App) connectRunner(ctx context.Context, backend config.Backend, cfg config.Config, cwd, entity string) (runner.Adapter, error) {
	return a.connect(entity)(ctx, backend, cfg, cwd)
}

func (a *App) connect(entity string) runner.Connector {
	if a.connector != nil {
		return a.connector
	}
	return runner.DefaultConnector(a.Store, entity, a.sandbox)
}

func New(state *store.Store, dataDir string, options ...Option) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		Store:   state,
		DataDir: filepath.Clean(dataDir),
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		runtime: runtimeState{tasks: map[string]taskJob{}, checkedCycles: map[string]struct{}{}, cleanups: map[cleanupKey]struct{}{}, cleanupReports: map[cleanupKey]cleanupReport{}, retentionCursors: map[cleanupKind]string{}},
	}
	a.taskRunner = TaskRunnerFunc(a.superviseTask)
	a.sandbox = sandbox.Host{}
	a.removeDir = workspace.RemoveOwnedDir
	for _, option := range options {
		if option != nil {
			option(a)
		}
	}
	return a
}

func (a *App) notify() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *App) Config() (config.Config, error) {
	cfg, err := store.Get[config.Config](a.Store, "settings", "config")
	if err != nil {
		return config.Config{}, err
	}
	if cfg == nil {
		return a.deployment.pin(config.Default()), nil
	}
	return a.deployment.pin(*cfg), nil
}

func (a *App) Control() (model.Control, error) {
	control, err := store.Get[model.Control](a.Store, "settings", "control")
	if err != nil {
		return model.Control{}, err
	}
	if control == nil {
		return model.DefaultControl(), nil
	}
	return *control, nil
}

func (a *App) Run(ctx context.Context) error {
	// A scheduler check may own the gate while waiting for the service context.
	stop := context.AfterFunc(ctx, a.cancel)
	defer stop()
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			a.Shutdown()
			return nil
		}
		if err := a.Tick(); err != nil && ctx.Err() == nil {
			a.fail(err)
		}
		select {
		case <-ctx.Done():
			a.Shutdown()
			return nil
		case <-a.ctx.Done():
			a.Shutdown()
			return nil
		case <-ticker.C:
		case <-a.wake:
		}
	}
}

func (a *App) Shutdown() {
	// Cancel checks holding the gate before waiting for the admission barrier.
	a.cancel()
	a.gate.Lock()
	a.runtimeMu.Lock()
	if a.runtime.cycle != nil {
		a.runtime.cycle.cancel()
	}
	for _, task := range a.runtime.tasks {
		task.cancel()
	}
	if a.runtime.prRefresh != nil {
		a.runtime.prRefresh.cancel()
	}
	if a.runtime.baseline != nil {
		a.runtime.baseline.cancel()
	}
	a.runtimeMu.Unlock()
	a.gate.Unlock()
	a.wg.Wait()
}

func (a *App) Context() context.Context { return a.ctx }

func (a *App) Drained() bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	r := &a.runtime
	return len(r.tasks) == 0 && !r.planning() && !r.housekeeping && r.prRefresh == nil && r.baseline == nil
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

func (a *App) fail(err error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	if a.ctx.Err() != nil {
		return
	}
	message := redact.Error(err)
	var recovery *recoveryError
	if errors.As(err, &recovery) {
		// The redactor may return a substring; retain only its bounded display text.
		message = strings.Clone(message)
		a.runtimeMu.Lock()
		a.runtime.activeRecoveryError = &message
		a.runtimeMu.Unlock()
		activity := &a.recordedRecoveryActivity
		if activity.overflowRecorded {
			return
		}
		for _, recorded := range activity.causes[:activity.count] {
			if recorded == message {
				return
			}
		}
		if activity.count == len(activity.causes) {
			if err := a.Store.Event("system", "recovery_error", "Additional recovery causes are suppressed until recovery succeeds; the latest cause remains available in service health."); err == nil {
				activity.overflowRecorded = true
			}
			return
		}
		if err := a.Store.Event("system", "recovery_error", message); err == nil {
			activity.causes[activity.count] = message
			activity.count++
		}
		return
	}
	if control, loadErr := a.Control(); loadErr == nil {
		redacted := redact.Text(message)
		_ = a.pauseLocked(&control, &redacted)
	}
	_ = a.Store.Event("system", "error", message)
}

func (a *App) Recover() error {
	a.gate.Lock()
	defer a.gate.Unlock()

	// Scratch roots only ever hold a check that died with the previous process.
	if err := workspace.RemoveOwnedDir(a.DataDir, filepath.Join(a.DataDir, scratchDir)); err != nil {
		return err
	}
	if err := a.recoverBaselines(); err != nil {
		return err
	}
	candidates, err := a.Store.PrReservationCandidates()
	if err != nil {
		return err
	}
	for _, task := range candidates {
		initializedQueued := task.Status == model.StatusQueued && workspace.Initialized(task)
		needsReservation := task.Status.Active() || initializedQueued || (task.Status != model.StatusPublished && task.OutputCommit != nil)
		if task.Proposal.Target == task.Config.DefaultBranch && task.Status != model.StatusCancelled && needsReservation {
			if err := a.Store.SeedPrReservation(task); err != nil {
				return err
			}
		}
	}

	active := make([]string, 0, len(model.ActiveStatuses()))
	for _, status := range model.ActiveStatuses() {
		active = append(active, status.String())
	}
	tasks, err := a.Store.TasksWithStatus(active)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		markedCancelled, err := a.Store.MarkerSet("cancel", task.ID)
		if err != nil {
			return err
		}
		model.InterruptRunning(task.Sessions)
		switch {
		case markedCancelled && task.OutputCommit == nil:
			task.Status = model.StatusCancelled
			task.Error = stringPointer("Operator cancellation preserved across restart")
		case workspace.Initialized(task) && task.Attempts < task.ExecutionConfig().MaxRetries:
			task.Status = model.StatusQueued
			task.Attempts++
			task.Error = stringPointer("Recovering an interrupted task: inspecting the recorded workspace and reconciling remote state before continuing.")
		default:
			task.Status = model.StatusBlocked
			reason := model.BlockedReasonWorkspaceInvalid
			message := "Service interrupted before workspace initialization completed, or retry budget exhausted. Inspect the preserved task before retrying."
			if workspace.Initialized(task) {
				reason = model.BlockedReasonRetryLimit
				if task.OutputCommit != nil {
					reason = model.BlockedReasonPublicationUncertain
					message = "Service interrupted during publication after the retry budget was exhausted. Reconcile publication to finish delivery without another model turn."
				}
			}
			task.BlockedReason = &reason
			task.Error = stringPointer(message)
		}
		task.UpdatedAt = model.Now()
		if err := a.Store.Put("task", task.ID, task); err != nil {
			return err
		}
		if err := a.Store.Event(task.ID, "recovery", *task.Error); err != nil {
			return err
		}
	}

	return a.interruptOrphanedCycles()
}

func stringPointer(value string) *string { return &value }

func (a *App) setTaskError(task *model.Task, err error) error {
	recordTaskError(task, err)
	return a.transition(task, model.StatusBlocked)
}

func (a *App) runTask(task model.Task) {
	ctx, cancel := context.WithCancel(a.ctx)
	a.runtimeMu.Lock()
	a.runtime.tasks[task.ID] = taskJob{branch: task.Branch, cancel: cancel}
	a.runtimeMu.Unlock()
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer cancel()
		var runErr error
		func() {
			defer func() {
				if panicked := recover(); panicked != nil {
					runErr = fmt.Errorf("Task worker panicked: %v", panicked)
				}
			}()
			runErr = a.taskRunner.RunTask(ctx, task.Clone())
		}()
		a.gate.Lock()
		current, loadErr := store.Get[model.Task](a.Store, "task", task.ID)
		interrupted := a.ctx.Err() != nil && current != nil && workspace.Initialized(*current)
		if loadErr == nil && current != nil && current.Status.Active() && !interrupted {
			if runErr == nil || errors.Is(runErr, context.Canceled) {
				runErr = errors.New("Task worker exited unexpectedly; inspect the preserved workspace")
			}
			_ = a.setTaskError(current, runErr)
		}
		a.runtimeMu.Lock()
		delete(a.runtime.tasks, task.ID)
		a.runtimeMu.Unlock()
		a.gate.Unlock()
		a.notify()
	}()
}

func invalidPlan(message string) error {
	return fmt.Errorf("%s: %w", message, model.BlockedReasonInvalidPlan)
}
