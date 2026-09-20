package model

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/jsoncompat"
)

type wireFixture struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Input    string         `json:"input"`
	Expected map[string]any `json:"expected"`
}

func TestFrozenWireContracts(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/compatibility/m1.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []wireFixture `json:"cases"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, c := range corpus.Cases {
		var target any
		switch c.Type {
		case "Proposal":
			target = new(Proposal)
		case "PlanningCapacity":
			target = new(PlanningCapacity)
		case "AttemptPolicy":
			target = new(AttemptPolicy)
		case "WorkspaceLifecycle":
			target = new(WorkspaceLifecycle)
		case "Finding":
			target = new(Finding)
		case "Review":
			target = new(Review)
		case "ReviewRound":
			target = new(ReviewRound)
		case "Verification":
			target = new(Verification)
		case "BaselineCommand":
			target = new(BaselineCommand)
		case "BaselineCheck":
			target = new(BaselineCheck)
		case "DefaultBranchObservation":
			target = new(DefaultBranchObservation)
		case "Session":
			target = new(Session)
		case "Task":
			target = new(Task)
		case "PullRequest":
			target = new(PullRequest)
		case "PrObservation":
			target = new(PrObservation)
		case "Grounding":
			target = new(Grounding)
		case "ExternalPrContext":
			target = new(ExternalPrContext)
		case "PrCoverage":
			target = new(PrCoverage)
		case "OpenPrInventory":
			target = new(OpenPrInventory)
		case "PrCapacity":
			target = new(PrCapacity)
		case "Cycle":
			target = new(Cycle)
		case "RunBatch":
			target = new(RunBatch)
		case "Control":
			target = new(Control)
		case "Event":
			target = new(Event)
		case "Status":
			target = new(Status)
		case "BlockedReason":
			target = new(BlockedReason)
		case "PlanningCapacityStatus":
			target = new(PlanningCapacityStatus)
		case "BaselineStatus":
			target = new(BaselineStatus)
		case "CycleMode":
			target = new(CycleMode)
		case "OperatingMode":
			target = new(OperatingMode)
		case "BatchPhase":
			target = new(BatchPhase)
		default:
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			err := json.Unmarshal([]byte(c.Input), target)
			if c.Expected["rejected"] == true {
				if err == nil {
					t.Fatalf("accepted invalid input: %s", c.Input)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected reference input: %v; %s", err, c.Input)
			}
			output, err := jsoncompat.Marshal(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(output) != c.Expected["json"] {
				t.Errorf("wire bytes differ\n Go: %s\nRust: %s", output, c.Expected["json"])
			}

			switch v := target.(type) {
			case *Task:
				actions, _ := json.Marshal(v.AllowedActions())
				want, _ := json.Marshal(c.Expected["actions"])
				if string(actions) != string(want) {
					t.Errorf("actions %s != %s", actions, want)
				}
				if float64(v.AttemptReviews()) != c.Expected["attempt_reviews"] {
					t.Errorf("review count: %d", v.AttemptReviews())
				}
				snapshot, err := jsoncompat.Marshal(v.ExecutionConfig())
				if err != nil {
					t.Fatal(err)
				}
				if string(snapshot) != c.Expected["execution_config"] {
					t.Errorf("execution config: %s", snapshot)
				}
			case *Review:
				if v.Clean() != c.Expected["clean"] || v.Valid() != c.Expected["valid"] {
					t.Error("review validity changed")
				}
			}
		})
	}
}
