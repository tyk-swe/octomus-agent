package engine

import (
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// StateView is the live application envelope around the store's bounded dashboard
// queries. Saved summaries stay owned by the store; activity and policy belong to
// the application. These views are response contracts, not durable records.
type StateView struct {
	store.Dashboard
	Capabilities     Capabilities             `json:"capabilities"`
	Status           string                   `json:"status"`
	RecoveryError    *string                  `json:"recovery_error"`
	Control          model.Control            `json:"control"`
	Repository       string                   `json:"repository"`
	DeliveryMode     config.DeliveryMode      `json:"delivery_mode"`
	AutoMerge        AutoMergeView            `json:"auto_merge"`
	Configured       bool                     `json:"configured"`
	AuditConfigured  bool                     `json:"audit_configured"`
	ActiveCycleMode  *model.CycleMode         `json:"active_cycle_mode"`
	ActiveTasks      int                      `json:"active_tasks"`
	CycleActive      bool                     `json:"cycle_active"`
	BaselineActive   bool                     `json:"baseline_active"`
	Baseline         *BaselineSummary         `json:"baseline"`
	Notifications    store.NotificationHealth `json:"notifications"`
	SessionLimit     uint64                   `json:"session_limit"`
	StorageLimit     uint64                   `json:"storage_limit"`
	Storage          *storageUsage            `json:"storage"`
	PlanningCapacity model.PlanningCapacity   `json:"planning_capacity"`
	PRCapacity       model.PRCapacity         `json:"pr_capacity"`
	PendingRequests  int64                    `json:"pending_requests"`
	Sandbox          SandboxPosture           `json:"sandbox"`
}

type AutoMergeView struct {
	Active bool             `json:"active"`
	Counts map[string]int64 `json:"counts"`
}

type BaselineSummary struct {
	ID             string               `json:"id"`
	Status         model.BaselineStatus `json:"status"`
	StartedAt      string               `json:"started_at"`
	CompletedAt    *string              `json:"completed_at"`
	Error          *string              `json:"error"`
	ConfigRevision string               `json:"config_revision"`
	ConfigMatches  bool                 `json:"config_matches"`
	RevisionStatus string               `json:"revision_status"`
}

type BaselineView struct {
	Check              *model.BaselineCheck            `json:"check"`
	Eligible           bool                            `json:"eligible"`
	Reason             *string                         `json:"reason"`
	ConfigMatches      *bool                           `json:"config_matches"`
	ConfigRevision     *string                         `json:"config_revision"`
	RevisionStatus     string                          `json:"revision_status"`
	DefaultObservation *model.DefaultBranchObservation `json:"default_observation"`
	Caveat             string                          `json:"caveat"`
}

type ControlResult struct {
	model.Control
	PlanningCapacity *model.PlanningCapacity `json:"planning_capacity,omitempty"`
}

// TaskDetail keeps the task's frozen policy separate from the live admission
// limits. Embedding preserves the established flat task response.
type TaskDetail struct {
	model.Task
	AllowedActions         []model.TaskAction  `json:"allowed_actions"`
	EffectiveAttemptPolicy model.AttemptPolicy `json:"effective_attempt_policy"`
	OperatingPolicy        OperatingPolicy     `json:"operating_policy"`
}

type OperatingPolicy struct {
	MaxSessionsPerDay uint64 `json:"max_sessions_per_day"`
	MaxWorkspaceBytes uint64 `json:"max_workspace_bytes"`
}

// The durable models have their own JSON methods. Flattening through a plain
// alias avoids promoting those methods and accidentally dropping response fields.
func (v TaskDetail) MarshalJSON() ([]byte, error) {
	type taskFields model.Task
	return wirejson.Record(struct {
		taskFields
		AllowedActions         []model.TaskAction  `json:"allowed_actions"`
		EffectiveAttemptPolicy model.AttemptPolicy `json:"effective_attempt_policy"`
		OperatingPolicy        OperatingPolicy     `json:"operating_policy"`
	}{wirejson.NormalizeRecord(taskFields(v.Task)), v.AllowedActions, v.EffectiveAttemptPolicy, v.OperatingPolicy})
}

func (v ControlResult) MarshalJSON() ([]byte, error) {
	type controlFields model.Control
	return wirejson.Marshal(struct {
		controlFields
		PlanningCapacity *model.PlanningCapacity `json:"planning_capacity,omitempty"`
	}{controlFields(v.Control), v.PlanningCapacity})
}

type DoctorResult struct {
	OK                 bool                 `json:"ok"`
	Mode               model.CycleMode      `json:"mode"`
	Models             []runner.Model       `json:"models"`
	Backends           []runner.Diagnostics `json:"backends"`
	Warnings           []string             `json:"warnings"`
	Message            string               `json:"message"`
	Sandbox            DoctorSandbox        `json:"sandbox"`
	CodexVersion       string               `json:"codex_version,omitempty"`
	TestedCodexVersion string               `json:"tested_codex_version,omitempty"`
}

type DoctorSandbox struct {
	Mode     string           `json:"mode"`
	SelfTest *SandboxSelfTest `json:"self_test,omitempty"`
}
