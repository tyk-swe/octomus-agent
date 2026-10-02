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
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// This backend only returns in-memory children. It never starts a process or runs
// a containment check; its channels model child startup and cleanup.
type selfTestBackend struct {
	sandbox.Backend
	start func(context.Context) (sandbox.Child, error)
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
