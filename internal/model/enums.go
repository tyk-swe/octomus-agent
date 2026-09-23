package model

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type Status uint8

const (
	StatusQueued     Status = 0
	StatusExecuting  Status = 1
	StatusReviewing  Status = 2
	StatusRepairing  Status = 3
	StatusVerifying  Status = 4
	StatusPublishing Status = 5
	StatusPublished  Status = 6
	StatusBlocked    Status = 7
	StatusFailed     Status = 8
	StatusCancelled  Status = 9
)

var statusNames = []string{"queued", "executing", "reviewing", "repairing", "verifying", "publishing", "published", "blocked", "failed", "cancelled"}

func (v Status) String() string               { return wirejson.EnumName(uint8(v), statusNames) }
func (v Status) MarshalJSON() ([]byte, error) { return wirejson.MarshalEnum(uint8(v), statusNames) }
func (v *Status) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, statusNames)
	if err == nil {
		*v = Status(n)
	}
	return err
}

type BlockedReason uint8

const (
	BlockedReasonBudgetExhausted      BlockedReason = 0
	BlockedReasonStorageLimit         BlockedReason = 1
	BlockedReasonStaleBase            BlockedReason = 2
	BlockedReasonRemoteConflict       BlockedReason = 3
	BlockedReasonPublicationUncertain BlockedReason = 4
	BlockedReasonRunnerUnavailable    BlockedReason = 5
	BlockedReasonInvalidReview        BlockedReason = 6
	BlockedReasonVerificationFailed   BlockedReason = 7
	BlockedReasonDependencyBlocked    BlockedReason = 8
	BlockedReasonInvalidPlan          BlockedReason = 9
	BlockedReasonWorkspaceInvalid     BlockedReason = 10
	BlockedReasonRetryLimit           BlockedReason = 11
	BlockedReasonTimeout              BlockedReason = 12
	BlockedReasonUnknown              BlockedReason = 13
)

var blockedReasonNames = []string{"budget_exhausted", "storage_limit", "stale_base", "remote_conflict", "publication_uncertain", "runner_unavailable", "invalid_review", "verification_failed", "dependency_blocked", "invalid_plan", "workspace_invalid", "retry_limit", "timeout", "unknown"}

func (v BlockedReason) String() string { return wirejson.EnumName(uint8(v), blockedReasonNames) }
func (v BlockedReason) MarshalJSON() ([]byte, error) {
	return wirejson.MarshalEnum(uint8(v), blockedReasonNames)
}
func (v *BlockedReason) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, blockedReasonNames)
	if err == nil {
		*v = BlockedReason(n)
	}
	return err
}

type PlanningCapacityStatus uint8

const (
	PlanningCapacityStatusReady          PlanningCapacityStatus = 0
	PlanningCapacityStatusDailyExhausted PlanningCapacityStatus = 1
	PlanningCapacityStatusLimitTooLow    PlanningCapacityStatus = 2
)

var planningCapacityStatusNames = []string{"ready", "daily_exhausted", "limit_too_low"}

func (v PlanningCapacityStatus) String() string {
	return wirejson.EnumName(uint8(v), planningCapacityStatusNames)
}
func (v PlanningCapacityStatus) MarshalJSON() ([]byte, error) {
	return wirejson.MarshalEnum(uint8(v), planningCapacityStatusNames)
}
func (v *PlanningCapacityStatus) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, planningCapacityStatusNames)
	if err == nil {
		*v = PlanningCapacityStatus(n)
	}
	return err
}

type BaselineStatus uint8

const (
	BaselineStatusRunning     BaselineStatus = 0
	BaselineStatusPassed      BaselineStatus = 1
	BaselineStatusFailed      BaselineStatus = 2
	BaselineStatusCancelled   BaselineStatus = 3
	BaselineStatusTimedOut    BaselineStatus = 4
	BaselineStatusInterrupted BaselineStatus = 5
)

var baselineStatusNames = []string{"running", "passed", "failed", "cancelled", "timed_out", "interrupted"}

func (v BaselineStatus) String() string { return wirejson.EnumName(uint8(v), baselineStatusNames) }
func (v BaselineStatus) MarshalJSON() ([]byte, error) {
	return wirejson.MarshalEnum(uint8(v), baselineStatusNames)
}
func (v *BaselineStatus) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, baselineStatusNames)
	if err == nil {
		*v = BaselineStatus(n)
	}
	return err
}

type CycleMode uint8

const (
	CycleModeExecution CycleMode = 0
	CycleModeAudit     CycleMode = 1
)

var cycleModeNames = []string{"execution", "audit"}

func (v CycleMode) String() string { return wirejson.EnumName(uint8(v), cycleModeNames) }
func (v CycleMode) MarshalJSON() ([]byte, error) {
	return wirejson.MarshalEnum(uint8(v), cycleModeNames)
}
func (v *CycleMode) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, cycleModeNames)
	if err == nil {
		*v = CycleMode(n)
	}
	return err
}

type OperatingMode uint8

const (
	OperatingModePaused     OperatingMode = 0
	OperatingModeRunOnce    OperatingMode = 1
	OperatingModeContinuous OperatingMode = 2
)

var operatingModeNames = []string{"paused", "run_once", "continuous"}

func (v OperatingMode) String() string { return wirejson.EnumName(uint8(v), operatingModeNames) }
func (v OperatingMode) MarshalJSON() ([]byte, error) {
	return wirejson.MarshalEnum(uint8(v), operatingModeNames)
}
func (v *OperatingMode) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, operatingModeNames)
	if err == nil {
		*v = OperatingMode(n)
	}
	return err
}

type BatchPhase uint8

const (
	BatchPhaseDraining  BatchPhase = 0
	BatchPhasePlanning  BatchPhase = 1
	BatchPhaseExecuting BatchPhase = 2
)

var batchPhaseNames = []string{"draining", "planning", "executing"}

func (v BatchPhase) String() string { return wirejson.EnumName(uint8(v), batchPhaseNames) }
func (v BatchPhase) MarshalJSON() ([]byte, error) {
	return wirejson.MarshalEnum(uint8(v), batchPhaseNames)
}
func (v *BatchPhase) UnmarshalJSON(data []byte) error {
	n, err := wirejson.Enum(data, batchPhaseNames)
	if err == nil {
		*v = BatchPhase(n)
	}
	return err
}
