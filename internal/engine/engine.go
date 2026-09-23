// Package engine owns durable scheduling and planning. Long-running Git,
// GitHub, and runner calls never hold gate; every result is revalidated against
// the live control/configuration before a durable transition.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

const schedulerInterval = time.Second

// TaskRunner owns a durably admitted task after it becomes executing.
type TaskRunner interface {
	RunTask(context.Context, model.Task) error
}

type TaskRunnerFunc func(context.Context, model.Task) error

func (f TaskRunnerFunc) RunTask(ctx context.Context, task model.Task) error { return f(ctx, task) }

type Option func(*App)

func WithTaskRunner(runner TaskRunner) Option { return func(a *App) { a.taskRunner = runner } }

// WithRunnerConnector makes every runner client the engine builds (task
// execution, planning roles, doctor/preflight, the settings preflight and
// model catalog) connect through connect instead of runner.Connect. Route
// validation and runner-unavailable classification still run for real; tests
// use it to supply a scripted adapter (package runnertest). Nil keeps the
// production connector.
func WithRunnerConnector(connect runner.Connector) Option {
	return func(a *App) { a.connector = connect }
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
	preflight              bool
	preflightMode          model.CycleMode
	tasks                  map[string]taskJob
	checkedCycles          map[string]struct{}
	prRefresh              *prRefreshJob
	prObservation          *freshPrObservation
	prRefreshError         string
	lastPrAttempt          time.Time
	housekeeping           bool
	lastRetention          time.Time
	lastObserve            time.Time
	reconcilingPublication bool
	baseline               *baselineJob
	defaultObservation     *model.DefaultBranchObservation
}

func (r *runtimeState) idle() bool {
	return r.cycle == nil && !r.preflight && len(r.tasks) == 0 && r.baseline == nil
}

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
	wg         sync.WaitGroup
}

// runners owns the runner clients of one invocation scope (a task, a planning
// role, a preflight) through the app's connector.
func (a *App) runners(ctx context.Context, cfg config.Config, entity string) *runner.Runners {
	return runner.New(ctx, cfg, a.Store, entity, a.connector)
}

// connectRunner builds one backend's client outside a Runners scope through
// the app's connector.
func (a *App) connectRunner(ctx context.Context, backend config.Backend, cfg config.Config, cwd, entity string) (runner.Adapter, error) {
	if a.connector != nil {
		return a.connector(ctx, backend, cfg, cwd)
	}
	return runner.Connect(ctx, backend, cfg, cwd, a.Store, entity)
}

func New(state *store.Store, dataDir string, options ...Option) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		Store:   state,
		DataDir: filepath.Clean(dataDir),
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		runtime: runtimeState{tasks: map[string]taskJob{}, checkedCycles: map[string]struct{}{}},
	}
	// The production runner is the supervised execution lifecycle; tests
	// substitute it with WithTaskRunner.
	a.taskRunner = TaskRunnerFunc(a.superviseTask)
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

// Config returns the saved configuration or shipped defaults. Downstream
// validation reports unsuitable fields rather than a missing record.
func (a *App) Config() (config.Config, error) {
	cfg, err := store.Get[config.Config](a.Store, "settings", "config")
	if err != nil {
		return config.Config{}, err
	}
	if cfg == nil {
		return config.Default(), nil
	}
	return *cfg, nil
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

// Run recovers durable state once, then schedules until ctx or Shutdown stops it.
func (a *App) Run(ctx context.Context) error {
	if err := a.Recover(); err != nil {
		return err
	}
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()
	for {
		if err := a.Tick(); err != nil {
			a.fail(err)
		}
		select {
		case <-ctx.Done():
			a.Shutdown()
			return nil
		case <-a.ctx.Done():
			a.wg.Wait()
			return nil
		case <-ticker.C:
		case <-a.wake:
		}
	}
}

func (a *App) Shutdown() {
	a.gate.Lock()
	a.cancel()
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

// Context is the app shutdown scope: cancelled by Shutdown so workers owned by
// the service (notifications) stop with everything else.
func (a *App) Context() context.Context { return a.ctx }

// Drained reports that every shutdown-time handle has finished: tasks, the
// cycle, housekeeping, the PR refresh and any baseline job.
func (a *App) Drained() bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	r := &a.runtime
	return len(r.tasks) == 0 && r.cycle == nil && !r.preflight && !r.housekeeping && r.prRefresh == nil && r.baseline == nil
}

func (a *App) fail(err error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	message := store.ErrorMessage(err)
	_ = a.Store.Event("system", "error", message)
	control, loadErr := a.Control()
	if loadErr != nil {
		return
	}
	redacted := store.Redact(message)
	control.Error = &redacted
	control.SetMode(model.OperatingModePaused)
	_ = a.Store.SaveControl(control)
}

// Recover turns interrupted in-memory work into explicit durable state and
// preserves an atomically committed RunOnce execution phase.
func (a *App) Recover() error {
	a.gate.Lock()
	defer a.gate.Unlock()

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
			if workspace.Initialized(task) {
				reason = model.BlockedReasonRetryLimit
			}
			task.BlockedReason = &reason
			task.Error = stringPointer("Service interrupted before workspace initialization completed, or retry budget exhausted. Inspect the preserved task before retrying.")
		}
		task.UpdatedAt = model.Now()
		if err := a.Store.Put("task", task.ID, task); err != nil {
			return err
		}
		if err := a.Store.Event(task.ID, "recovery", *task.Error); err != nil {
			return err
		}
	}

	cycles, err := a.Store.RunningCycles()
	if err != nil {
		return err
	}
	for _, cycle := range cycles {
		model.InterruptRunning(cycle.Sessions)
		cycle.Status = model.CycleInterrupted
		cycle.CompletedAt = stringPointer(model.Now())
		cycle.Error = stringPointer("Discovery interrupted; incomplete proposals were not dispatched")
		if err := a.Store.Put("cycle", cycle.ID, cycle); err != nil {
			return err
		}
	}

	control, err := a.Control()
	if err != nil {
		return err
	}
	if control.Mode == model.OperatingModeRunOnce && control.Batch != nil && control.Batch.Phase == model.BatchPhasePlanning {
		message := "Run once was interrupted before its planning transaction committed"
		control.SetMode(model.OperatingModePaused)
		control.Error = &message
		if err := a.Store.SaveControl(control); err != nil {
			return err
		}
	}
	return nil
}

func stringPointer(value string) *string { return &value }

func (a *App) setTaskError(task *model.Task, err error) error {
	reason := model.BlockedReasonFromError(err)
	task.Status = model.StatusBlocked
	task.BlockedReason = &reason
	task.Error = stringPointer(store.ErrorMessage(err))
	task.UpdatedAt = model.Now()
	if saveErr := a.Store.Put("task", task.ID, *task); saveErr != nil {
		return saveErr
	}
	return a.Store.Event(task.ID, "status", "Blocked")
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
		if loadErr == nil && current != nil && current.Status.Active() {
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
