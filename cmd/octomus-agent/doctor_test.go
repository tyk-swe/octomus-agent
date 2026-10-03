package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func doctorFixture(t *testing.T, git, gh string) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{"git": git, "gh": gh} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	data := t.TempDir()
	state, err := store.Open(filepath.Join(data, stateDBName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	cfg := config.Default()
	cfg.Repository = repo
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"true"}
	for key := range cfg.Roles {
		cfg.Roles[key] = doctorRoute
	}
	for key := range cfg.Tiers {
		cfg.Tiers[key] = doctorRoute
	}
	cfg.RepairRoute = doctorRoute
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	return state, data
}

var doctorRoute = config.NewRoute("m", "medium")

type mismatchAdapter struct {
	runner.Adapter
	warning string
}

func (m mismatchAdapter) Diagnose(cwd string) (runner.Diagnostics, error) {
	diagnostics, err := m.Adapter.Diagnose(cwd)
	diagnostics.Warning = &m.warning
	return diagnostics, err
}

func TestDoctorPrintsWarningsToCommandStderr(t *testing.T) {
	state, data := doctorFixture(t, "#!/bin/sh\necho https://github.com/fixture/project.git\n", "#!/bin/sh\nexit 0\n")
	script := runnertest.New(runnertest.CatalogFor(doctorRoute)...)
	warning := runner.VersionWarning(config.BackendCodex, "scripted", "tested with a fixture")
	connect := script.Connector()
	app := engine.New(state, data, engine.WithRunnerConnector(func(ctx context.Context, backend config.Backend, cfg config.Config, cwd string) (runner.Adapter, error) {
		client, err := connect(ctx, backend, cfg, cwd)
		if err != nil {
			return nil, err
		}
		return mismatchAdapter{Adapter: client, warning: warning}, nil
	}))
	t.Cleanup(app.Shutdown)

	var stdout, stderr bytes.Buffer
	if err := runDoctor(context.Background(), app, model.CycleModeExecution, &stdout, &stderr); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if stderr.String() != "WARN "+warning+"\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var result struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || len(result.Warnings) != 1 || result.Warnings[0] != warning {
		t.Fatalf("stdout warnings = %q, %v:\n%s", result.Warnings, err, stdout.String())
	}

	script.SetCatalog()
	stdout.Reset()
	stderr.Reset()
	if err := runDoctor(context.Background(), app, model.CycleModeExecution, &stdout, &stderr); err == nil {
		t.Fatal("doctor passed without a catalog")
	}
	if stderr.String() != "WARN "+warning+"\n" || stdout.Len() != 0 {
		t.Fatalf("failing doctor stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
