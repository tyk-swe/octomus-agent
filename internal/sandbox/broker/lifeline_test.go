package broker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// tune sets a teardown bound for one test.
func tune(t *testing.T, bound *time.Duration, value time.Duration) {
	t.Helper()
	previous := *bound
	*bound = value
	t.Cleanup(func() { *bound = previous })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var versionsProbe = sandbox.Spec{Kind: sandbox.KindProbe, Probe: wire.ProbeVersions}

func probePlan(timeout time.Duration) plan {
	return plan{kind: wire.KindProbe, probe: wire.ProbeVersions, timeout: timeout}
}

func TestFailedTimeLimitKillIsRetried(t *testing.T) {
	e := newFakeEngine(t)
	var kills atomic.Int32
	e.kill = func(_ *fakeContainer, signal string) (int, string) {
		if signal == "SIGKILL" && kills.Add(1) == 1 {
			return http.StatusInternalServerError, "fixture kill failure"
		}
		return 0, ""
	}
	e.run = func(*fakeContainer) {}
	b := e.broker(t, testConfig(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := b.runSandbox(ctx, probePlan(200*time.Millisecond), discard, discard, nil)
	if err != nil || !report.Killed || report.Error != "Sandbox time limit reached" || report.Code != 137 {
		t.Fatalf("time limit after a failed kill = %+v, %v; want the limit enforced", report, err)
	}
}

func TestKillWhoseReplyIsLateStillReportsTheKill(t *testing.T) {
	tune(t, &killWait, 200*time.Millisecond)
	// Docker answers a SIGKILL only once the container has stopped; on a loaded host that outlasts the broker's wait.
	slowKill := func(c *fakeContainer, signal string) (int, string) {
		if signal == "SIGKILL" {
			c.closeOutput()
			c.Exit(137, false)
			time.Sleep(600 * time.Millisecond)
		}
		return 0, ""
	}
	t.Run("time limit", func(t *testing.T) {
		e := newFakeEngine(t)
		e.kill, e.run = slowKill, func(*fakeContainer) {}
		report, err := e.broker(t, testConfig(t)).runSandbox(context.Background(), probePlan(100*time.Millisecond), discard, discard, nil)
		if err != nil || !report.Killed || report.Error != sandbox.TimeLimitReason || report.Code != 137 {
			t.Fatalf("time limit whose kill reply came late = %+v, %v; want the limit named", report, err)
		}
	})
	t.Run("client kill", func(t *testing.T) {
		e := newFakeEngine(t)
		e.kill, e.run = slowKill, func(*fakeContainer) {}
		child, err := serve(t, e.broker(t, testConfig(t))).Start(context.Background(), versionsProbe)
		if err != nil {
			t.Fatal(err)
		}
		e.WaitCreated(t, 1)
		child.Kill()
		if status, err := child.Wait(); !errors.Is(status.Err(), process.ErrKilled) || err != nil {
			t.Fatalf("kill whose reply came late = %v, %v; want it reported as Octomus's kill", status, err)
		}
	})
	t.Run("exit of its own", func(t *testing.T) {
		e := newFakeEngine(t)
		// The command ends on its own while a kill is still unanswered: its exit status stands.
		e.kill = func(c *fakeContainer, signal string) (int, string) {
			if signal == "SIGKILL" {
				c.End(3)
				time.Sleep(600 * time.Millisecond)
			}
			return 0, ""
		}
		e.run = func(*fakeContainer) {}
		report, err := e.broker(t, testConfig(t)).runSandbox(context.Background(), probePlan(100*time.Millisecond), discard, discard, nil)
		if err != nil || report.Killed || report.Code != 3 || report.Error != "" {
			t.Fatalf("exit during an unanswered kill = %+v, %v; want the exit's own status", report, err)
		}
	})
}

func TestKillReportsWhatEndedTheSandbox(t *testing.T) {
	run := func(t *testing.T, e *fakeEngine, timeout time.Duration) wire.ExitReport {
		t.Helper()
		// The daemon reports the exit a moment late, so the kill arrives after the sandbox already ended.
		e.waitDelay = 300 * time.Millisecond
		b := e.broker(t, testConfig(t))
		controls := make(chan control)
		type ended struct {
			report wire.ExitReport
			err    error
		}
		done := make(chan ended, 1)
		go func() {
			report, err := b.runSandbox(context.Background(), probePlan(timeout), discard, discard, controls)
			done <- ended{report, err}
		}()
		<-e.WaitCreated(t, 1).Exited()
		var got ended
		select {
		case controls <- control{signal: wire.SignalKill}:
			got = <-done
		case got = <-done:
			// The exit reached the broker before the kill could: the report must be the same.
		}
		if got.err != nil {
			t.Fatal(got.err)
		}
		return got.report
	}
	t.Run("exited before the kill", func(t *testing.T) {
		e := newFakeEngine(t)
		e.run = func(c *fakeContainer) { c.End(3) }
		if report := run(t, e, time.Minute); report.Killed || report.Code != 3 || report.Error != "" {
			t.Fatalf("kill after a natural exit = %+v; want the exit's own status", report)
		}
	})
	t.Run("time limit before a kill", func(t *testing.T) {
		e := newFakeEngine(t)
		e.run = func(*fakeContainer) {}
		if report := run(t, e, 100*time.Millisecond); !report.Killed || report.Error != "Sandbox time limit reached" {
			t.Fatalf("kill after the time limit stopped it = %+v; want the time limit named", report)
		}
	})
}

func TestOutputAfterTheExitArrivesBeforeTheReport(t *testing.T) {
	e := newFakeEngine(t)
	e.run = func(c *fakeContainer) {
		c.Stdout("before ")
		c.Exit(0, false)
		time.Sleep(100 * time.Millisecond)
		c.Stdout("after")
		c.closeOutput()
	}
	child, err := serve(t, e.broker(t, testConfig(t))).Start(context.Background(), versionsProbe)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(child.Stdout())
	if status, err := child.Wait(); err != nil || !status.Success() || string(out) != "before after" {
		t.Fatalf("sandbox = %v, %v with output %q", status, err, out)
	}
}

func TestOutputThatOutlivesTheSandboxIsReportedCutShort(t *testing.T) {
	tune(t, &drainWait, 300*time.Millisecond)
	e := newFakeEngine(t)
	e.run = func(c *fakeContainer) {
		c.Stdout("partial")
		c.Exit(0, false)
	}
	child, err := serve(t, e.broker(t, testConfig(t))).Start(context.Background(), versionsProbe)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
	if status, err := child.Wait(); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("sandbox whose output did not end = %v, %v; want the output reported cut short", status, err)
	}
}

func TestClientThatStopsReadingCannotHoldItsSandbox(t *testing.T) {
	tune(t, &drainWait, 200*time.Millisecond)
	tune(t, &joinWait, 200*time.Millisecond)
	e := newFakeEngine(t)
	e.run = func(c *fakeContainer) {
		go func() {
			chunk := strings.Repeat("x", 32<<10)
			for range 256 {
				c.Stdout(chunk)
			}
		}()
		time.Sleep(200 * time.Millisecond)
		c.Exit(0, false)
	}
	cfg := testConfig(t)
	cfg.Max = 1
	b := e.broker(t, cfg)
	child, err := serve(t, b).Start(context.Background(), versionsProbe)
	if err != nil {
		t.Fatal(err)
	}
	// Nobody reads the sandbox's stdout, so the control plane stops reading its stream.
	waitUntil(t, "the broker removed the sandbox and gave its slot back", func() bool {
		return len(e.Remaining()) == 0 && b.Info().Live == 0 && len(b.slots) == 0
	})
	go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
	_, _ = child.Wait()
}

func TestOnlyAFailedCommandReportsTheMemoryLimit(t *testing.T) {
	for _, code := range []int{0, 137} {
		e := newFakeEngine(t)
		// The kernel killed a process in the sandbox for memory; the command itself exits with code.
		e.run = func(c *fakeContainer) {
			c.closeOutput()
			c.Exit(code, true)
		}
		report, err := e.broker(t, testConfig(t)).runSandbox(context.Background(), probePlan(time.Minute), discard, discard, nil)
		if err != nil || report.Code != code || report.OOM != (code != 0) || report.Sandbox == nil || !report.Sandbox.OOM {
			t.Fatalf("exit %d after an OOM kill = %+v, %v; want the status OOM only for a failure, the evidence always", code, report, err)
		}
	}
}

func TestSandboxThatFailsAfterItStartedKeepsItsRecord(t *testing.T) {
	e := newFakeEngine(t)
	// The daemon loses the wait while the program runs.
	e.waitStatus = http.StatusInternalServerError
	e.run = func(*fakeContainer) {}
	child, err := serve(t, e.broker(t, testConfig(t))).Start(context.Background(), versionsProbe)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, child.Stdout()) }()
	if _, err := child.Wait(); err == nil || !strings.Contains(err.Error(), "Waiting for the sandbox") {
		t.Fatalf("sandbox whose wait failed = %v; want the failure reported", err)
	}
	if sandbox.EvidenceOf(child) == nil {
		t.Fatal("a sandbox whose program ran before it failed has no record")
	}
}

