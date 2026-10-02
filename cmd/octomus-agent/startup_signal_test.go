package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const (
	startupSignalHelperData       = "OCTOMUS_TEST_STARTUP_SIGNAL_DATA"
	startupSignalHelperDeployment = "OCTOMUS_TEST_STARTUP_SIGNAL_DEPLOYMENT"
	startupSignalHelperDoctor     = "OCTOMUS_TEST_STARTUP_SIGNAL_DOCTOR"
)

func TestServiceStartupSignalHelper(t *testing.T) {
	data, helper := os.LookupEnv(startupSignalHelperData)
	if !helper {
		return
	}
	// Observe delivery without a timing sleep. service has already registered
	// its startup handler when prepareDeployment first reads this setting.
	observed, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	deployment := os.Getenv(startupSignalHelperDeployment)
	sent := false
	env := func(key string) (string, bool) {
		if deployment == "" && key == "OCTOMUS_GITHUB_REPO" && !sent {
			sent = true
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			<-observed.Done()
		}
		if deployment != "" {
			switch key {
			case "OCTOMUS_GITHUB_REPO":
				return "fixture/project", true
			case githubTokenEnv:
				return "fixture-token", true
			}
		}
		if key == redact.TokenEnv {
			return strings.Repeat("t", 32), true
		}
		return "", false
	}
	mode := "off"
	if deployment != "" {
		mode = "docker"
	}
	args := []string{"--sandbox", mode, "--data-dir", data, "--listen", "127.0.0.1:0"}
	if os.Getenv(startupSignalHelperDoctor) == "1" {
		args = append(args, "--doctor")
	}
	os.Exit(run(args, env, os.Stdout, os.Stderr))
}

func startupSignalProcess(t *testing.T, doctor bool, deployment string, interrupted bool) (string, int) {
	t.Helper()
	data := t.TempDir()
	path := "/usr/bin:/bin"
	if doctor && deployment == "" {
		// If the signal handler is still being scheduled when preparation
		// returns, doctor must reach a cancellable check instead of failing
		// immediately on the intentionally empty service configuration.
		repo := filepath.Join(data, "repo")
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		state, err := store.Open(filepath.Join(data, stateDBName))
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.Repository, cfg.GitHubRepo = repo, "fixture/project"
		cfg.VerificationCommands = []string{"true"}
		for key := range cfg.Roles {
			cfg.Roles[key] = doctorRoute
		}
		for key := range cfg.Tiers {
			cfg.Tiers[key] = doctorRoute
		}
		cfg.RepairRoute = doctorRoute
		err = state.Put("settings", "config", cfg)
		if closeErr := state.Close(); err != nil || closeErr != nil {
			t.Fatalf("prepare doctor configuration: %v; close: %v", err, closeErr)
		}
	}
	if deployment != "" || doctor {
		bin := t.TempDir()
		path = bin + string(os.PathListSeparator) + path
		if deployment == "existing-remote" {
			if err := os.MkdirAll(filepath.Join(data, "checkout", ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		script := "#!/bin/sh\n"
		if deployment == "new-remote" {
			// Simulate a completed clone, then interrupt or fail its validation.
			script += "if [ \"$1\" = clone ]; then mkdir -p \"$5/.git\"; exit; fi\n"
		}
		script += ": > \"$" + startupSignalHelperData + "/git-started\"\n"
		if deployment == "" {
			script += "exec sleep 97\n"
		} else if interrupted {
			// Stay blocked until service cancels its own preparation context;
			// this exercises the error returned by an interrupted Git command.
			script += "kill -TERM \"$PPID\"\nexec sleep 97\n"
		} else {
			script += "echo 'fixture preparation failure' >&2\nexit 1\n"
		}
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceStartupSignalHelper$")
	command.Env = []string{"PATH=" + path, startupSignalHelperData + "=" + data, startupSignalHelperDeployment + "=" + deployment}
	if doctor {
		command.Env = append(command.Env, startupSignalHelperDoctor+"=1")
	}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("startup did not finish: %v\n%s", ctx.Err(), output)
	}
	if deployment != "" {
		if _, err := os.Stat(filepath.Join(data, "git-started")); err != nil {
			t.Fatalf("preparation did not reach the fake Git command: %v\n%s", err, output)
		}
	}
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		t.Fatalf("could not run startup helper: %v\n%s", err, output)
	}
	return string(output), command.ProcessState.ExitCode()
}

func TestServiceHonorsTerminationDuringDeploymentPreparation(t *testing.T) {
	t.Parallel()
	if output, code := startupSignalProcess(t, false, "", true); code != 0 {
		t.Fatalf("startup termination was not graceful: exit %d\n%s", code, output)
	}
}

func TestDoctorHonorsTerminationDuringDeploymentPreparation(t *testing.T) {
	t.Parallel()
	if output, code := startupSignalProcess(t, true, "", true); code != 1 || !strings.Contains(output, "Error: Doctor interrupted\n") {
		t.Fatalf("doctor did not report interruption: exit %d\n%s", code, output)
	}
}

func TestDeploymentPreparationErrorsAndTermination(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"service", "doctor"} {
		for _, deployment := range []string{"clone", "existing-remote", "new-remote"} {
			for _, result := range []string{"interrupted", "failed"} {
				t.Run(mode+"/"+deployment+"/"+result, func(t *testing.T) {
					t.Parallel()
					doctor := mode == "doctor"
					interrupted := result == "interrupted"
					output, code := startupSignalProcess(t, doctor, deployment, interrupted)
					if interrupted && !doctor {
						if code != 0 || strings.Contains(output, "Error:") {
							t.Fatalf("startup termination was not graceful: exit %d\n%s", code, output)
						}
						return
					}
					want := "fixture preparation failure"
					if interrupted {
						want = "Error: Doctor interrupted\n"
					}
					if code != 1 || !strings.Contains(output, want) {
						t.Fatalf("startup did not report %q: exit %d\n%s", want, code, output)
					}
				})
			}
		}
	}
}
