package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
)

type drainingDiagnosticAdapter struct {
	runner.Adapter
	ctx            context.Context
	requestStarted chan struct{}
	cleanupStarted chan struct{}
	releaseCleanup chan struct{}
}

func (a drainingDiagnosticAdapter) Models(string) ([]runner.Model, error) {
	close(a.requestStarted)
	<-a.ctx.Done()
	return nil, a.ctx.Err()
}

func (a drainingDiagnosticAdapter) Close() error {
	close(a.cleanupStarted)
	<-a.releaseCleanup
	return a.Adapter.Close()
}

func TestShutdownWaitsForOperatorDiagnosticCleanup(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"doctor", "model catalog"} {
		for _, phase := range []string{"connection", "model listing"} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
				fixture := newScriptedFixture(t, withGitHubIdentity())
				requestStarted := make(chan struct{})
				cleanupStarted := make(chan struct{})
				releaseCleanup := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseCleanup) }) }
				defer release()
				connect := fixture.script.Connector()
				var scratch string
				app := fixture.pausedApp(t, WithRunnerConnector(func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
					client, err := connect(ctx, backend, cfg, cwd)
					if err != nil {
						return nil, err
					}
					scratch = cwd
					if phase == "connection" {
						close(requestStarted)
						<-ctx.Done()
						close(cleanupStarted)
						<-releaseCleanup
						return nil, errors.Join(ctx.Err(), client.Close())
					}
					return drainingDiagnosticAdapter{client, ctx, requestStarted, cleanupStarted, releaseCleanup}, nil
				}))
				requestDone := make(chan error, 1)
				go func() {
					if operation == "doctor" {
						_, _, err := app.DoctorFor(fixture.cfg, model.CycleModeExecution)
						requestDone <- err
					} else {
						_, err := app.ModelCatalog(config.BackendCodex, fixture.cfg.CodexBinary)
						requestDone <- err
					}
				}()
				select {
				case <-requestStarted:
				case <-time.After(5 * time.Second):
					t.Fatalf("diagnostic never reached %s", phase)
				}
				stopped := make(chan struct{})
				go func() { app.Shutdown(); close(stopped) }()
				select {
				case <-cleanupStarted:
				case <-time.After(5 * time.Second):
					t.Fatalf("shutdown did not cancel %s", phase)
				}
				select {
				case <-stopped:
					t.Error("shutdown returned before the diagnostic runner finished cleanup")
				case <-time.After(50 * time.Millisecond):
				}
				if _, err := os.Stat(scratch); err != nil {
					t.Errorf("diagnostic scratch root disappeared before runner cleanup: %v", err)
				}
				release()
				select {
				case err := <-requestDone:
					if err == nil {
						t.Error("cancelled diagnostic reported success")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("diagnostic never returned after cleanup")
				}
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Fatal("shutdown did not finish after diagnostic cleanup")
				}
				assertNoOpenClients(t, fixture.script)
				if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("diagnostic scratch root remains after shutdown: %v", err)
				}
			})
		}
	}
}

func TestOperatorDiagnosticsRefuseAdmissionAfterShutdown(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"doctor", "model catalog"} {
		for _, admission := range []string{"waiting for gate", "store closed"} {
			t.Run(operation+"/"+admission, func(t *testing.T) {
				fixture := newScriptedFixture(t, withGitHubIdentity())
				app := fixture.pausedApp(t)
				if admission == "waiting for gate" {
					app.gate.Lock()
				} else {
					app.Shutdown()
					if err := fixture.state.Close(); err != nil {
						t.Fatal(err)
					}
				}
				started := make(chan struct{})
				done := make(chan error, 1)
				go func() {
					close(started)
					if operation == "doctor" {
						_, _, err := app.DoctorFor(fixture.cfg, model.CycleModeExecution)
						done <- err
					} else {
						_, err := app.ModelCatalog(config.BackendCodex, fixture.cfg.CodexBinary)
						done <- err
					}
				}()
				<-started
				if admission == "waiting for gate" {
					app.cancel()
					app.gate.Unlock()
				}
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("diagnostic started after shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("diagnostic did not refuse shutdown admission")
				}
				if calls := fixture.script.Calls(); len(calls) != 0 {
					t.Errorf("refused diagnostic contacted a runner: %+v", calls)
				}
				if _, err := os.Stat(filepath.Join(app.DataDir, scratchDir)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("refused diagnostic created a scratch root: %v", err)
				}
			})
		}
	}
}
