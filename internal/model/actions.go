package model

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type TaskAction uint8

const (
	TaskActionCancel    TaskAction = 0
	TaskActionRetry     TaskAction = 1
	TaskActionSupersede TaskAction = 2
	TaskActionArchive   TaskAction = 3
	TaskActionDiscard   TaskAction = 4
	TaskActionReconcile TaskAction = 5
)

var taskActionNames = []string{"cancel", "retry", "supersede", "archive", "discard", "reconcile"}

func (a TaskAction) String() string               { return wirejson.EnumName(a, taskActionNames) }
func (a TaskAction) MarshalText() ([]byte, error) { return wirejson.EnumText(a, taskActionNames) }
func (a *TaskAction) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(a, text, taskActionNames)
}

type CycleAction uint8

const (
	CycleActionArchive CycleAction = 0
	CycleActionDiscard CycleAction = 1
)

var cycleActionNames = []string{"archive", "discard"}

func (a CycleAction) String() string               { return wirejson.EnumName(a, cycleActionNames) }
func (a CycleAction) MarshalText() ([]byte, error) { return wirejson.EnumText(a, cycleActionNames) }
func (a *CycleAction) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(a, text, cycleActionNames)
}

type ControlAction uint8

const (
	ControlPause  ControlAction = 0
	ControlResume ControlAction = 1
	ControlCycle  ControlAction = 2
	ControlAudit  ControlAction = 3
)

var controlActionNames = []string{"pause", "resume", "cycle", "audit"}

func (a ControlAction) String() string               { return wirejson.EnumName(a, controlActionNames) }
func (a ControlAction) MarshalText() ([]byte, error) { return wirejson.EnumText(a, controlActionNames) }
func (a *ControlAction) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(a, text, controlActionNames)
}

// CycleActions selects persisted lifecycle eligibility. The service separately
// revalidates worker and cleanup ownership when the operator action arrives.
func CycleActions(status CycleStatus, lifecycle WorkspaceLifecycle) []CycleAction {
	if status == CycleRunning || lifecycle.DiscardedAt != nil {
		return []CycleAction{}
	}
	if lifecycle.ArchivedAt == nil {
		return []CycleAction{CycleActionArchive}
	}
	return []CycleAction{CycleActionDiscard}
}
