package store

import (
	"encoding/json"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

// Summary types describe the indexed read models. They deliberately exclude
// task configuration, transcripts, prompts and PR bodies; full records remain
// available through their separate detail and evidence operations.
type TaskSummary struct {
	ID            string                   `json:"id"`
	CycleID       string                   `json:"cycle_id"`
	Title         string                   `json:"title"`
	Category      string                   `json:"category"`
	Tier          string                   `json:"tier"`
	Target        string                   `json:"target"`
	Branch        string                   `json:"branch"`
	Status        model.Status             `json:"status"`
	PRURL         *string                  `json:"pr_url"`
	PRNumber      *uint64                  `json:"pr_number"`
	Error         *string                  `json:"error"`
	BlockedReason *model.BlockedReason     `json:"blocked_reason"`
	CreatedAt     string                   `json:"created_at"`
	UpdatedAt     string                   `json:"updated_at"`
	Lifecycle     model.WorkspaceLifecycle `json:"lifecycle"`
	SupersededBy  []string                 `json:"superseded_by"`
}

type CycleSummary struct {
	ID             string                   `json:"id"`
	Number         uint64                   `json:"number"`
	Mode           model.CycleMode          `json:"mode"`
	Status         model.CycleStatus        `json:"status"`
	StartedAt      string                   `json:"started_at"`
	CompletedAt    *string                  `json:"completed_at"`
	Error          *string                  `json:"error"`
	SessionCount   uint64                   `json:"session_count"`
	Decisions      DecisionCounts           `json:"decisions"`
	Lifecycle      model.WorkspaceLifecycle `json:"lifecycle"`
	AllowedActions []model.CycleAction      `json:"allowed_actions"`
}

// The indexed projection stores facts. Derived policy is attached only to the
// current read model so released databases need no projection migration.
func (summary *CycleSummary) UnmarshalJSON(data []byte) error {
	type fields CycleSummary
	var value fields
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*summary = CycleSummary(value)
	summary.AllowedActions = model.CycleActions(summary.Status, summary.Lifecycle)
	return nil
}

type DecisionCounts struct {
	Accepted  int64 `json:"accepted"`
	Rejected  int64 `json:"rejected"`
	Deferred  int64 `json:"deferred"`
	Candidate int64 `json:"candidate"`
}

type PullRequestSummary struct {
	Number           uint64 `json:"number"`
	Title            string `json:"title"`
	Branch           string `json:"branch"`
	Head             string `json:"head"`
	Base             string `json:"base"`
	URL              string `json:"url"`
	State            string `json:"state"`
	ChangedLines     uint64 `json:"changed_lines"`
	CreatedAt        string `json:"created_at"`
	Owned            bool   `json:"owned"`
	HeadRepository   string `json:"head_repository"`
	BaseRepository   string `json:"base_repository"`
	ReviewDecision   string `json:"review_decision"`
	CheckStatus      string `json:"check_status"`
	Mergeability     string `json:"mergeability"`
	StatusSource     string `json:"status_source"`
	StatusObservedAt string `json:"status_observed_at"`
}

type PRSummary struct {
	Repository           string                `json:"repository"`
	PR                   PullRequestSummary    `json:"pr"`
	ObservedAt           string                `json:"observed_at"`
	DeliveredHead        *string               `json:"delivered_head"`
	ExternalHeadMovement bool                  `json:"external_head_movement"`
	AutoMerge            *model.AutoMergeState `json:"auto_merge"`
}

// ProposalFields retains the canonical proposal shape without inheriting its
// record JSON methods: these projections also carry their own revision and cycle.
type ProposalFields model.Proposal

type ProposalDetail struct {
	ProposalFields
	ContentRevision uint64 `json:"content_revision"`
}

type ProposalSummary struct {
	ProposalDetail
	Cycle   uint64          `json:"cycle"`
	CycleID string          `json:"cycle_id"`
	Mode    model.CycleMode `json:"mode"`
}
