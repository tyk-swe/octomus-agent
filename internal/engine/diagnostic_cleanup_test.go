package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

func TestDiagnosticRequestsReportRunnerCleanupFailure(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"doctor", "model catalog"} {
		for _, stage := range []string{"success", "catalog error", "cancelled"} {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				fixture := newScriptedFixture(t, withGitHubIdentity())
				const cleanupMessage = "fixture runner cleanup could not confirm container removal"
				const secret = "synthetic-cleanup-credential"
				cleanupErr := &sandbox.SandboxError{Err: errors.New(cleanupMessage + "; Bearer " + secret)}
				fixture.script.FailClose(config.BackendCodex, cleanupErr)
				connect := fixture.script.Connector()
				catalogErr := errors.New("fixture model catalog failed")
				if stage == "cancelled" {
					catalogErr = context.Canceled
				}
				var app *App
				app = fixture.pausedApp(t, WithRunnerConnector(func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
					client, err := connect(ctx, backend, cfg, cwd)
					if err != nil || stage == "success" {
						return client, err
					}
					var cancel context.CancelFunc
					if stage == "cancelled" {
						cancel = app.cancel
					}
					return diagnosticCatalogFailure{Adapter: client, err: catalogErr, cancel: cancel}, nil
				}))
				var err error
				if operation == "doctor" {
					var result map[string]any
					result, _, err = app.DoctorFor(fixture.cfg, model.CycleModeExecution)
					if result != nil {
						t.Errorf("doctor reported success despite failed runner cleanup: %+v", result)
					}
				} else {
					_, err = app.ModelCatalog(config.BackendCodex, fixture.cfg.CodexBinary)
				}
				if err == nil || !strings.Contains(err.Error(), cleanupMessage) {
					t.Errorf("%s dropped runner cleanup error: %v", operation, err)
				}
				if err != nil && (strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]")) {
					t.Error("runner cleanup diagnostics exposed an unredacted credential")
				}
				if stage != "success" && (err == nil || !strings.Contains(err.Error(), catalogErr.Error())) {
					t.Errorf("%s dropped original catalog error: %v", operation, err)
				}
				if operation == "model catalog" && stage != "success" && !errors.Is(err, catalogErr) {
					t.Errorf("model catalog lost original error identity: %v", err)
				}
				if stage == "cancelled" && app.Context().Err() != context.Canceled {
					t.Fatal("fixture did not cancel the request during model listing")
				}
				closed := 0
				for _, call := range fixture.script.Calls() {
					if call.Kind == runnertest.CallClose {
						closed++
					}
				}
				if closed != 1 {
					t.Errorf("runner closed %d times; want once", closed)
				}
				assertNoOpenClients(t, fixture.script)
			})
		}
	}
}

func TestRoutePreflightReportsValidationAndCleanupFailures(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"audit", "execution"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newScriptedFixture(t, withGitHubIdentity())
			fixture.script.SetCatalog()
			const cleanupMessage = "fixture runner cleanup could not confirm container removal"
			fixture.script.FailClose(config.BackendCodex, &sandbox.SandboxError{Err: errors.New(cleanupMessage)})
			var message string
			if operation == "audit" {
				app := fixture.pausedApp(t)
				id, err := app.StartAudit(context.Background())
				if err == nil || id != "" {
					t.Fatalf("audit passed a missing route: id=%q error=%v", id, err)
				}
				message = err.Error()
				if !app.runtimeIdle() {
					t.Error("failed audit left preflight active")
				}
			} else {
				task := executionTask(t, fixture.planningFixture, fixture.cfg.DefaultBranch)
				saveExecutionTask(t, fixture.planningFixture, task)
				saved := driveTask(t, fixture.planningFixture, fixture.newApp(t), task.ID)
				if !blockedAs(saved, model.BlockedReasonRunnerUnavailable) || saved.Error == nil {
					t.Fatalf("execution passed a missing route: %+v", saved)
				}
				message = *saved.Error
				if saved.Workspace != "" || len(saved.Sessions) != 0 {
					t.Errorf("failed route preflight initialized execution: %+v", saved)
				}
			}
			for _, want := range []string{"unavailable in this runtime", cleanupMessage} {
				if !strings.Contains(message, want) {
					t.Errorf("%s preflight omitted %q: %s", operation, want, message)
				}
			}
			closed := 0
			for _, call := range fixture.script.Calls() {
				switch call.Kind {
				case runnertest.CallClose:
					closed++
				case runnertest.CallStart, runnertest.CallTurn:
					t.Errorf("failed preflight started a model session: %+v", call)
				}
			}
			if closed != 1 {
				t.Errorf("runner closed %d times; want once", closed)
			}
			assertAdmissions(t, fixture.state, 0, "failed route preflight")
			assertNoOpenClients(t, fixture.script)
		})
	}
}

type diagnosticCatalogFailure struct {
	runner.Adapter
	err    error
	cancel context.CancelFunc
}

func (a diagnosticCatalogFailure) Models(string) ([]runner.Model, error) {
	if a.cancel != nil {
		a.cancel()
	}
	return nil, a.err
}
