package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type runLoopHealthProbe struct {
	sandbox.Host
	calls atomic.Uint64
	err   error
}

func (b *runLoopHealthProbe) Healthy(context.Context) error {
	b.calls.Add(1)
	return b.err
}

func TestRunRetriesPlanningRecoveryWithoutChangingContinuousSchedule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		due          bool
		refuseEvents bool
	}{
		{name: "saved future backoff"},
		{name: "due schedule", due: true},
		{name: "due schedule with activity refusal", due: true, refuseEvents: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newScriptedPlanningFixture(t)
			completePlan(t, fixture).queue(fixture)
			backend := &runLoopHealthProbe{}
			app := fixture.pausedApp(t, WithSandbox(backend))
			// Count refused recovery writes independently of whether activity is
			// inserted or coalesced. FAIL preserves this test counter, not the cycle write.
			schedulerSQL(t, fixture.state, "CREATE TEMP TABLE recovery_write_attempts (count INTEGER)")
			schedulerSQL(t, fixture.state, "INSERT INTO recovery_write_attempts VALUES (0)")
			schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_cycle_terminal BEFORE UPDATE ON records
				WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')!='running'
				BEGIN UPDATE recovery_write_attempts SET count=count+1;
				SELECT RAISE(FAIL, 'synthetic cycle terminal refusal bearer fixtureplanningsecret123'); END`)
			if err := app.Resume(); err != nil {
				t.Fatal(err)
			}
			if err := app.Tick(); err != nil {
				t.Fatal(err)
			}
			app.wg.Wait()
			cycles, err := store.List[model.Cycle](fixture.state, "cycle")
			if err != nil || len(cycles) != 1 || cycles[0].Status != model.CycleRunning || !app.runtimeIdle() {
				t.Fatalf("worker exit: cycles=%+v idle=%v error=%v", cycles, app.runtimeIdle(), err)
			}
			before := cycles[0]
			control, err := app.Control()
			if err != nil || control.Mode != model.OperatingModeContinuous || control.Error == nil || control.NextCycleAt <= time.Now().Unix() {
				t.Fatalf("worker did not preserve continuous backoff: %+v, %v", control, err)
			}
			if tc.due {
				// Advance the saved deadline without waiting the minimum 30-second interval.
				control.NextCycleAt = 0
				if err := fixture.state.SaveControl(control); err != nil {
					t.Fatal(err)
				}
			}
			var classified *recoveryError
			if err := app.Tick(); !errors.As(err, &classified) || errors.Unwrap(err) == nil || !strings.Contains(err.Error(), "synthetic cycle terminal refusal") {
				t.Fatalf("direct Tick did not propagate its recovery failure: %v", err)
			}
			app.runtimeMu.Lock()
			observation := app.runtime.prObservation
			app.runtimeMu.Unlock()
			if observation == nil {
				t.Fatal("failed planning lost its retained PR observation before recovery")
			}
			if tc.refuseEvents {
				schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_recovery_activity BEFORE INSERT ON events
					WHEN NEW.entity_id='system' AND NEW.kind IN ('error','recovery_error')
					BEGIN SELECT RAISE(ABORT, 'synthetic activity refusal'); END`)
			}
			attempts := func() int {
				t.Helper()
				var count int
				if err := fixture.state.Snapshot(func(conn *sql.Conn) error {
					return conn.QueryRowContext(context.Background(), "SELECT count FROM recovery_write_attempts").Scan(&count)
				}); err != nil {
					t.Fatal(err)
				}
				return count
			}
			var grounding *runnertest.Gate
			if tc.due {
				grounding = runnertest.NewGate()
				t.Cleanup(grounding.Release)
			}
			initialHealth := backend.calls.Load()
			initialAttempts := attempts()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- app.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				if err := waitPlanningStorage(t, done); err != nil {
					t.Error(err)
				}
			})
			// Entering a third recovery write proves two prior Run failure handlers
			// completed, even when an identical activity entry was coalesced.
			if !testutil.WaitUntil(10*time.Second, func() bool { return attempts() >= initialAttempts+3 }) {
				t.Fatal("Run did not retry the failed recovery")
			}
			if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
				t.Fatalf("recovery refusal changed continuous mode or backoff: %+v, %v; want %+v", saved, err, control)
			}
			if saved, err := store.Get[model.Cycle](fixture.state, "cycle", before.ID); err != nil || !wirejson.Equal(saved, before) {
				t.Fatalf("refused recovery changed retained evidence: %+v, %v", saved, err)
			}
			app.runtimeMu.Lock()
			sameObservation := app.runtime.prObservation == observation
			app.runtimeMu.Unlock()
			if !sameObservation {
				t.Fatal("recovery refusal revoked PR authority without a durable pause")
			}
			if backend.calls.Load() != initialHealth {
				t.Fatal("failed recovery allowed ordinary scheduling to proceed")
			}
			assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "failed recovery blocks every new admission, even when due")
			system := "system"
			events, err := fixture.state.Events(&system)
			if err != nil {
				t.Fatal(err)
			}
			recoveryEvents := 0
			for _, event := range events {
				if event.Kind == "error" || strings.Contains(event.Message, "fixtureplanningsecret123") {
					t.Fatalf("recovery activity used a fatal classification or exposed raw text: %+v", event)
				}
				if event.Kind == "recovery_error" {
					recoveryEvents++
					if !strings.Contains(event.Message, "synthetic cycle terminal refusal") {
						t.Fatalf("recovery activity omitted the cause: %+v", event)
					}
				}
			}
			if tc.refuseEvents && recoveryEvents != 0 || !tc.refuseEvents && recoveryEvents != 1 {
				t.Fatalf("recorded recovery errors=%d with activity refusal=%t", recoveryEvents, tc.refuseEvents)
			}
			if tc.due {
				plan := completePlan(t, fixture)
				plan.grounding.Gate = grounding
				plan.queue(fixture)
			}
			schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_cycle_terminal")
			app.notify()
			if !testutil.WaitUntil(10*time.Second, func() bool {
				cycle, err := store.Get[model.Cycle](fixture.state, "cycle", before.ID)
				if err != nil {
					t.Fatal(err)
				}
				return cycle.Status == model.CycleInterrupted && backend.calls.Load() > initialHealth
			}) {
				t.Fatal("Run did not resume ordinary scheduling after storage healed")
			}
			if tc.due {
				waitPlanningStorage(t, grounding.Entered())
				cycles, err := store.List[model.Cycle](fixture.state, "cycle")
				if err != nil || len(cycles) != 2 {
					t.Fatalf("healed due schedule did not start a fresh cycle: %+v, %v", cycles, err)
				}
			} else {
				assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "healed recovery still respects a future backoff")
			}
			if saved, err := app.Control(); err != nil || saved.Mode != model.OperatingModeContinuous || !tc.due && !wirejson.Equal(saved, control) {
				t.Fatalf("healed recovery did not preserve continuous scheduling: %+v, %v", saved, err)
			}
		})
	}
}

