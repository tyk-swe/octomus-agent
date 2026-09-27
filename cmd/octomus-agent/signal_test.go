package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/redact"
)

// serviceHelperData names the data directory a re-executed test binary runs
// the real service in; see TestServiceHelperProcess.
const serviceHelperData = "OCTOMUS_TEST_SERVICE_DATA"

// TestServiceHelperProcess is not a test: the signal tests re-execute the test
// binary with serviceHelperData set so the real service runs in its own
// process, where delivered signals cannot affect the test runner.
func TestServiceHelperProcess(t *testing.T) {
	data, ok := os.LookupEnv(serviceHelperData)
	if !ok {
		return
	}
	os.Exit(run([]string{"--data-dir", data, "--listen", "127.0.0.1:0"}, os.LookupEnv, os.Stdout, os.Stderr))
}

// serviceProcess is the real service running in a child test binary.
type serviceProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error // valid once done is closed
}

// exit waits up to limit for the service to exit and reports whether it did.
func (p *serviceProcess) exit(limit time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(limit):
		return false
	}
}

// startServiceProcess runs the service in a child process and returns once it
// is listening. With hangupIgnored the child starts with SIGHUP ignored, as
// nohup leaves it.
func startServiceProcess(t *testing.T, hangupIgnored bool) *serviceProcess {
	t.Helper()
	args := []string{"-test.run=^TestServiceHelperProcess$"}
	command := exec.Command(os.Args[0], args...)
	if hangupIgnored {
		// An ignored disposition survives exec, exactly as nohup hands it on.
		command = exec.Command("/bin/sh", append([]string{"-c", `trap "" HUP; exec "$0" "$@"`, os.Args[0]}, args...)...)
	}
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "OCTOMUS_") {
			env = append(env, entry)
		}
	}
	command.Env = append(env, serviceHelperData+"="+t.TempDir(), redact.TokenEnv+"="+strings.Repeat("t", 32))
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &serviceProcess{command: command, done: make(chan struct{})}
	listening := make(chan struct{})
	go func() {
		defer close(process.done)
		var lines []string
		announced := false
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
			if !announced && strings.HasPrefix(scanner.Text(), "Octomus listening on ") {
				announced = true
				close(listening)
			}
		}
		// Wait closes the pipe, so it runs only after the reader saw end of file.
		if err := command.Wait(); err != nil {
			process.err = fmt.Errorf("%w; stderr: %s", err, strings.Join(lines, "\n"))
		}
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-process.done
	})
	select {
	case <-listening:
	case <-process.done:
		t.Fatalf("service exited before listening: %v", process.err)
	case <-time.After(30 * time.Second):
		t.Fatal("service never started listening")
	}
	return process
}

// A terminal or SSH hangup stops the foreground service through the same
// graceful drain as SIGTERM, so owned process groups are terminated rather
// than orphaned; under nohup the hangup stays ignored.
func TestHangupStopsTheServiceGracefullyUnlessIgnored(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		if signal.Ignored(syscall.SIGHUP) {
			// An inherited ignored hangup cannot be restored for the child.
			t.Skip("the test runner itself ignores SIGHUP")
		}
		process := startServiceProcess(t, false)
		if err := process.command.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		if !process.exit(30 * time.Second) {
			t.Fatal("service kept running after a hangup")
		}
		if process.err != nil {
			t.Fatalf("hangup did not shut the service down cleanly: %v", process.err)
		}
	})
	t.Run("nohup", func(t *testing.T) {
		process := startServiceProcess(t, true)
		if err := process.command.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		if process.exit(500 * time.Millisecond) {
			t.Fatalf("an ignored hangup stopped the service: %v", process.err)
		}
		if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if !process.exit(30 * time.Second) {
			t.Fatal("service kept running after termination")
		}
		if process.err != nil {
			t.Fatalf("termination after an ignored hangup: %v", process.err)
		}
	})
}
