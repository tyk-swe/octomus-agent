package model

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type EventKind uint8

const (
	EventAttemptPolicy     EventKind = 0
	EventAutomerge         EventKind = 1
	EventBaseline          EventKind = 2
	EventBaselineError     EventKind = 3
	EventCleanupError      EventKind = 4
	EventConfiguration     EventKind = 5
	EventError             EventKind = 6
	EventGrounding         EventKind = 7
	EventHousekeepingError EventKind = 8
	EventInterrupted       EventKind = 9
	EventMaintenance       EventKind = 10
	EventOperator          EventKind = 11
	EventPlanningCapacity  EventKind = 12
	EventPlanningError     EventKind = 13
	EventRecovery          EventKind = 14
	EventRecoveryError     EventKind = 15
	EventRunComplete       EventKind = 16
	EventSandboxEvidence   EventKind = 17
	EventSessionProgress   EventKind = 18
	EventSessionStarted    EventKind = 19
	EventStatus            EventKind = 20
	EventUpgrade           EventKind = 21
	EventWorkerError       EventKind = 22
	EventRequest           EventKind = 23
)

var eventKindNames = []string{"attempt_policy", "automerge", "baseline", "baseline_error", "cleanup_error", "configuration", "error", "grounding", "housekeeping_error", "interrupted", "maintenance", "operator", "planning_capacity", "planning_error", "recovery", "recovery_error", "run_complete", "sandbox_evidence", "session_progress", "session_started", "status", "upgrade", "worker_error", "request"}

func (k EventKind) String() string               { return wirejson.EnumName(k, eventKindNames) }
func (k EventKind) MarshalText() ([]byte, error) { return wirejson.EnumText(k, eventKindNames) }
func (k *EventKind) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(k, text, eventKindNames)
}

// ErrorEventKinds lists the kinds that record a failure the operator should notice.
func ErrorEventKinds() []EventKind {
	return []EventKind{EventError, EventPlanningError, EventWorkerError, EventRecoveryError, EventCleanupError, EventHousekeepingError, EventBaselineError}
}
