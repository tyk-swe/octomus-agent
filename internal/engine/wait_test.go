package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	cycleWaitTimeout = 30 * time.Second
	taskWaitTimeout  = 60 * time.Second
)

func fixtureContext(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	// Leave time for fixture cancellation, diagnostics and cleanup before Go's watchdog.
	if end, ok := t.Deadline(); ok && end.Add(-10*time.Second).Before(deadline) {
		deadline = end.Add(-10 * time.Second)
	}
	return context.WithDeadline(t.Context(), deadline)
}

// waitFixture bounds the operation itself, including a scheduler tick or a worker join.
// Its result channel stays writable after cancellation so cooperative work can unwind.
func waitFixture(ctx context.Context, app *App, label string, work func() error) error {
	done := make(chan error, 1)
	go func() { done <- work() }()
	select {
	case err := <-done:
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			app.cancel()
			return fmt.Errorf("%s: %w\n%s", label, ctx.Err(), fixtureDiagnostics(app))
		}
		return err
	case <-ctx.Done():
		app.cancel()
		return fmt.Errorf("%s: %w\n%s", label, ctx.Err(), fixtureDiagnostics(app))
	}
}

func waitApp(t *testing.T, app *App) {
	t.Helper()
	ctx, cancel := fixtureContext(t, cycleWaitTimeout)
	defer cancel()
	if err := waitFixture(ctx, app, "engine workers did not finish", func() error {
		app.wg.Wait()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func cleanupApp(t *testing.T, app *App) {
	t.Helper()
	t.Cleanup(func() { shutdownApp(t, app) })
}

func shutdownApp(t *testing.T, app *App) {
	t.Helper()
	// The test context is already cancelled when cleanup starts.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitFixture(ctx, app, "engine shutdown did not finish", func() error {
		app.Shutdown()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func fixtureDiagnostics(app *App) string {
	state := "saved state: unavailable"
	if app.Store != nil {
		saved := make(chan string, 1)
		go func() {
			tasks, taskErr := store.List[model.Task](app.Store, "task")
			cycles, cycleErr := store.List[model.Cycle](app.Store, "cycle")
			snapshot := map[string]any{"tasks": tasks, "cycles": cycles}
			if taskErr != nil || cycleErr != nil {
				snapshot["read_error"] = fmt.Sprintf("tasks: %v; cycles: %v", taskErr, cycleErr)
			}
			data, err := json.Marshal(snapshot)
			saved <- fmt.Sprintf("saved state: %s (marshal error: %v)", redact.Text(string(data)), err)
		}()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case state = <-saved:
		case <-timer.C:
			state = "saved state: read did not finish after cancellation"
		}
	}
	return state + "\ngoroutines:\n" + fixtureStacks()
}

func fixtureStacks() string {
	buffer := make([]byte, 1<<20)
	n := runtime.Stack(buffer, true)
	return string(buffer[:n])
}

func waitCycle(t *testing.T, app *App, id string) model.Cycle {
	t.Helper()
	return waitForCycle(t, app, id)
}

func waitOnlyCycle(t *testing.T, app *App) model.Cycle {
	t.Helper()
	return waitForCycle(t, app, "")
}

func waitForCycle(t *testing.T, app *App, id string) model.Cycle {
	t.Helper()
	ctx, cancel := fixtureContext(t, cycleWaitTimeout)
	defer cancel()
	var result model.Cycle
	err := waitFixture(ctx, app, "cycle "+id+" did not finish", func() error {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			var cycle *model.Cycle
			if id != "" {
				var err error
				cycle, err = store.Get[model.Cycle](app.Store, "cycle", id)
				if err != nil {
					return err
				}
			} else {
				cycles, err := store.List[model.Cycle](app.Store, "cycle")
				if err != nil {
					return err
				}
				if len(cycles) == 1 {
					cycle = &cycles[0]
				}
			}
			if cycle != nil && cycle.Status != model.CycleRunning {
				result = *cycle
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFixtureWorkerWait(t *testing.T) {
	t.Parallel()
	t.Run("completed", func(t *testing.T) {
		app := New(nil, t.TempDir())
		cleanupApp(t, app)
		waitApp(t, app)
		if app.ctx.Err() != nil {
			t.Fatal("a successful wait cancelled the app")
		}
	})
	for _, test := range []struct {
		name string
		err  error
	}{
		{"cancelled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := New(nil, t.TempDir())
			cleanupApp(t, app)
			stopped := make(chan struct{})
			app.wg.Add(1)
			go func() {
				defer close(stopped)
				defer app.wg.Done()
				<-app.ctx.Done()
			}()
			ctx, cancel := context.WithCancel(t.Context())
			if test.err == context.DeadlineExceeded {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			} else {
				cancel()
			}
			defer cancel()
			err := waitFixture(ctx, app, "fixture worker", func() error {
				app.wg.Wait()
				return nil
			})
			if !errors.Is(err, test.err) || !strings.Contains(err.Error(), "goroutines:") {
				t.Fatalf("worker wait = %v; want %v and diagnostics", err, test.err)
			}
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("timed-out wait did not cancel its worker")
			}
		})
	}
}

func TestDriveTaskResultCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	secret := "ghp_" + strings.Repeat("a", 36)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"printf " + secret} })
	gate := runnertest.NewGate()
	t.Cleanup(gate.Release)
	f.script.Queue(f.routes.Executor, runnertest.Reply{Answer: "Never delivered", Gate: gate})
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)
	app := f.newApp(t)
	ctx, cancel := fixtureContext(t, cycleWaitTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := driveTaskResult(ctx, f, app, task.ID)
		done <- err
	}()
	select {
	case <-gate.Entered():
	case <-ctx.Done():
		t.Fatal("the executor was never reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), task.ID) ||
			!strings.Contains(err.Error(), "saved state:") || !strings.Contains(err.Error(), "goroutines:") {
			t.Fatalf("cancelled driver = %v; want task and cancellation diagnostics", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("fixture diagnostics exposed a secret")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the task driver stayed blocked on workers after cancellation")
	}
}
