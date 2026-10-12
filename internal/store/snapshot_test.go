package store_test

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestTypedSnapshotKeepsCycleAndTaskEvidenceTogether(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	task := task()
	cycle := cycleFor(task)
	task.CycleID = cycle.ID
	cycle.Assessments = []any{"historical malformed assessment"}
	must(t, store.Put(s, store.Cycles.Key(cycle.ID), cycle))
	must(t, store.Put(s, store.Tasks.Key(task.ID), task))
	r, err := store.OpenReadOnly(path, "snapshot test")
	must(t, err)
	defer r.Close()
	writer := raw(t, path)
	must(t, r.Snapshot(func(snapshot *store.Snapshot) error {
		before, err := snapshot.Cycle(cycle.ID)
		if err != nil {
			return err
		}
		if before == nil || before.Assessments[0] != "historical malformed assessment" {
			t.Fatal("typed reading erased raw historical assessment evidence")
		}
		tx, err := writer.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec("UPDATE records SET data=json_set(data,'$.proposal.title','After snapshot') WHERE kind='task' AND id=?1", task.ID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		tasks, err := snapshot.TasksForCycle(cycle.ID)
		if err != nil {
			return err
		}
		if len(tasks) != 1 || tasks[0].Proposal.Title != task.Proposal.Title {
			t.Fatalf("cycle and task reads crossed snapshots: %+v", tasks)
		}
		return nil
	}))
	must(t, r.Snapshot(func(snapshot *store.Snapshot) error {
		tasks, err := snapshot.TasksForCycle(cycle.ID)
		if err == nil && (len(tasks) != 1 || tasks[0].Proposal.Title != "After snapshot") {
			t.Fatalf("a new snapshot did not see the committed update: %+v", tasks)
		}
		return err
	}))
}

func TestSnapshotSelectsExactEvidenceBeforeDecoding(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	task := task()
	must(t, store.Put(s, store.Tasks.Key(task.ID), task))
	observation := model.PRObservation{
		Repository: "Fixture/Project", PR: model.PullRequest{Number: 5, State: "open"},
	}
	must(t, store.Put(s, store.PRs.Key("fixture/project:5"), observation))
	db := raw(t, path)
	// These unrelated records cannot decode as their domain types. Bounded
	// selectors must exclude them before touching full historical evidence.
	exec(t, db, `INSERT INTO records VALUES ('task','unrelated-task','{"cycle_id":"other"}')`)
	exec(t, db, `INSERT INTO records VALUES ('pr','unrelated-pr','{"repository":"fixture/project","pr":{"number":6}}')`)
	must(t, s.Snapshot(func(snapshot *store.Snapshot) error {
		tasks, err := snapshot.TasksForCycle(task.CycleID)
		if err != nil {
			return err
		}
		if len(tasks) != 1 || tasks[0].ID != task.ID {
			t.Fatalf("cycle selector returned unrelated evidence: %+v", tasks)
		}
		observations, err := snapshot.PRObservations("fixture/PROJECT", 5)
		if err != nil {
			return err
		}
		if len(observations) != 1 || observations[0].PR.Number != 5 {
			t.Fatalf("PR selector returned unrelated evidence: %+v", observations)
		}
		return nil
	}))
	for _, id := range []string{"duplicate-a", "duplicate-b"} {
		must(t, store.Put(s, store.PRs.Key(id), observation))
	}
	must(t, s.Snapshot(func(snapshot *store.Snapshot) error {
		observations, err := snapshot.PRObservations("fixture/project", 5)
		if err == nil && len(observations) != 2 {
			t.Fatalf("ambiguous PR identity was not retained within the bound: %d", len(observations))
		}
		return err
	}))
}
