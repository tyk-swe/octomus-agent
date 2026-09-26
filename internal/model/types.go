// Wire records use the current Go JSON field names and request defaults.
package model

import (
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type Proposal struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Problem       string   `json:"problem"`
	Evidence      []string `json:"evidence"`
	Benefit       string   `json:"benefit"`
	Category      string   `json:"category"`
	Target        string   `json:"target"`
	Tier          string   `json:"tier"`
	Scope         string   `json:"scope"`
	Dependencies  []string `json:"dependencies"`
	Prompt        string   `json:"prompt"`
	Decision      string   `json:"decision"`
	Reason        string   `json:"reason"`
	ProblemKey    string   `json:"problem_key"`
	RelevantPaths []string `json:"relevant_paths"`
	Reconsiders   []string `json:"reconsiders"`
}

func (v *Proposal) UnmarshalJSON(data []byte) error {
	type plain Proposal
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, true, false); err != nil {
		return err
	}
	*v = Proposal(decoded)
	return nil
}
func (v Proposal) MarshalJSON() ([]byte, error) {
	type plain Proposal
	return wirejson.Record(plain(v))
}
func (v Proposal) Clone() Proposal { return wirejson.Clone(v) }

type PlanningCapacity struct {
	Day         string                 `json:"day"`
	Limit       uint64                 `json:"limit"`
	Used        uint64                 `json:"used"`
	Remaining   uint64                 `json:"remaining"`
	Required    uint64                 `json:"required"`
	NextResetAt int64                  `json:"next_reset_at"`
	Status      PlanningCapacityStatus `json:"status"`
}

func (v *PlanningCapacity) UnmarshalJSON(data []byte) error {
	type plain PlanningCapacity
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = PlanningCapacity(decoded)
	return nil
}
func (v PlanningCapacity) MarshalJSON() ([]byte, error) {
	type plain PlanningCapacity
	return wirejson.Record(plain(v))
}

type AttemptPolicy struct {
	MaxRepairRounds       uint64 `json:"max_repair_rounds"`
	MaxNoProgressRounds   uint64 `json:"max_no_progress_rounds"`
	MaxRetries            uint64 `json:"max_retries"`
	TaskTimeoutSeconds    uint64 `json:"task_timeout_seconds"`
	SessionTimeoutSeconds uint64 `json:"session_timeout_seconds"`
	CommandTimeoutSeconds uint64 `json:"command_timeout_seconds"`
}

func (v *AttemptPolicy) UnmarshalJSON(data []byte) error {
	type plain AttemptPolicy
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = AttemptPolicy(decoded)
	return nil
}
func (v AttemptPolicy) MarshalJSON() ([]byte, error) {
	type plain AttemptPolicy
	return wirejson.Record(plain(v))
}

type WorkspaceLifecycle struct {
	ArchivedAt  *string `json:"archived_at"`
	DiscardedAt *string `json:"discarded_at"`
}

func (v *WorkspaceLifecycle) UnmarshalJSON(data []byte) error {
	type plain WorkspaceLifecycle
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = WorkspaceLifecycle(decoded)
	return nil
}
func (v WorkspaceLifecycle) MarshalJSON() ([]byte, error) {
	type plain WorkspaceLifecycle
	return wirejson.Record(plain(v))
}

type Finding struct {
	Title    string `json:"title"`
	File     string `json:"file"`
	Detail   string `json:"detail"`
	Priority string `json:"priority"`
}

func (v *Finding) UnmarshalJSON(data []byte) error {
	type plain Finding
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, true, false); err != nil {
		return err
	}
	*v = Finding(decoded)
	return nil
}
func (v Finding) MarshalJSON() ([]byte, error) {
	type plain Finding
	return wirejson.Record(plain(v))
}

type Review struct {
	Completed bool      `json:"completed"`
	Summary   string    `json:"summary"`
	Findings  []Finding `json:"findings"`
}

func (v *Review) UnmarshalJSON(data []byte) error {
	type plain Review
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, true, false); err != nil {
		return err
	}
	*v = Review(decoded)
	return nil
}
func (v Review) MarshalJSON() ([]byte, error) { type plain Review; return wirejson.Record(plain(v)) }

type ReviewRound struct {
	SessionID      string `json:"session_id"`
	Revision       string `json:"revision"`
	ComparisonBase string `json:"comparison_base"`
	Result         Review `json:"result"`
	CreatedAt      string `json:"created_at"`
}

