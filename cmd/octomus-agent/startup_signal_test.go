package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/redact"
)

const startupSignalHelperData = "OCTOMUS_TEST_STARTUP_SIGNAL_DATA"

func TestServiceStartupSignalHelper(t *testing.T) {
	data, helper := os.LookupEnv(startupSignalHelperData)
	if !helper {
		return
	}
	// Observe delivery without a timing sleep. service has already registered
	// its startup handler when prepareDeployment first reads this setting.
	observed, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	sent := false
	env := func(key string) (string, bool) {
		if key == "OCTOMUS_GITHUB_REPO" && !sent {
			sent = true
			if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			<-observed.Done()
		}
		if key == redact.TokenEnv {
			return strings.Repeat("t", 32), true
		}
		return "", false
	}
	os.Exit(run([]string{"--sandbox", "off", "--data-dir", data, "--listen", "127.0.0.1:0"}, env, os.Stdout, os.Stderr))
}

func TestServiceHonorsTerminationDuringDeploymentPreparation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceStartupSignalHelper$")
	command.Env = []string{"PATH=/usr/bin:/bin", startupSignalHelperData + "=" + t.TempDir()}
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("service kept running after SIGTERM during deployment preparation: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("startup termination was not graceful: %v\n%s", err, output)
	}
}
