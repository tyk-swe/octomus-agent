package broker

import (
	"context"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/sandbox/wire"
)

func probePlan(timeout time.Duration) plan {
	return plan{kind: wire.KindProbe, probe: wire.ProbeVersions, timeout: timeout}
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
