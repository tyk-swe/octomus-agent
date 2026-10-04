package engine

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const schedulerInterval = time.Second

type Option func(*App)

// WithSandbox selects where untrusted children run. Without it the engine runs them directly on the host.
func WithSandbox(backend sandbox.Backend) Option {
	return func(a *App) {
		if backend != nil {
			a.sandbox = backend
		}
	}
}

// WithDeployment applies the host deployment's fixed settings.
func WithDeployment(deployment Deployment) Option {
	return func(a *App) { a.deployment = deployment }
}

func WithRunnerConnector(connect runner.Connector) Option {
	return func(a *App) { a.connector = connect }
}

type App struct {
	Store      *store.Store
	dataDir    string
	gate       sync.Mutex
	runtimeMu  sync.Mutex
	runtime    runtimeState
	ctx        context.Context
	cancel     context.CancelFunc
	wake       chan struct{}
	supervise  func(context.Context, model.Task) error
	connector  runner.Connector
	sandbox    sandbox.Backend
	deployment Deployment
	wg         sync.WaitGroup

	// Guarded by gate; remember only successfully recorded recovery activity.
	recoveryLog recoveryActivity

	// fsLock excludes admission scans from trusted planning filesystem changes. Hold it only during
	// filesystem work, never across store calls, gate acquisition, runner work, or another acquisition of this lock.
	fsLock sync.RWMutex
}

func New(state *store.Store, dataDir string, options ...Option) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{
		Store:   state,
		dataDir: filepath.Clean(dataDir),
		ctx:     ctx,
		cancel:  cancel,
		wake:    make(chan struct{}, 1),
		runtime: runtimeState{tasks: map[string]taskJob{}, checkedCycles: map[string]struct{}{}, cleanups: map[cleanupKey]struct{}{}, cleanupReports: map[cleanupKey]cleanupReport{}, retentionCursors: map[cleanupKind]string{}},
	}
	a.supervise = a.superviseTask
	a.sandbox = sandbox.Host{}
	for _, option := range options {
		if option != nil {
			option(a)
		}
	}
	return a
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
		if err := a.tick(); err != nil && ctx.Err() == nil {
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

func (a *App) withoutGate(fn func()) {
	a.gate.Unlock()
	defer a.gate.Lock()
	fn()
}

func (a *App) runners(ctx context.Context, cfg config.Config, entity string) *runner.Runners {
	return runner.New(ctx, cfg, a.connect(entity))
}

func (a *App) connect(entity string) runner.Connector {
	if a.connector != nil {
		return a.connector
	}
	return runner.DefaultConnector(a.Store, entity, a.sandbox)
}

func (a *App) fail(err error) {
	a.gate.Lock()
	defer a.gate.Unlock()
	if a.ctx.Err() != nil {
		return
	}
	message := redact.Error(err)
	var recovery *recoveryError
	if errors.As(err, &recovery) {
		message = a.setRecoveryError(err)
		activity := &a.recoveryLog
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

func (a *App) Drained() bool {
	a.runtimeMu.Lock()
	defer a.runtimeMu.Unlock()
	r := &a.runtime
	return len(r.tasks) == 0 && !r.planning() && !r.housekeeping && r.prRefresh == nil && r.baseline == nil
}

func (a *App) Context() context.Context { return a.ctx }