func TestLostEvidenceMarksTheRecordIncomplete(t *testing.T) {
	gateway := func(t *testing.T, listener net.Listener) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		g := egress.New(egress.Policy{}, t.TempDir(), io.Discard)
		go func() { _ = g.ServeCollector(ctx, listener) }()
	}
	// The containment probe holds an egress lease, as every agent and verification sandbox does.
	run := func(t *testing.T, e *fakeEngine, collector string) (*model.SandboxRecord, string) {
		t.Helper()
		cfg := testConfig(t)
		cfg.LeaseDir, cfg.EgressProxy, cfg.EgressCollector = t.TempDir(), "egress:3128", collector
		log := &testutil.SyncBuffer{}
		cfg.Log = log
		report, err := e.broker(t, cfg).runSandbox(context.Background(),
			plan{kind: wire.KindProbe, probe: wire.ProbeContainment, timeout: time.Minute}, discard, discard, nil)
		if err != nil || report.Sandbox == nil {
			t.Fatalf("sandbox = %+v, %v", report, err)
		}
		return report.Sandbox, log.String()
	}
	t.Run("complete", func(t *testing.T) {
		listener, socket := testutil.ListenUnix(t, "collector.sock")
		gateway(t, listener)
		if record, log := run(t, newFakeEngine(t), socket); record.Incomplete {
			t.Fatalf("record read in full = %+v (log %q); want it complete", record, log)
		}
	})
	t.Run("collector unreachable", func(t *testing.T) {
		record, log := run(t, newFakeEngine(t), filepath.Join(t.TempDir(), "missing.sock"))
		if !record.Incomplete || !strings.Contains(log, "egress record of sandbox") {
			t.Fatalf("record without its egress = %+v (log %q); want it marked incomplete and logged", record, log)
		}
	})
	t.Run("gateway restarted", func(t *testing.T) {
		listener, socket := testutil.ListenUnix(t, "collector.sock")
		e := newFakeEngine(t)
		// The gateway that answers started while the sandbox ran: what came before is lost to it.
		e.run = func(c *fakeContainer) {
			gateway(t, listener)
			c.End(0)
		}
		if record, log := run(t, e, socket); !record.Incomplete || !strings.Contains(log, "restarted") {
			t.Fatalf("record from a restarted gateway = %+v (log %q); want it marked incomplete", record, log)
		}
	})
	t.Run("state unreadable", func(t *testing.T) {
		listener, socket := testutil.ListenUnix(t, "collector.sock")
		gateway(t, listener)
		e := newFakeEngine(t)
		e.inspectStatus = http.StatusInternalServerError
		if record, log := run(t, e, socket); !record.Incomplete || record.OOM {
			t.Fatalf("record without the memory state = %+v (log %q); want it marked incomplete", record, log)
		}
	})
}
