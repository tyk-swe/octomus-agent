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

func (s Status) String() string                   { return wirejson.EnumName(s, statusNames) }
func (s Status) MarshalText() ([]byte, error)     { return wirejson.EnumText(s, statusNames) }
func (s *Status) UnmarshalText(text []byte) error { return wirejson.ParseEnum(s, text, statusNames) }

type BlockedReason uint8

const (
	BlockedBudgetExhausted      BlockedReason = 0
	BlockedStorageLimit         BlockedReason = 1
	BlockedStaleBase            BlockedReason = 2
	BlockedRemoteConflict       BlockedReason = 3
	BlockedPublicationUncertain BlockedReason = 4
	BlockedRunnerUnavailable    BlockedReason = 5
	BlockedInvalidReview        BlockedReason = 6
	BlockedVerificationFailed   BlockedReason = 7
	BlockedDependencyBlocked    BlockedReason = 8
	BlockedInvalidPlan          BlockedReason = 9
	BlockedWorkspaceInvalid     BlockedReason = 10
	BlockedRetryLimit           BlockedReason = 11
	BlockedTimeout              BlockedReason = 12
	BlockedUnknown              BlockedReason = 13
)

var blockedReasonNames = []string{"budget_exhausted", "storage_limit", "stale_base", "remote_conflict", "publication_uncertain", "runner_unavailable", "invalid_review", "verification_failed", "dependency_blocked", "invalid_plan", "workspace_invalid", "retry_limit", "timeout", "unknown"}

var blockedReasonMessages = [...]string{
	"Daily admission budget exhausted; adjust the current limit or wait until UTC midnight",
	"Storage admission limit reached; resolve retained workspaces or adjust the limit",
	"Source or default branch moved; supersede this task and rediscover against current context",
	"Remote branch moved outside recorded task outputs; reconcile the preserved work",
	"Publication result is uncertain; reconcile the preserved output commit",
	"Runner request failed; inspect the saved route and runner diagnostics",
	"Incomplete or invalid review cannot authorize publication",
	"Verification or repairs remain unresolved; evidence is preserved",
	"A dependency is unresolved; deliver it or rediscover dependent work",
	"The saved dependency plan cannot execute; rediscover a valid task order",
	"Workspace initialization or recorded evidence is inconsistent; preserve and inspect it",
	"Attempt or repair limit exhausted; inspect evidence before adjusting attempt limits",
	"Task time limit exceeded; inspect the preserved workspace",
	"Unclassified task failure; inspect the recorded diagnostics",
}

func (b BlockedReason) String() string               { return wirejson.EnumName(b, blockedReasonNames) }
func (b BlockedReason) MarshalText() ([]byte, error) { return wirejson.EnumText(b, blockedReasonNames) }
func (b *BlockedReason) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(b, text, blockedReasonNames)
}

type CapacityStatus uint8

const (
	CapacityReady     CapacityStatus = 0
	CapacityExhausted CapacityStatus = 1
	CapacityTooLow    CapacityStatus = 2
)

var planningCapacityStatusNames = []string{"ready", "daily_exhausted", "limit_too_low"}

func (v CapacityStatus) String() string {
	return wirejson.EnumName(v, planningCapacityStatusNames)
}
func (v CapacityStatus) MarshalText() ([]byte, error) {
	return wirejson.EnumText(v, planningCapacityStatusNames)
}
func (v *CapacityStatus) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(v, text, planningCapacityStatusNames)
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

func (v BaselineStatus) String() string { return wirejson.EnumName(v, baselineStatusNames) }
func (v BaselineStatus) MarshalText() ([]byte, error) {
	return wirejson.EnumText(v, baselineStatusNames)
}
func (v *BaselineStatus) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(v, text, baselineStatusNames)
}

type CycleStatus uint8

const (
	CycleRunning CycleStatus = iota
	CycleCompleted
	CycleIdle
	CycleFailed
	CycleInterrupted
)

var cycleStatusNames = []string{"running", "completed", "idle", "failed", "interrupted"}

func (v CycleStatus) String() string { return wirejson.EnumName(v, cycleStatusNames) }
func (v CycleStatus) MarshalText() ([]byte, error) {
	return wirejson.EnumText(v, cycleStatusNames)
}
func (v *CycleStatus) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(v, text, cycleStatusNames)
}

type SessionStatus uint8

const (
	SessionRunning SessionStatus = iota
	SessionCompleted
	SessionFailed
	SessionInterrupted
)

var sessionStatusNames = []string{"running", "completed", "failed", "interrupted"}

func (v SessionStatus) String() string { return wirejson.EnumName(v, sessionStatusNames) }
func (v SessionStatus) MarshalText() ([]byte, error) {
	return wirejson.EnumText(v, sessionStatusNames)
}
func (v *SessionStatus) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(v, text, sessionStatusNames)
}

type CycleMode uint8

const (
	CycleModeExecution CycleMode = 0
	CycleModeAudit     CycleMode = 1
)

var cycleModeNames = []string{"execution", "audit"}

func (v CycleMode) String() string               { return wirejson.EnumName(v, cycleModeNames) }
func (v CycleMode) MarshalText() ([]byte, error) { return wirejson.EnumText(v, cycleModeNames) }
func (v *CycleMode) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(v, text, cycleModeNames)
}

type OperatingMode uint8

const (
	OperatingModePaused     OperatingMode = 0
	OperatingModeRunOnce    OperatingMode = 1
	OperatingModeContinuous OperatingMode = 2
)

var operatingModeNames = []string{"paused", "run_once", "continuous"}

func (v OperatingMode) String() string               { return wirejson.EnumName(v, operatingModeNames) }
func (v OperatingMode) MarshalText() ([]byte, error) { return wirejson.EnumText(v, operatingModeNames) }
func (v *OperatingMode) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(v, text, operatingModeNames)
}

type BatchPhase uint8

const (
	BatchPhaseDraining  BatchPhase = 0
	BatchPhasePlanning  BatchPhase = 1
	BatchPhaseExecuting BatchPhase = 2
)

var batchPhaseNames = []string{"draining", "planning", "executing"}

func (v BatchPhase) String() string               { return wirejson.EnumName(v, batchPhaseNames) }
func (v BatchPhase) MarshalText() ([]byte, error) { return wirejson.EnumText(v, batchPhaseNames) }
func (v *BatchPhase) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(v, text, batchPhaseNames)
}
