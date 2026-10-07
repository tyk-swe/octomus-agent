package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

type pendingDiagnostic struct {
	runner.Adapter
	ctx       context.Context
	root      string
	entered   chan<- *pendingDiagnostic
	closing   chan struct{}
	allowExit <-chan struct{}
}

func (d *pendingDiagnostic) Models(string) ([]runner.Model, error) {
	d.entered <- d
	<-d.ctx.Done()
	return nil, d.ctx.Err()
}

func (d *pendingDiagnostic) Diagnose(string) (runner.Diagnostics, error) {
	_, err := d.Models("")
	return runner.Diagnostics{}, err
}

func (d *pendingDiagnostic) Close() error {
	close(d.closing)
	<-d.allowExit
	return nil
}

func TestDiagnosticCancellationJoinsBeforeRemovingScratch(t *testing.T) {
	for _, endpoint := range []string{"/api/model-catalog", "/api/doctor"} {
		for _, trigger := range []string{"disconnect", "shutdown"} {
			t.Run(endpoint+"/"+trigger, func(t *testing.T) {
				entered := make(chan *pendingDiagnostic, 2)
				allowExit := make(chan struct{})
				defer close(allowExit)
				connect := func(ctx context.Context, _ config.Backend, _ config.Config, cwd string) (runner.Adapter, error) {
					return &pendingDiagnostic{ctx: ctx, root: filepath.Dir(cwd), entered: entered, closing: make(chan struct{}), allowExit: allowExit}, nil
				}
				app, state := testApp(t, engine.WithRunnerConnector(connect))
				if endpoint == "/api/doctor" {
					cfg := config.Default()
					cfg.Repository = t.TempDir()
					cfg.GitHubRepo = "fixture/project"
					cfg.VerificationCommands = []string{"true"}
					for _, role := range config.Roles() {
						cfg.Roles[role] = config.NewRoute("scripted", "medium")
					}
					for _, tier := range config.Tiers() {
						cfg.Tiers[tier] = config.NewRoute("scripted", "medium")
					}
					cfg.RepairRoute = config.NewRoute("scripted", "medium")
					if err := os.Mkdir(filepath.Join(cfg.Repository, ".git"), 0o755); err != nil {
						t.Fatal(err)
					}
					bin := t.TempDir()
					for name, script := range map[string]string{"git": "#!/bin/sh\necho https://github.com/fixture/project.git\n", "gh": "#!/bin/sh\nexit 0\n"} {
						if err := testutil.WriteExecutable(filepath.Join(bin, name), []byte(script)); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
					if err := state.Put("settings", "config", cfg); err != nil {
						t.Fatal(err)
					}
				}
				router := Router(app, token, "", "test")
				handled := make(chan struct{}, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer func() { handled <- struct{}{} }()
					router.ServeHTTP(w, r)
				}))
				t.Cleanup(server.Close)
				t.Cleanup(app.Shutdown)
				clients := make(chan error, 2)
				var cancelRequests []context.CancelFunc
				for range 2 {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					cancelRequests = append(cancelRequests, cancel)
					method, body := http.MethodPost, "{}"
					if endpoint == "/api/model-catalog" {
						method, body = http.MethodPost, `{"backend":"codex","binary":"codex"}`
					}
					req, err := http.NewRequestWithContext(ctx, method, server.URL+endpoint, strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("Content-Type", "application/json")
					go func() {
						response, err := server.Client().Do(req)
						if response != nil {
							_, _ = io.Copy(io.Discard, response.Body)
							_ = response.Body.Close()
						}
						clients <- err
					}()
				}
				var diagnostics []*pendingDiagnostic
				for range 2 {
					select {
					case diagnostic := <-entered:
						diagnostics = append(diagnostics, diagnostic)
					case err := <-clients:
						t.Fatalf("request returned before entering diagnostic: %v", err)
					case <-time.After(5 * time.Second):
						t.Fatal("diagnostic did not start")
					}
				}
				shutdownDone := make(chan struct{})
				if trigger == "disconnect" {
					for _, cancel := range cancelRequests {
						cancel()
					}
				} else {
					go func() { app.Shutdown(); close(shutdownDone) }()
				}
				for _, diagnostic := range diagnostics {
					select {
					case <-diagnostic.closing:
					case <-time.After(5 * time.Second):
						t.Fatal("diagnostic was not canceled and joined")
					}
					if !errors.Is(diagnostic.ctx.Err(), context.Canceled) {
						t.Fatalf("runner context = %v", diagnostic.ctx.Err())
					}
					if _, err := os.Stat(diagnostic.root); err != nil {
						t.Fatalf("scratch removed before runner cleanup: %v", err)
					}
				}
				select {
				case <-shutdownDone:
					t.Fatal("shutdown returned before runner cleanup")
				default:
				}
				// Release each close without ending the test's deferred fallback release.
				allowExit <- struct{}{}
				allowExit <- struct{}{}
				for range 2 {
					select {
					case <-handled:
					case <-time.After(5 * time.Second):
						t.Fatal("diagnostic handler did not finish")
					}
				}
				for _, diagnostic := range diagnostics {
					if _, err := os.Stat(diagnostic.root); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("scratch retained after runner cleanup: %v", err)
					}
				}
				if trigger == "shutdown" {
					select {
					case <-shutdownDone:
					case <-time.After(5 * time.Second):
						t.Fatal("shutdown did not join diagnostic cleanup")
					}
				}
			})
		}
	}
}