func (v *ReviewRound) UnmarshalJSON(data []byte) error {
	type plain ReviewRound
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = ReviewRound(decoded)
	return nil
}
func (v ReviewRound) MarshalJSON() ([]byte, error) {
	type plain ReviewRound
	return wirejson.Record(plain(v))
}

type Verification struct {
	Command   string `json:"command"`
	Success   bool   `json:"success"`
	Output    string `json:"output"`
	Revision  string `json:"revision"`
	CreatedAt string `json:"created_at"`
}

func (v *Verification) UnmarshalJSON(data []byte) error {
	type plain Verification
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = Verification(decoded)
	return nil
}
func (v Verification) MarshalJSON() ([]byte, error) {
	type plain Verification
	return wirejson.Record(plain(v))
}

type BaselineCommand struct {
	Command         string `json:"command"`
	Success         bool   `json:"success"`
	Output          string `json:"output"`
	OutputTruncated bool   `json:"output_truncated"`
	CreatedAt       string `json:"created_at"`
}

func (v *BaselineCommand) UnmarshalJSON(data []byte) error {
	type plain BaselineCommand
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = BaselineCommand(decoded)
	return nil
}
func (v BaselineCommand) MarshalJSON() ([]byte, error) {
	type plain BaselineCommand
	return wirejson.Record(plain(v))
}

type BaselineCheck struct {
	ID                string            `json:"id"`
	Status            BaselineStatus    `json:"status"`
	Config            config.Config     `json:"config"`
	ConfigFingerprint string            `json:"config_fingerprint"`
	Revision          *string           `json:"revision"`
	StartedAt         string            `json:"started_at"`
	CompletedAt       *string           `json:"completed_at"`
	Commands          []BaselineCommand `json:"commands"`
	Error             *string           `json:"error"`
	WorkspaceRemoved  bool              `json:"workspace_removed"`
	CleanupError      *string           `json:"cleanup_error"`
}

func (v *BaselineCheck) UnmarshalJSON(data []byte) error {
	type plain BaselineCheck
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = BaselineCheck(decoded)
	return nil
}
func (v BaselineCheck) MarshalJSON() ([]byte, error) {
	type plain BaselineCheck
	return wirejson.Record(plain(v))
}

type DefaultBranchObservation struct {
	Repository    string `json:"repository"`
	DefaultBranch string `json:"default_branch"`
	Revision      string `json:"revision"`
	ObservedAt    string `json:"observed_at"`
}

func (v *DefaultBranchObservation) UnmarshalJSON(data []byte) error {
	type plain DefaultBranchObservation
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = DefaultBranchObservation(decoded)
	return nil
}
func (v DefaultBranchObservation) MarshalJSON() ([]byte, error) {
	type plain DefaultBranchObservation
	return wirejson.Record(plain(v))
}

type Session struct {
	ID        string       `json:"id"`
	Role      string       `json:"role"`
	Route     config.Route `json:"route"`
	Status    string       `json:"status"`
	StartedAt string       `json:"started_at"`
	Summary   string       `json:"summary"`
}

func (v *Session) UnmarshalJSON(data []byte) error {
	type plain Session
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = Session(decoded)
	return nil
}
func (v Session) MarshalJSON() ([]byte, error) {
	type plain Session
	return wirejson.Record(plain(v))
}
func (v Session) Clone() Session { return wirejson.Clone(v) }

type Task struct {
	ID                   string             `json:"id"`
	CycleID              string             `json:"cycle_id"`
	Proposal             Proposal           `json:"proposal"`
	Status               Status             `json:"status"`
	Route                config.Route       `json:"route"`
	Config               config.Config      `json:"config"`
	SourceRevision       string             `json:"source_revision"`
	ComparisonBase       string             `json:"comparison_base"`
	DefaultRevision      string             `json:"default_revision"`
	Branch               string             `json:"branch"`
	Workspace            string             `json:"workspace"`
	ExecutionSession     *string            `json:"execution_session"`
	RepairSession        *string            `json:"repair_session"`
	Sessions             []Session          `json:"sessions"`
	Reviews              []ReviewRound      `json:"reviews"`
	Verification         []Verification     `json:"verification"`
	OutputCommit         *string            `json:"output_commit"`
	PRNumber             *uint64            `json:"pr_number"`
	PRURL                *string            `json:"pr_url"`
	Attempts             uint64             `json:"attempts"`
	Error                *string            `json:"error"`
	CreatedAt            string             `json:"created_at"`
	UpdatedAt            string             `json:"updated_at"`
	AttemptPolicy        *AttemptPolicy     `json:"attempt_policy"`
	ReviewBaseline       uint64             `json:"review_baseline"`
	BlockedReason        *BlockedReason     `json:"blocked_reason"`
	RunID                *string            `json:"run_id"`
	SupersededBy         []string           `json:"superseded_by"`
	Supersedes           []string           `json:"supersedes"`
	RediscoveryRequested bool               `json:"rediscovery_requested"`
	RediscoveryResult    *string            `json:"rediscovery_result"`
	Lifecycle            WorkspaceLifecycle `json:"lifecycle"`
}

