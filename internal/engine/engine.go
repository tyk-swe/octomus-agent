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

// TaskRunnerFunc adapts a function to TaskRunner.
type TaskRunnerFunc func(context.Context, model.Task) error

// RunTask calls f.
func (f TaskRunnerFunc) RunTask(ctx context.Context, task model.Task) error { return f(ctx, task) }

// Option adjusts an App as New builds it, before any work can start.
type Option func(*App)

// WithTaskRunner replaces the supervised execution lifecycle that owns each
// admitted task; tests use it to observe or script dispatch.
func WithTaskRunner(runner TaskRunner) Option { return func(a *App) { a.taskRunner = runner } }

// WithRunnerConnector makes every runner client the engine builds (task
// execution, planning roles, doctor/preflight, the settings preflight and
// model catalog) connect through connect instead of runner.DefaultConnector.
// Route validation and runner-unavailable classification still run for real;
// tests use it to supply a scripted adapter (package runnertest). Nil keeps
// the production connector.
func WithRunnerConnector(connect runner.Connector) Option {
	return func(a *App) { a.connector = connect }
}

// WithWorkspaceRemoval replaces the managed-directory removal used by task,
// cycle and baseline cleanup. The production default is
// workspace.RemoveOwnedDir; tests inject a barrier or failure here rather than
// hooking inside the cleanup helpers. Nil keeps the production removal.
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

// runtimeState is the process-local picture of in-flight work, guarded by
// App.runtimeMu. Nothing here is durable: a restart starts from an empty
// runtime and Recover turns interrupted durable records into explicit state.
type runtimeState struct {
	// cycle is the running planning cycle (execution or audit) and its cancel.
	cycle *cycleJob
	// preflight marks a planning launch whose remote and route checks are in
	// flight before a cycle exists; preflightMode says which kind of cycle it
	// will start. beginCycle clears it when it assigns cycle.
	preflight     bool
	preflightMode model.CycleMode
	// tasks holds every in-flight owner of a task's work, keyed by task ID:
	// scheduler workers (runTask) and the publication reconcile owner
	// (reconcileLocked). An entry reserves its branch for dispatch, and its
	// cancel stops that work on operator cancel or Shutdown.
	tasks map[string]taskJob
	// checkedCycles caches the queued cycles whose task sets the scheduler
	// has already validated.
	checkedCycles map[string]struct{}
	// prRefresh is the running PR capacity refresh; prObservation,
	// prRefreshError and lastPrAttempt are its latest result and retry pacing.
	prRefresh      *prRefreshJob
	prObservation  *freshPrObservation
	prRefreshError string
	lastPrAttempt  time.Time
	// housekeeping marks a running retention or observation pass;
	// lastRetention and lastObserve pace them.
	housekeeping  bool
	lastRetention time.Time
	lastObserve   time.Time
	// reconcilingPublication marks a publication reconcile in flight; the
	// scheduler dispatches nothing beside it.
	reconcilingPublication bool
	// baseline is the running baseline check, which excludes all other work.
	baseline *baselineJob
	// defaultObservation is the newest default-branch revision observed for
	// the configured repository.
	defaultObservation *model.DefaultBranchObservation
	// cleanups holds the in-memory exclusive cleanup claims by entity kind and
	// durable ID. Claims are taken under the gate, held across the off-gate
	// removal, and released on every exit; nothing persists them, so a restart
	// never inherits a lockout.
	cleanups map[cleanupKey]struct{}
}

func (r *runtimeState) idle() bool {
	return r.cycle == nil && !r.preflight && len(r.tasks) == 0 && r.baseline == nil
}

// App is the engine: the scheduler, planning and task execution, and the
// operator controls, over one durable store and managed data directory.
//
// Concurrency contract:
//   - gate serialises durable decisions: every eligibility check and the
//     durable write it authorises happen under it. Long-running Git, GitHub,
//     runner and filesystem work runs with it released, and the result is
//     revalidated under the gate before it is written.
//   - runtimeMu guards only runtime (in-memory state). It may be taken on its
//     own, or while the gate is held, but the gate is never taken while
//     runtimeMu is held: the lock order is gate, then runtimeMu.
//   - wg counts service-owned work that Shutdown waits for. Every wg.Add runs
//     under the gate after checking that ctx is live, because Shutdown cancels
//     ctx under the gate before it waits; work registered that way is always
//     either refused or waited for, never started after the wait.
//   - retryTask, reconcileLocked, DiscardTask and DiscardCycle release the
//     gate for remote or filesystem work and re-acquire it (reconcileLocked
//     returns with it released). Their callers revalidate durable state
//     afterwards instead of trusting what they read before.
type App struct {
	Store   *store.Store
	DataDir string
	// gate serialises durable decisions; see the App contract above.
	gate sync.Mutex
	// runtimeMu guards runtime and is always taken after gate, never before.
	runtimeMu  sync.Mutex
	runtime    runtimeState
	ctx        context.Context
	cancel     context.CancelFunc
	wake       chan struct{}
	taskRunner TaskRunner
	connector  runner.Connector
	// removeDir deletes one managed workspace directory (workspace.RemoveOwnedDir
	// in production). Cleanup callers invoke it with the gate released.
	removeDir func(root, path string) error
	// wg counts service-owned work; Add only under gate while ctx is live.
	wg sync.WaitGroup
}

// runners owns the runner clients of one invocation scope (a task, a planning
// role, a preflight) through the app's connector.
func (a *App) runners(ctx context.Context, cfg config.Config, entity string) *runner.Runners {
	return runner.New(ctx, cfg, a.connect(entity))
}

// connectRunner builds one backend's client outside a Runners scope through
// the app's connector.
func (a *App) connectRunner(ctx context.Context, backend config.Backend, cfg config.Config, cwd, entity string) (runner.Adapter, error) {
	return a.connect(entity)(ctx, backend, cfg, cwd)
}

// connect is the connector every runner client the engine builds goes
// through: the injected one, or the production connector recording runner
// events under entity.
func (a *App) connect(entity string) runner.Connector {
	if a.connector != nil {
		return a.connector
	}
	return runner.DefaultConnector(a.Store, entity)
}

// New builds an idle App over state and dataDir. Nothing runs until the
// service owner calls Recover and then Run (or Tick in tests).
func New(state *store.Store, dataDir string, options ...Option) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		Store:   state,
		DataDir: filepath.Clean(dataDir),
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		runtime: runtimeState{tasks: map[string]taskJob{}, checkedCycles: map[string]struct{}{}, cleanups: map[cleanupKey]struct{}{}},
	}
	// The production runner is the supervised execution lifecycle; tests
	// substitute it with WithTaskRunner.
	a.taskRunner = TaskRunnerFunc(a.superviseTask)
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

// Control returns the saved operating control record, or the paused default
// when none is saved.
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

// Run schedules until ctx or Shutdown stops it. The service owner calls Recover
// once before exposing HTTP, so a failed recovery cannot leave a healthy
// listener beside a stopped scheduler.
func (a *App) Run(ctx context.Context) error {
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

// Shutdown cancels the service scope and every in-flight job under the gate,
// so no new work can register, then waits for all registered work to return.
// It is safe to call more than once.
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
			message := "Service interrupted before workspace initialization completed, or retry budget exhausted. Inspect the preserved task before retrying."
			if workspace.Initialized(task) {
				reason = model.BlockedReasonRetryLimit
				if task.OutputCommit != nil {
					// Reviewed, verified output only needs its publication
					// finished; reconciliation needs no attempt or model turn.
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
		// During shutdown an initialized task that is still active is left to
		// restart recovery, exactly as after a crash; anything else still
		// active is blocked here.
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
