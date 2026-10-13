package engine_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func TestComposedResponseJSONPreservesFlatFieldsAndRecordSemantics(t *testing.T) {
	t.Parallel()
	detail := engine.TaskDetail{Task: model.Task{ID: "task-1"}, OperatingPolicy: engine.OperatingPolicy{MaxSessionsPerDay: 7}}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var task map[string]json.RawMessage
	if err := json.Unmarshal(data, &task); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reviews", "verification", "allowed_actions"} {
		if string(task[name]) != "[]" {
			t.Fatalf("%s=%s; want []", name, task[name])
		}
	}
	if string(task["id"]) != `"task-1"` || task["effective_attempt_policy"] == nil || task["operating_policy"] == nil {
		t.Fatalf("missing flat response fields: %s", data)
	}
	if detail.Task.Reviews != nil || detail.AllowedActions != nil {
		t.Fatal("serializing mutated the owned record")
	}
	var reply map[string]json.RawMessage
	data, err = json.Marshal(engine.ControlResult{Control: model.Control{Paused: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	if string(reply["paused"]) != "true" || string(reply["mode"]) != `"paused"` || reply["planning_capacity"] != nil {
		t.Fatalf("control envelope=%s", data)
	}
	capacity := model.PlanningCapacity{Status: model.CapacityReady}
	data, err = json.Marshal(engine.ControlResult{Control: model.Control{Paused: true}, PlanningCapacity: &capacity})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["planning_capacity"] == nil {
		t.Fatalf("capacity lost by promoted serializer: %s", data)
	}
	data, err = json.Marshal(engine.DoctorResult{Sandbox: engine.DoctorSandbox{Mode: "docker"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &reply); err != nil {
		t.Fatal(err)
	}
	var sandbox map[string]json.RawMessage
	if err = json.Unmarshal(reply["sandbox"], &sandbox); err != nil {
		t.Fatal(err)
	}
	if string(sandbox["mode"]) != `"docker"` {
		t.Fatalf("sandbox mode=%s", sandbox["mode"])
	}
}