func (v *Task) UnmarshalJSON(data []byte) error {
	type plain Task
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = Task(decoded)
	return nil
}
func (v Task) MarshalJSON() ([]byte, error) { type plain Task; return wirejson.Record(plain(v)) }
func (v Task) Clone() Task                  { return wirejson.Clone(v) }

type PullRequest struct {
	Number         uint64 `json:"number"`
	Title          string `json:"title"`
	Branch         string `json:"branch"`
	Head           string `json:"head"`
	Base           string `json:"base"`
	URL            string `json:"url"`
	Body           string `json:"body"`
	State          string `json:"state"`
	ChangedLines   uint64 `json:"changed_lines"`
	CreatedAt      string `json:"created_at"`
	Owned          bool   `json:"owned"`
	HeadRepository string `json:"head_repository"`
	BaseRepository string `json:"base_repository"`
}

func (v *PullRequest) UnmarshalJSON(data []byte) error {
	type plain PullRequest
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = PullRequest(decoded)
	return nil
}
func (v PullRequest) MarshalJSON() ([]byte, error) {
	type plain PullRequest
	return wirejson.Record(plain(v))
}
func (v PullRequest) Clone() PullRequest { return wirejson.Clone(v) }

type PrObservation struct {
	Repository           string      `json:"repository"`
	PR                   PullRequest `json:"pr"`
	ObservedAt           string      `json:"observed_at"`
	DeliveredHead        *string     `json:"delivered_head"`
	ExternalHeadMovement bool        `json:"external_head_movement"`
}

func (v *PrObservation) UnmarshalJSON(data []byte) error {
	type plain PrObservation
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = PrObservation(decoded)
	return nil
}
func (v PrObservation) MarshalJSON() ([]byte, error) {
	type plain PrObservation
	return wirejson.Record(plain(v))
}

type Grounding struct {
	Revision           string              `json:"revision"`
	PRs                []PullRequest       `json:"prs"`
	ExternalPRs        []ExternalPrContext `json:"external_prs"`
	PRCoverage         PrCoverage          `json:"pr_coverage"`
	History            any                 `json:"history"`
	MaintenanceDue     bool                `json:"maintenance_due"`
	MaintenanceTargets []string            `json:"maintenance_targets"`
}

func (v *Grounding) UnmarshalJSON(data []byte) error {
	type plain Grounding
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = Grounding(decoded)
	return nil
}
func (v Grounding) MarshalJSON() ([]byte, error) {
	type plain Grounding
	return wirejson.Record(plain(v))
}

