package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// An interrupted doctor stops its checks and terminates the owned process
// group it started (here a git that never answers) instead of leaving it
// running after the command exits.
func TestDoctorInterruptTerminatesOwnedChildGroups(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "git.pid")
	shim := "#!/bin/sh\necho $$ > \"$OCTOMUS_TEST_PIDFILE.tmp\"\nmv \"$OCTOMUS_TEST_PIDFILE.tmp\" \"$OCTOMUS_TEST_PIDFILE\"\nexec sleep 97\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OCTOMUS_TEST_PIDFILE", pidFile)
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
	route := config.NewRoute("m", "medium")
	for key := range cfg.Roles {
		cfg.Roles[key] = route
	}
	for key := range cfg.Tiers {
		cfg.Tiers[key] = route
	}
	cfg.RepairRoute = route
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	app := engine.New(state, data)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runDoctor(ctx, app, model.CycleModeExecution, io.Discard) }()

	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; {
		select {
		case err := <-done:
			t.Fatalf("doctor finished before starting git: %v", err)
		default:
		}
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err = strconv.Atoi(strings.TrimSpace(string(raw))); err != nil {
				t.Fatal(err)
			}
		} else if time.Now().After(deadline) {
			t.Fatal("git shim never started")
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	// Only a failing run can leave the shim's group behind; never signal a
	// pid the passing path has already seen reaped.
	fail := func(format string, args ...any) {
		t.Helper()
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		t.Fatalf(format, args...)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != "Doctor interrupted" {
			fail("interrupted doctor: %v", err)
		}
	case <-time.After(10 * time.Second):
		fail("doctor did not stop after interruption")
	}
	// Capture reaps the group leader before returning, so the pid is gone
	// rather than a zombie.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		fail("git shim %d survived the interrupted doctor: %v", pid, err)
	}
}
