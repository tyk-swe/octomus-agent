// Wire records use the current Go JSON field names and request defaults.
package config

import (
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type Route struct {
	Backend  Backend `json:"backend" wire:"default"`
	Model    string  `json:"model"`
	Effort   string  `json:"effort" wire:"default"`
	Provider *string `json:"provider,omitempty" wire:"default"`
	Variant  *string `json:"variant,omitempty" wire:"default"`
}

func (v *Route) UnmarshalJSON(data []byte) error { return wirejson.DecodeStrict(data, v) }
func (v Route) MarshalJSON() ([]byte, error)     { type plain Route; return wirejson.Record(plain(v)) }
func (v Route) Clone() Route                     { return wirejson.Clone(v) }

type Config struct {
	Repository             string            `json:"repository"`
	GitHubRepo             string            `json:"github_repo"`
	DefaultBranch          string            `json:"default_branch"`
	BranchPrefix           string            `json:"branch_prefix"`
	CodexBinary            string            `json:"codex_binary"`
	OpencodeBinary         string            `json:"opencode_binary"`
	Roles                  map[string]Route  `json:"roles"`
	Tiers                  map[string]Route  `json:"tiers"`
	RepairRoute            Route             `json:"repair_route"`
	Categories             []string          `json:"categories"`
	VerificationCommands   []string          `json:"verification_commands"`
	DiscoveryAgents        uint64            `json:"discovery_agents"`
	ExecutionConcurrency   uint64            `json:"execution_concurrency"`
	CycleIntervalSeconds   uint64            `json:"cycle_interval_seconds"`
	MaintenanceEveryCycles uint64            `json:"maintenance_every_cycles"`
	LargePRLines           uint64            `json:"large_pr_lines"`
	LongLivedPRDays        uint64            `json:"long_lived_pr_days"`
	MaxTasksPerCycle       uint64            `json:"max_tasks_per_cycle"`
	MaxRepairRounds        uint64            `json:"max_repair_rounds"`
	MaxNoProgressRounds    uint64            `json:"max_no_progress_rounds"`
	MaxRetries             uint64            `json:"max_retries"`
	SessionTimeoutSeconds  uint64            `json:"session_timeout_seconds"`
	TaskTimeoutSeconds     uint64            `json:"task_timeout_seconds"`
	CommandTimeoutSeconds  uint64            `json:"command_timeout_seconds"`
	MaxSessionsPerDay      uint64            `json:"max_sessions_per_day"`
	MaxOpenPRs             uint64            `json:"max_open_prs"`
	MaxWorkspaceBytes      uint64            `json:"max_workspace_bytes"`
	RunnerStoragePaths     map[string]string `json:"runner_storage_paths"`
	RetainCompletedDays    uint64            `json:"retain_completed_days"`
	RetainEvents           uint64            `json:"retain_events"`
}

// UnmarshalJSON decodes strictly; absent fields take their Default() values.
func (v *Config) UnmarshalJSON(data []byte) error {
	return wirejson.DecodeWithDefaults(data, v, Default())
}
func (v Config) MarshalJSON() ([]byte, error) { type plain Config; return wirejson.Record(plain(v)) }
func (v Config) Clone() Config                { return wirejson.Clone(v) }
