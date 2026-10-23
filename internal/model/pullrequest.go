package model

import (
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type PullRequest struct {
	Number           uint64 `json:"number"`
	Title            string `json:"title"`
	Branch           string `json:"branch"`
	Head             string `json:"head"`
	Base             string `json:"base"`
	URL              string `json:"url"`
	Body             string `json:"body"`
	State            string `json:"state"`
	ChangedLines     uint64 `json:"changed_lines"`
	CreatedAt        string `json:"created_at"`
	Owned            bool   `json:"owned"`
	HeadRepository   string `json:"head_repository"`
	BaseRepository   string `json:"base_repository"`
	ReviewDecision   string `json:"review_decision" wire:"default"`
	CheckStatus      string `json:"check_status" wire:"default"`
	Mergeability     string `json:"mergeability" wire:"default"`
	StatusSource     string `json:"status_source" wire:"default"`
	StatusObservedAt string `json:"status_observed_at" wire:"default"`
}

func (p *PullRequest) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, p) }
func (p PullRequest) MarshalJSON() ([]byte, error) {
	type plain PullRequest
	return wirejson.Record(plain(p))
}
func (p PullRequest) Clone() PullRequest { return wirejson.Clone(p) }

type PRObservation struct {
	Repository           string          `json:"repository"`
	PR                   PullRequest     `json:"pr"`
	ObservedAt           string          `json:"observed_at"`
	DeliveredHead        *string         `json:"delivered_head"`
	ExternalHeadMovement bool            `json:"external_head_movement"`
	AutoMerge            *AutoMergeState `json:"auto_merge" wire:"default"`
}

func (v *PRObservation) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v PRObservation) MarshalJSON() ([]byte, error) {
	type plain PRObservation
	return wirejson.Record(plain(v))
}

type Grounding struct {
	Revision           string              `json:"revision"`
	PRs                []PullRequest       `json:"prs"`
	ExternalPRs        []ExternalPRContext `json:"external_prs"`
	PRCoverage         PRCoverage          `json:"pr_coverage"`
	History            any                 `json:"history"`
	MaintenanceDue     bool                `json:"maintenance_due"`
	MaintenanceTargets []string            `json:"maintenance_targets"`
}

func (v *Grounding) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v Grounding) MarshalJSON() ([]byte, error) {
	type plain Grounding
	return wirejson.Record(plain(v))
}

type ExternalPRContext struct {
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
	// False also covers historical records without a local object observation.
	LocalHeadAvailable bool `json:"local_head_available" wire:"default"`
}

func (v *ExternalPRContext) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v ExternalPRContext) MarshalJSON() ([]byte, error) {
	type plain ExternalPRContext
	return wirejson.Record(plain(v))
}

type PRCoverage struct {
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

func (v *PRCoverage) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v PRCoverage) MarshalJSON() ([]byte, error) {
	type plain PRCoverage
	return wirejson.Record(plain(v))
}

type OpenPRInventory struct {
	Repository string        `json:"repository"`
	ObservedAt string        `json:"observed_at"`
	PRs        []PullRequest `json:"prs"`
}

func (v *OpenPRInventory) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v OpenPRInventory) MarshalJSON() ([]byte, error) {
	type plain OpenPRInventory
	return wirejson.Record(plain(v))
}
func (v OpenPRInventory) Clone() OpenPRInventory { return wirejson.Clone(v) }

// OwnedBranches is the set of branches with an owned open PR in the inventory.
func (v OpenPRInventory) OwnedBranches() map[string]struct{} {
	branches := map[string]struct{}{}
	for _, pr := range v.PRs {
		if pr.OwnedOpen() {
			branches[pr.Branch] = struct{}{}
		}
	}
	return branches
}

type PRCapacity struct {
	Limit      uint64  `json:"limit"`
	OwnedOpen  *uint64 `json:"owned_open"`
	Reserved   uint64  `json:"reserved"`
	Remaining  *uint64 `json:"remaining"`
	ObservedAt *string `json:"observed_at"`
	Status     string  `json:"status"`
	Reason     *string `json:"reason"`
}

func (v *PRCapacity) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v PRCapacity) MarshalJSON() ([]byte, error) {
	type plain PRCapacity
	return wirejson.Record(plain(v))
}
