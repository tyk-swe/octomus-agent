package engine

import (
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func TestContinuousPlanningRecoveryHonorsSavedSchedule(t *testing.T) {
	t.Parallel()
	for _, recovery := range []string{"live tick", "restart"} {
		for _, refuseControl := range []bool{false, true} {
			name := recovery + "/saved backoff"
			if refuseControl {
				name = recovery + "/refused backoff"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newScriptedPlanningFixture(t)
				completePlan(t, fixture).queue(fixture)
				app := fixture.pausedApp(t)
				schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_cycle_terminal BEFORE UPDATE ON records
					WHEN NEW.kind='cycle' AND json_extract(NEW.data,'$.status')!='running'
					BEGIN SELECT RAISE(ABORT, 'synthetic cycle terminal refusal'); END`)
				if refuseControl {
					schedulerSQL(t, fixture.state, `CREATE TEMP TRIGGER refuse_cycle_backoff BEFORE UPDATE ON records
						WHEN NEW.kind='settings' AND NEW.id='control' AND json_extract(NEW.data,'$.next_cycle_at')>0
						BEGIN SELECT RAISE(ABORT, 'synthetic backoff refusal'); END`)
				}
				if err := app.Resume(); err != nil {
					t.Fatal(err)
				}
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
				app.wg.Wait()
				cycles, err := store.List[model.Cycle](fixture.state, "cycle")
				if err != nil || len(cycles) != 1 || cycles[0].Status != model.CycleRunning || !app.runtimeIdle() {
					t.Fatalf("worker did not leave a retained running cycle: %+v, %v", cycles, err)
				}
				old := cycles[0]
				control, err := app.Control()
				if err != nil || control.Mode != model.OperatingModeContinuous || control.Batch != nil || control.CycleNumber != old.Number {
					t.Fatalf("saved continuous policy: %+v, %v", control, err)
				}
				if refuseControl && control.NextCycleAt != 0 || !refuseControl && control.NextCycleAt <= time.Now().Unix() {
					t.Fatalf("unexpected saved schedule after backoff refusal=%t: %+v", refuseControl, control)
				}
				if tasks, err := store.List[model.Task](fixture.state, "task"); err != nil || len(tasks) != 0 {
					t.Fatalf("failed plan created executable work: %+v, %v", tasks, err)
				}
				schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_cycle_terminal")
				if refuseControl {
					schedulerSQL(t, fixture.state, "DROP TRIGGER refuse_cycle_backoff")
				}
				if recovery == "restart" {
					app.Shutdown()
					app = fixture.pausedApp(t)
					if err := app.Recover(); err != nil {
						t.Fatal(err)
					}
					if saved, err := app.Control(); err != nil || !wirejson.Equal(saved, control) {
						t.Fatalf("restart changed continuous scheduling: %+v, %v", saved, err)
					}
				}
				var newRevision string
				if refuseControl {
					// A newly due cycle must ground against the current local fixture remote,
					// not replay the source, sessions or accepted plan from the orphan.
					git(t, fixture.repo, "-c", "user.name=dot", "-c", "user.email=dot@localhost", "commit", "--allow-empty", "-m", "Advance fixture for fresh discovery")
					git(t, fixture.repo, "push", "origin", "main")
					newRevision = git(t, fixture.repo, "rev-parse", "HEAD")
					completePlan(t, fixture).queue(fixture)
				}
				if err := app.Tick(); err != nil {
					t.Fatal(err)
				}
				app.wg.Wait()
				interrupted, err := store.Get[model.Cycle](fixture.state, "cycle", old.ID)
				if err != nil || interrupted == nil || interrupted.Status != model.CycleInterrupted || interrupted.CompletedAt == nil || interrupted.Error == nil || *interrupted.Error != interruptedPlanningMessage {
					t.Fatalf("recovery did not settle the old cycle: %+v, %v", interrupted, err)
				}
				expected := old.Clone()
				expected.Status, expected.CompletedAt, expected.Error = interrupted.Status, interrupted.CompletedAt, interrupted.Error
				if !wirejson.Equal(interrupted, expected) {
					t.Fatal("continuous recovery rewrote the interrupted cycle's evidence")
				}
				current, err := app.Control()
				if err != nil || current.Mode != model.OperatingModeContinuous || current.Batch != nil {
					t.Fatalf("recovery changed the continuous operating mode: %+v, %v", current, err)
				}
				cycles, err = store.List[model.Cycle](fixture.state, "cycle")
				if err != nil {
					t.Fatal(err)
				}
				if !refuseControl {
					if len(cycles) != 1 || !wirejson.Equal(current, control) {
						t.Fatalf("recovery ignored the saved backoff: cycles=%d control=%+v", len(cycles), current)
					}
					assertAdmissions(t, fixture.state, fixture.cfg.PlanningAdmissionsRequired(), "saved backoff must defer a fresh cycle")
					assertNoOpenClients(t, fixture.script)
					return
				}
				if len(cycles) != 2 {
					t.Fatalf("due continuous operation created %d cycles; want the retained old cycle and one fresh cycle", len(cycles))
				}
				var next model.Cycle
				for _, cycle := range cycles {
					if cycle.ID != old.ID {
						next = cycle
					}
				}
				if next.Status != model.CycleCompleted || next.Number != old.Number+1 || next.Grounding == nil || next.Grounding.Revision != newRevision || next.Grounding.Revision == old.Grounding.Revision || len(next.Sessions) != int(fixture.cfg.PlanningAdmissionsRequired()) {
					t.Fatalf("due continuous operation did not ground a fresh cycle: %+v", next)
				}
				for _, session := range next.Sessions {
					if session.Status != model.SessionCompleted {
						t.Fatalf("new cycle did not complete its fresh session: %+v", session)
					}
					for _, prior := range old.Sessions {
						if session.ID == prior.ID {
							t.Fatalf("new cycle replayed interrupted session %s", session.ID)
						}
					}
				}
				tasks, err := store.List[model.Task](fixture.state, "task")
				if err != nil || len(tasks) != 1 || tasks[0].CycleID != next.ID || tasks[0].SourceRevision != newRevision || tasks[0].Status != model.StatusQueued {
					t.Fatalf("queued work did not come exclusively from the fresh plan: %+v, %v", tasks, err)
				}
				assertAdmissions(t, fixture.state, 2*fixture.cfg.PlanningAdmissionsRequired(), "due continuous operation runs a wholly new planning pass")
				assertNoOpenClients(t, fixture.script)
				t.Logf("retained interrupted cycle %s; fresh cycle %s/%d uses new grounding %s and new sessions", old.ID, next.ID, next.Number, newRevision)
			})
		}
	}
}
