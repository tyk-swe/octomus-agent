package model

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func TestRepairProgressKeepsVersionSevenTasksLoadable(t *testing.T) {
	rounds := uint64(1)
	task := Task{Config: config.Default(), Status: StatusBlocked, RepairRounds: &rounds, RepairProgress: &RepairProgress{Revision: "reviewed", NoProgressRounds: 1, AwaitingReview: true}}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "repair_progress")
	delete(fields, "repair_rounds")
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Task
	if err := json.Unmarshal(legacy, &loaded); err != nil || loaded.RepairProgress != nil || loaded.RepairRounds != nil {
		t.Fatalf("pre-checkpoint version-7 task failed to load: progress=%+v error=%v", loaded.RepairProgress, err)
	}
	clone := task.Clone()
	if clone.RepairRounds == nil || *clone.RepairRounds != rounds {
		t.Fatalf("completed repair count did not survive serialization: %v", clone.RepairRounds)
	}
	*clone.RepairRounds++
	if *task.RepairRounds != 1 {
		t.Fatal("task clones share mutable repair counts")
	}
	if clone.RepairProgress == nil || *clone.RepairProgress != *task.RepairProgress {
		t.Fatalf("progress did not survive task serialization: %+v", clone.RepairProgress)
	}
	clone.RepairProgress.NoProgressRounds++
	if task.RepairProgress.NoProgressRounds != 1 {
		t.Fatal("task clones share mutable repair progress")
	}
}
