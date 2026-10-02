package engine

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// This backend only returns in-memory children. It never starts a process or runs
// a containment check; its channels model child startup and cleanup.
type selfTestBackend struct {
	sandbox.Backend
	start func(context.Context) (sandbox.Child, error)
}

// AfterFunc may run later than the parent's cancellation. Hold that documented
// callback boundary so the regression does not depend on goroutine scheduling.
type delayedSelfTestContext struct {
	context.Context
	release <-chan struct{}
}

func (delayedSelfTestContext) Value(any) any { return nil }
func (c delayedSelfTestContext) AfterFunc(f func()) func() bool {
	return context.AfterFunc(c.Context, func() { <-c.release; f() })
}

func TestSelfTestPreservesProofBeforeShutdownCallbackRuns(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	release := make(chan struct{})
	defer close(release)
	var app *App
	backend := selfTestBackend{start: func(ctx context.Context) (sandbox.Child, error) {
		app.cancel()
		if ctx.Err() != nil {
			t.Fatal("fixture did not delay the service cancellation callback")
		}
		return nil, errors.New("synthetic startup failure during shutdown")
	}}
	app = New(state, t.TempDir(), WithSandbox(backend))
	t.Cleanup(app.Shutdown)
	app.ctx = delayedSelfTestContext{app.ctx, release}
	proof := SandboxSelfTest{At: "previous proof", Passed: true, Checks: []sandbox.ProbeCheck{}}
	if err := state.Put("settings", selfTestRecord, proof); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SelfTest(context.Background()); !errors.Is(err, context.Canceled) {
		t.Errorf("shutdown during startup = %v; want cancellation", err)
	}
	saved, err := store.Get[SandboxSelfTest](state, "settings", selfTestRecord)
	if err != nil || !reflect.DeepEqual(saved, &proof) {
		t.Fatalf("delayed cancellation replaced completed proof: %+v, %v", saved, err)
	}
}

type completedSelfTestChild struct{}

func (completedSelfTestChild) Stdin() sandbox.DeadlineWriter { return nil }
func (completedSelfTestChild) Stdout() io.ReadCloser {
	return io.NopCloser(strings.NewReader(`{"checks":[{"id":"resource_limits","passed":true}],"kernel":"fixture","limits":{"memory_max":"67108864","pids_max":"64"}}`))
}
func (completedSelfTestChild) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (completedSelfTestChild) Kill()                 {}
func (completedSelfTestChild) Terminate()            {}
func (completedSelfTestChild) Wait() (process.Status, error) {
	return process.ExitStatus(process.Exit{}), nil
}

type finishingSelfTestBackend struct {
	selfTestBackend
	info func(context.Context) (wire.BrokerInfo, error)
}

func (b finishingSelfTestBackend) Info(ctx context.Context) (wire.BrokerInfo, error) {
	return b.info(ctx)
}

func TestSelfTestPreservesProofWhenFinalInfoIsCancelled(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"caller cancellation", "service cancellation", "unavailable", "complete"} {
		t.Run(outcome, func(t *testing.T) {
			state := testStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var app *App
			backend := finishingSelfTestBackend{
				selfTestBackend: selfTestBackend{start: func(context.Context) (sandbox.Child, error) {
					return completedSelfTestChild{}, nil
				}},
				info: func(ctx context.Context) (wire.BrokerInfo, error) {
					switch outcome {
					case "caller cancellation":
						cancel()
						return wire.BrokerInfo{}, ctx.Err()
					case "service cancellation":
						app.cancel()
						<-ctx.Done()
						return wire.BrokerInfo{}, ctx.Err()
					case "unavailable":
						return wire.BrokerInfo{}, errors.New("synthetic unavailable limits")
					default:
						return wire.BrokerInfo{Limits: wire.BrokerLimits{Memory: 67108864, Pids: 64}}, nil
					}
				},
			}
			app = New(state, t.TempDir(), WithSandbox(backend))
			t.Cleanup(app.Shutdown)
			proof := SandboxSelfTest{At: "previous proof", Passed: true, Checks: []sandbox.ProbeCheck{}}
			if err := state.Put("settings", selfTestRecord, proof); err != nil {
				t.Fatal(err)
			}
			result, err := app.SelfTest(ctx)
			cancelled := strings.HasSuffix(outcome, "cancellation")
			if cancelled && !errors.Is(err, context.Canceled) || !cancelled && err != nil {
				t.Errorf("self-test result error = %v; cancellation=%t", err, cancelled)
			}
			saved, readErr := store.Get[SandboxSelfTest](state, "settings", selfTestRecord)
			if readErr != nil || saved == nil {
				t.Fatalf("saved result: %+v, %v", saved, readErr)
			}
			if cancelled {
				if !reflect.DeepEqual(saved, &proof) {
					t.Fatalf("cancelled final metadata replaced proof: %+v", saved)
				}
			} else if saved.At == proof.At || saved.Passed != (outcome == "complete") || !reflect.DeepEqual(saved, &result) {
				t.Fatalf("uncancelled result was not persisted accurately: %+v / %+v", saved, result)
			}
		})
	}
}

