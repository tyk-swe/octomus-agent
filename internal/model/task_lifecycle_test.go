package model

import (
	"errors"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func lifecycleTask() Task {
	return Task{
		ID: "task", Status: StatusExecuting, Config: config.Default(),
		RepairRounds: new(uint64(0)),
		Sessions:     []Session{NewSession("executor", "executor", config.NewRoute("fixture", "low"))},
	}
}

func TestTaskPolicyRejectsCrossOperationFields(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"policy", "attempts", "output", "source", "workspace", "archive", "lineage", "session", "reviews", "first-turn"} {
		t.Run(field, func(t *testing.T) {
			current := lifecycleTask()
			current.Reviews = []ReviewRound{{Revision: "prior"}}
			current.Sessions[0].FirstTurnStarted = new(true)
			next := current.Clone()
			switch field {
			case "policy":
				next.AttemptPolicy = new(PolicyOf(current.Config))
			case "attempts":
				next.Attempts++
			case "output":
				next.OutputCommit = new("unauthorized")
			case "source":
				next.SourceRevision = "different"
			case "workspace":
				next.Workspace = "/different/workspace"
			case "archive":
				next.Lifecycle.ArchivedAt = new(Now())
			case "lineage":
				next.SupersededBy = []string{"another-task"}
			case "session":
				next.Sessions[0].ID = "different-session"
			case "reviews":
				next.Reviews = nil
			case "first-turn":
				next.Sessions[0].FirstTurnStarted = new(false)
			}
			if err := ValidateTaskChange(current, next, TaskCheckpoint, false); err == nil {
				t.Fatalf("worker replaced %s", field)
			}
		})
	}
}

func TestTaskPolicyCancellationAndOutputBoundary(t *testing.T) {
	t.Parallel()
	current := lifecycleTask()
	next := current.Clone()
	if err := ValidateTaskChange(current, next, TaskCheckpoint, true); !errors.Is(err, ErrTaskCancelled) {
		t.Fatalf("cancelled checkpoint: %v", err)
	}
	next.Status = StatusQueued
	if err := ValidateTaskChange(current, next, TaskRecovery, true); err == nil {
		t.Fatal("restart recovery ignored cancellation")
	}
	next.Status = StatusCancelled
	if err := ValidateTaskChange(current, next, TaskRecovery, true); err != nil {
		t.Fatal(err)
	}
	current.Status, current.OutputCommit = StatusPublishing, new("authorized")
	next = current.Clone()
	next.Status = StatusBlocked
	if err := ValidateTaskChange(current, next, TaskSettlement, true); err != nil {
		t.Fatalf("publication uncertainty was cancelled: %v", err)
	}
	next.OutputCommit = new("different")
	if err := ValidateTaskChange(current, next, TaskSettlement, false); err == nil {
		t.Fatal("settlement replaced the output checkpoint")
	}
}

func TestTaskPolicyRequiresRepairAccountingWithEvidence(t *testing.T) {
	t.Parallel()
	current := lifecycleTask()
	current.Status, current.Sessions[0].Role = StatusRepairing, "repair"
	next := current.Clone()
	next.Sessions[0].MarkCompleted("Completed repair")
	if err := ValidateTaskChange(current, next, TaskCheckpoint, false); err == nil {
		t.Fatal("repair completion committed without its accounting")
	}
	next.RepairRounds = new(uint64(1))
	if err := ValidateTaskChange(current, next, TaskCheckpoint, false); err != nil {
		t.Fatal(err)
	}
	current = next.Clone()
	if err := ValidateTaskChange(current, next, TaskCheckpoint, false); err != nil {
		t.Fatalf("unchanged completed evidence was charged again: %v", err)
	}
	next.RepairRounds = new(uint64(2))
	if err := ValidateTaskChange(current, next, TaskCheckpoint, false); err == nil {
		t.Fatal("repair was charged twice")
	}
}
