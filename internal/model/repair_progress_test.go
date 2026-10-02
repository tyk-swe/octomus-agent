package model

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func TestRepairProgressKeepsVersionSevenTasksLoadable(t *testing.T) {
	task := Task{Config: config.Default(), Status: StatusBlocked, RepairProgress: &RepairProgress{Revision: "reviewed", NoProgressRounds: 1, AwaitingReview: true}}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "repair_progress")
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Task
	if err := json.Unmarshal(legacy, &loaded); err != nil || loaded.RepairProgress != nil {
		t.Fatalf("pre-checkpoint version-7 task failed to load: progress=%+v error=%v", loaded.RepairProgress, err)
	}
	clone := task.Clone()
	if clone.RepairProgress == nil || *clone.RepairProgress != *task.RepairProgress {
		t.Fatalf("progress did not survive task serialization: %+v", clone.RepairProgress)
	}
	clone.RepairProgress.NoProgressRounds++
	if task.RepairProgress.NoProgressRounds != 1 {
		t.Fatal("task clones share mutable repair progress")
	}
}