func (selfTestBackend) Mode() sandbox.Mode { return sandbox.ModeDocker }

func (b selfTestBackend) Start(ctx context.Context, _ sandbox.Spec) (sandbox.Child, error) {
	return b.start(ctx)
}

type drainingSelfTestChild struct {
	terminated     chan struct{}
	cleanupStarted chan struct{}
	releaseCleanup chan struct{}
	terminateOnce  sync.Once
}

func (c *drainingSelfTestChild) Stdin() sandbox.DeadlineWriter { return nil }
func (c *drainingSelfTestChild) Stdout() io.ReadCloser         { return io.NopCloser(strings.NewReader("")) }
func (c *drainingSelfTestChild) Stderr() io.ReadCloser         { return io.NopCloser(strings.NewReader("")) }
func (c *drainingSelfTestChild) Kill()                         { c.Terminate() }
func (c *drainingSelfTestChild) Terminate() {
	c.terminateOnce.Do(func() { close(c.terminated) })
}
func (c *drainingSelfTestChild) Wait() (process.Status, error) {
	<-c.terminated
	close(c.cleanupStarted)
	<-c.releaseCleanup
	return process.ExitStatus(process.Exit{Killed: true}), nil
}

func TestShutdownCancelsAndJoinsSelfTest(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"self-test", "doctor"} {
		for _, phase := range []string{"start", "child cleanup"} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
				requestStarted := make(chan struct{})
				cleanupStarted := make(chan struct{})
				releaseCleanup := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseCleanup) }) }
				defer release()
				backend := selfTestBackend{start: func(ctx context.Context) (sandbox.Child, error) {
					close(requestStarted)
					if phase == "start" {
						<-ctx.Done()
						close(cleanupStarted)
						<-releaseCleanup
						return nil, ctx.Err()
					}
					return &drainingSelfTestChild{terminated: make(chan struct{}), cleanupStarted: cleanupStarted, releaseCleanup: releaseCleanup}, nil
				}}
				state := testStore(t)
				app := New(state, t.TempDir(), WithSandbox(backend))
				t.Cleanup(app.Shutdown)
				proof := SandboxSelfTest{At: "previous proof", Passed: true, Checks: []sandbox.ProbeCheck{}}
				if err := state.Put("settings", selfTestRecord, proof); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				requestDone := make(chan error, 1)
				go func() {
					if operation == "doctor" {
						_, _, err := app.DoctorFor(config.Default(), model.CycleModeExecution)
						requestDone <- err
					} else {
						_, err := app.SelfTest(ctx)
						requestDone <- err
					}
				}()
				select {
				case <-requestStarted:
				case <-time.After(5 * time.Second):
					t.Fatal("self-test never started")
				}
				stopped := make(chan struct{})
				go func() { app.Shutdown(); close(stopped) }()
				select {
				case <-cleanupStarted:
				case <-stopped:
					t.Error("shutdown returned without canceling and joining the self-test")
					cancel()
				case <-time.After(5 * time.Second):
					t.Error("shutdown did not cancel the self-test")
					cancel()
				}
				select {
				case <-cleanupStarted:
				case <-time.After(5 * time.Second):
					t.Fatal("self-test did not enter cleanup after cancellation")
				}
				select {
				case <-stopped:
					t.Error("shutdown returned before self-test cleanup finished")
				case <-time.After(50 * time.Millisecond):
				}
				release()
				select {
				case err := <-requestDone:
					if !errors.Is(err, context.Canceled) && !errors.Is(err, process.ErrCancelled) {
						t.Errorf("self-test error = %v; want cancellation", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("self-test did not return after cleanup")
				}
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Fatal("shutdown did not return after self-test cleanup")
				}
				saved, err := store.Get[SandboxSelfTest](state, "settings", selfTestRecord)
				if err != nil || !reflect.DeepEqual(saved, &proof) {
					t.Errorf("saved self-test = %+v, %v; want the last completed proof", saved, err)
				}
			})
		}
	}
}

func TestSelfTestRefusesCanceledAdmission(t *testing.T) {
	t.Parallel()
	for _, admission := range []string{"waiting for gate", "store closed", "caller canceled"} {
		t.Run(admission, func(t *testing.T) {
			var starts atomic.Int32
			backend := selfTestBackend{start: func(context.Context) (sandbox.Child, error) {
				starts.Add(1)
				return nil, errors.New("unexpected child start")
			}}
			state := testStore(t)
			app := New(state, t.TempDir(), WithSandbox(backend))
			t.Cleanup(app.Shutdown)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch admission {
			case "waiting for gate":
				app.gate.Lock()
			case "store closed":
				app.Shutdown()
				if err := state.Close(); err != nil {
					t.Fatal(err)
				}
			case "caller canceled":
				cancel()
			}
			started := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				close(started)
				_, err := app.SelfTest(ctx)
				done <- err
			}()
			<-started
			if admission == "waiting for gate" {
				app.cancel()
				app.gate.Unlock()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("self-test after cancellation = %v; want context.Canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("self-test did not refuse canceled admission")
			}
			if calls := starts.Load(); calls != 0 {
				t.Errorf("canceled admission started %d children", calls)
			}
		})
	}
}
