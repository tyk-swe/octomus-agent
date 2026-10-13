package model

import "github.com/tyk-swe/octomus-agent/internal/wirejson"

type RequestStatus uint8

const (
	RequestPending   RequestStatus = 0
	RequestAccepted  RequestStatus = 1
	RequestRejected  RequestStatus = 2
	RequestWithdrawn RequestStatus = 3
)

var requestStatusNames = []string{"pending", "accepted", "rejected", "withdrawn"}

func (s RequestStatus) String() string               { return wirejson.EnumName(s, requestStatusNames) }
func (s RequestStatus) MarshalText() ([]byte, error) { return wirejson.EnumText(s, requestStatusNames) }
func (s *RequestStatus) UnmarshalText(text []byte) error {
	return wirejson.ParseEnum(s, text, requestStatusNames)
}

// RequestAssessment is the latest planning outcome recorded for a request. Audit
// cycles record an assessment without changing the request's status.
type RequestAssessment struct {
	CycleID  string    `json:"cycle_id"`
	Mode     CycleMode `json:"mode"`
	Decision string    `json:"decision"`
	Reason   string    `json:"reason"`
	At       string    `json:"at"`
}

func (v *RequestAssessment) UnmarshalJSON(data []byte) error {
	return wirejson.DecodeRecord(data, v)
}
func (v RequestAssessment) MarshalJSON() ([]byte, error) {
	type plain RequestAssessment
	return wirejson.Record(plain(v))
}
func (v RequestAssessment) Clone() RequestAssessment { return wirejson.Clone(v) }

type OperatorRequest struct {
	ID          string             `json:"id"`
	Repository  string             `json:"repository"`
	Title       string             `json:"title"`
	Description string             `json:"description"`
	Target      string             `json:"target"`
	Status      RequestStatus      `json:"status"`
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
	Assessment  *RequestAssessment `json:"assessment"`
	TaskID      *string            `json:"task_id"`
}

func (v *OperatorRequest) UnmarshalJSON(data []byte) error {
	return wirejson.DecodeRecord(data, v)
}
func (v OperatorRequest) MarshalJSON() ([]byte, error) {
	type plain OperatorRequest
	return wirejson.Record(plain(v))
}
func (v OperatorRequest) Clone() OperatorRequest { return wirejson.Clone(v) }

// ProposalID is the candidate identity a pending request takes in a plan.
func (r OperatorRequest) ProposalID() string { return RequestProposalPrefix + r.ID }

const (
	RequestProposalPrefix = "request-"
	RequestPendingLimit   = 20
	RequestSeedLimit      = 10
)
