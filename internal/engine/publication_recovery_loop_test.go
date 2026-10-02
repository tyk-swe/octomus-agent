package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestRunPublicationRecoveryFailureKeepsContinuousMode(t *testing.T) {
	t.Parallel()
	for _, activityOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("activity_only=%t", activityOnly), func(t *testing.T) {
			fixture := newScriptedFixture(t)
			backend := &runLoopHealthProbe{}
			app := fixture.pausedApp(t, WithSandbox(backend))
			control := model.DefaultControl()
			control.SetMode(model.OperatingModeContinuous)
			control.NextCycleAt = time.Now().Add(time.Hour).Unix()
			if err := fixture.state.SaveControl(control); err != nil {
				t.Fatal(err)
			}
			task := queuedTask(fixture.cfg, model.ID(), fixture.cfg.DefaultBranch, fixture.cfg.BranchPrefix+model.ID())
			task.Status = model.StatusPublishing
			task.OutputCommit = stringPointer("retained-publication-output")
			saveExecutionTask(t, fixture.planningFixture, task)
			if activityOnly {
				schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_publication_recovery BEFORE INSERT ON events
					WHEN NEW.kind='status' AND NEW.message='Blocked'
					BEGIN SELECT RAISE(ABORT, 'synthetic publication recovery refusal'); END`)
			} else {
				schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_publication_recovery BEFORE UPDATE ON records
					WHEN NEW.kind='task' AND json_extract(NEW.data,'$.status')='blocked'
					BEGIN SELECT RAISE(ABORT, 'synthetic publication recovery refusal'); END`)
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
			wantEvents := 2
			if activityOnly {
				wantEvents = 1
			}
			if !testutil.WaitUntil(10*time.Second, func() bool {
				system := "system"
				events, err := fixture.state.Events(&system)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, event := range events {
					if strings.Contains(event.Message, "synthetic publication recovery refusal") {
						count++
					}
				}
				return count >= wantEvents
			}) {
				t.Fatal("service loop did not report publication recovery failure")
			}
			if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
				t.Fatalf("publication recovery failure changed continuous control: %+v, %v", saved, err)
			}
			if hasEvent(t, fixture.state, "system", "error", "") || !hasEvent(t, fixture.state, "system", "recovery_error", "synthetic publication recovery refusal") {
				t.Fatal("publication recovery used the fatal scheduler-error classification")
			}
			if !activityOnly && (loadTask(t, fixture.state, task.ID).Status != model.StatusPublishing || backend.calls.Load() != 0) {
				t.Fatal("refused publication recovery changed its checkpoint or allowed ordinary scheduling")
			}
			schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_publication_recovery")
			app.notify()
			if !testutil.WaitUntil(10*time.Second, func() bool {
				return blockedAs(loadTask(t, fixture.state, task.ID), model.BlockedReasonPublicationUncertain) && backend.calls.Load() > 0
			}) {
				t.Fatal("healed publication recovery did not expose reconciliation and resume scheduling")
			}
			if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
				t.Fatalf("healed publication recovery changed continuous control: %+v, %v", saved, err)
			}
			assertAdmissions(t, fixture.state, 0, "publication recovery cannot replay any model work")
			if entries := publications(t, fixture.planningFixture); len(entries) != 0 {
				t.Fatalf("publication recovery retried delivery: %+v", entries)
			}
		})
	}
}