func TestRunStillPausesForOrdinarySchedulingFailure(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	backend := &runLoopHealthProbe{err: errors.New("synthetic ordinary health refusal")}
	app := fixture.pausedApp(t, WithSandbox(backend))
	if err := app.Resume(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := waitPlanningStorage(t, done); err != nil {
			t.Error(err)
		}
	})
	if !testutil.WaitUntil(10*time.Second, func() bool {
		control, err := app.Control()
		if err != nil {
			t.Fatal(err)
		}
		return control.Mode == model.OperatingModePaused && hasEvent(t, fixture.state, "system", "error", "synthetic ordinary health refusal")
	}) {
		t.Fatal("ordinary scheduling failure did not pause the service")
	}
	if !hasEvent(t, fixture.state, "system", "error", "synthetic ordinary health refusal") || hasEvent(t, fixture.state, "system", "recovery_error", "") {
		t.Fatal("ordinary scheduling failure lost its error activity classification")
	}
	assertAdmissions(t, fixture.state, 0, "ordinary scheduling errors must continue blocking new work")
}

func TestRecoveryErrorPreservesCauseAndRuntimeAuthority(t *testing.T) {
	t.Parallel()
	fixture := newScriptedPlanningFixture(t)
	app := fixture.pausedApp(t)
	control := model.DefaultControl()
	control.SetMode(model.OperatingModeContinuous)
	control.NextCycleAt = time.Now().Add(time.Hour).Unix()
	if err := fixture.state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	refreshContext, cancelRefresh := context.WithCancel(app.Context())
	defer cancelRefresh()
	observation := &freshPrObservation{}
	refresh := &prRefreshJob{cancel: cancelRefresh}
	app.runtimeMu.Lock()
	app.runtime.prObservation = observation
	app.runtime.prRefresh = refresh
	app.runtimeMu.Unlock()
	sentinel := errors.New("synthetic recovery failure")
	cause := &os.PathError{Op: "write", Path: "fixture", Err: sentinel}
	wrapped := fmt.Errorf("during scheduler recovery: %w", &recoveryError{err: cause})
	var typed *os.PathError
	if !errors.Is(wrapped, sentinel) || !errors.As(wrapped, &typed) || typed != cause {
		t.Fatal("recovery classification hid the original error chain")
	}
	app.fail(wrapped)
	if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
		t.Fatalf("retryable recovery changed durable control: %+v, %v", saved, err)
	}
	app.runtimeMu.Lock()
	unchanged := app.runtime.prObservation == observation && app.runtime.prRefresh == refresh
	app.runtimeMu.Unlock()
	if !unchanged || refreshContext.Err() != nil {
		t.Fatal("retryable recovery revoked PR observation or refresh authority")
	}
	select {
	case <-app.wake:
		t.Fatal("retryable recovery woke the scheduler into a busy retry loop")
	default:
	}
}