type ExternalPrContext struct {
	Number         uint64 `json:"number"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Branch         string `json:"branch"`
	Head           string `json:"head"`
	Base           string `json:"base"`
	HeadRepository string `json:"head_repository"`
	BaseRepository string `json:"base_repository"`
	TitleTruncated bool   `json:"title_truncated"`
	BodyTruncated  bool   `json:"body_truncated"`
}

func (v *ExternalPrContext) UnmarshalJSON(data []byte) error {
	type plain ExternalPrContext
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = ExternalPrContext(decoded)
	return nil
}
func (v ExternalPrContext) MarshalJSON() ([]byte, error) {
	type plain ExternalPrContext
	return wirejson.Record(plain(v))
}

type PrCoverage struct {
	ObservedAt       *string `json:"observed_at"`
	Complete         bool    `json:"complete"`
	TotalOpen        uint64  `json:"total_open"`
	TotalExternal    uint64  `json:"total_external"`
	IncludedExternal uint64  `json:"included_external"`
	OmittedExternal  uint64  `json:"omitted_external"`
	MaxExternal      uint64  `json:"max_external"`
	MaxTitleChars    uint64  `json:"max_title_chars"`
	MaxBodyChars     uint64  `json:"max_body_chars"`
	MaxContextBytes  uint64  `json:"max_context_bytes"`
}

func (v *PrCoverage) UnmarshalJSON(data []byte) error {
	type plain PrCoverage
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = PrCoverage(decoded)
	return nil
}
func (v PrCoverage) MarshalJSON() ([]byte, error) {
	type plain PrCoverage
	return wirejson.Record(plain(v))
}

type OpenPrInventory struct {
	Repository string        `json:"repository"`
	ObservedAt string        `json:"observed_at"`
	PRs        []PullRequest `json:"prs"`
}

func (v *OpenPrInventory) UnmarshalJSON(data []byte) error {
	type plain OpenPrInventory
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = OpenPrInventory(decoded)
	return nil
}
func (v OpenPrInventory) MarshalJSON() ([]byte, error) {
	type plain OpenPrInventory
	return wirejson.Record(plain(v))
}
func (v OpenPrInventory) Clone() OpenPrInventory { return wirejson.Clone(v) }

type PrCapacity struct {
	Limit      uint64  `json:"limit"`
	OwnedOpen  *uint64 `json:"owned_open"`
	Reserved   uint64  `json:"reserved"`
	Remaining  *uint64 `json:"remaining"`
	ObservedAt *string `json:"observed_at"`
	Status     string  `json:"status"`
	Reason     *string `json:"reason"`
}

func (v *PrCapacity) UnmarshalJSON(data []byte) error {
	type plain PrCapacity
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = PrCapacity(decoded)
	return nil
}
func (v PrCapacity) MarshalJSON() ([]byte, error) {
	type plain PrCapacity
	return wirejson.Record(plain(v))
}

type Cycle struct {
	Mode           CycleMode          `json:"mode"`
	ID             string             `json:"id"`
	Number         uint64             `json:"number"`
	Status         string             `json:"status"`
	StartedAt      string             `json:"started_at"`
	CompletedAt    *string            `json:"completed_at"`
	Grounding      *Grounding         `json:"grounding"`
	Proposals      []Proposal         `json:"proposals"`
	Assessments    []any              `json:"assessments"`
	Sessions       []Session          `json:"sessions"`
	Error          *string            `json:"error"`
	Repository     string             `json:"repository,omitempty" wire:"default"`
	DecisionMemory []any              `json:"decision_memory,omitempty" wire:"default"`
	RunID          *string            `json:"run_id,omitempty" wire:"default"`
	Lifecycle      WorkspaceLifecycle `json:"lifecycle,omitzero" wire:"default"` // omitted while both timestamps are nil
}

func (v *Cycle) UnmarshalJSON(data []byte) error {
	type plain Cycle
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = Cycle(decoded)
	return nil
}
func (v Cycle) MarshalJSON() ([]byte, error) { type plain Cycle; return wirejson.Record(plain(v)) }
func (v Cycle) Clone() Cycle                 { return wirejson.Clone(v) }

type RunBatch struct {
	ID      string     `json:"id"`
	Phase   BatchPhase `json:"phase"`
	CycleID *string    `json:"cycle_id"`
}

func (v *RunBatch) UnmarshalJSON(data []byte) error {
	type plain RunBatch
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = RunBatch(decoded)
	return nil
}
func (v RunBatch) MarshalJSON() ([]byte, error) {
	type plain RunBatch
	return wirejson.Record(plain(v))
}
func (v RunBatch) Clone() RunBatch { return wirejson.Clone(v) }

type Control struct {
	Paused             bool          `json:"paused"`
	CycleNumber        uint64        `json:"cycle_number"`
	NextCycleAt        int64         `json:"next_cycle_at"`
	Error              *string       `json:"error"`
	Mode               OperatingMode `json:"mode"`
	Batch              *RunBatch     `json:"batch"`
	IdleStreak         uint32        `json:"idle_streak"`
	ContextFingerprint string        `json:"context_fingerprint"`
}

type Event struct {
	ID       int64  `json:"id"`
	At       string `json:"at"`
	EntityID string `json:"entity_id"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
}

func (v *Event) UnmarshalJSON(data []byte) error {
	type plain Event
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = Event(decoded)
	return nil
}
func (v Event) MarshalJSON() ([]byte, error) { type plain Event; return wirejson.Record(plain(v)) }
