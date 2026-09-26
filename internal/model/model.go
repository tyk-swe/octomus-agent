// Package model defines owned, durable records and their local domain behavior.
// Call Clone when transferring a mutable record into or out of a snapshot owner.
package model

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func Now() string { return timestamp(time.Now()) }
func timestamp(at time.Time) string {
	at = at.UTC()
	layout := "2006-01-02T15:04:05"
	switch n := at.Nanosecond(); {
	case n == 0:
	case n%1_000_000 == 0:
		layout += ".000"
	case n%1_000 == 0:
		layout += ".000000"
	default:
		layout += ".000000000"
	}
	return at.Format(layout + "+00:00")
}
func UTCDay(at time.Time) string { return at.UTC().Format("2006-01-02") }
func Today() string              { return UTCDay(time.Now()) }
func ID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}
func ReviewerSlots() []string { return []string{"adversary-a", "adversary-b"} }
func ActiveStatuses() []Status {
	return []Status{StatusExecuting, StatusReviewing, StatusRepairing, StatusVerifying, StatusPublishing}
}
func AttentionStatuses() []Status  { return []Status{StatusBlocked, StatusFailed} }
func UnresolvedStatuses() []Status { return []Status{StatusBlocked, StatusFailed, StatusCancelled} }
func TerminalStatuses() []Status   { return []Status{StatusPublished, StatusCancelled} }
func (s Status) Active() bool      { return s >= StatusExecuting && s <= StatusPublishing }
func (s Status) Retryable() bool   { return s == StatusFailed || s == StatusBlocked }
func ProblemIdentity(title, key string) string {
	if strings.TrimSpace(key) == "" {
		key = title
	}
	return lowercaseIdentity(strings.TrimSpace(key))
}
func (p Proposal) ProblemIdentity() string { return ProblemIdentity(p.Title, p.ProblemKey) }
func (p Proposal) SameWork(other Proposal) bool {
	return p.Target == other.Target && (config.EqualASCII(strings.TrimSpace(p.Title), strings.TrimSpace(other.Title)) || p.ProblemIdentity() == other.ProblemIdentity())
}

// Error returns the operator guidance for b; out-of-range values read as unknown.
func (b BlockedReason) Error() string {
	if int(b) >= len(blockedReasonMessages) {
		return blockedReasonMessages[BlockedReasonUnknown]
	}
	return blockedReasonMessages[b]
}

// BlockedReasonFromError walks wrapped and multi-cause errors so the deepest
// typed reason wins.
func BlockedReasonFromError(err error) BlockedReason {
	result := BlockedReasonUnknown
	var visit func(error)
	visit = func(e error) {
		if e == nil {
			return
		}
		if reason, ok := e.(BlockedReason); ok {
			result = reason
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range u.Unwrap() {
				visit(child)
			}
		case interface{ Unwrap() error }:
			visit(u.Unwrap())
		}
	}
	visit(err)
	return result
}
func (p PlanningCapacity) Available() bool { return p.Status == PlanningCapacityStatusReady }
func (p PlanningCapacity) Message() string {
	guidance := "Planning can start."
	if p.Status == PlanningCapacityStatusDailyExhausted {
		guidance = "Wait until UTC midnight or increase the daily limit."
	}
	if p.Status == PlanningCapacityStatusLimitTooLow {
		guidance = "The configured daily limit cannot fund a complete planning pass; increase it."
	}
	return fmt.Sprintf("A complete planning pass requires %d daily admissions; %d remain (%d of %d used). %s", p.Required, p.Remaining, p.Used, p.Limit, guidance)
}
func (p PlanningCapacity) EnsureAvailable() error {
	if p.Available() {
		return nil
	}
	return fmt.Errorf("%s: %w", p.Message(), BlockedReasonBudgetExhausted)
}
func AttemptPolicyFromConfig(c config.Config) AttemptPolicy {
	return AttemptPolicy{c.MaxRepairRounds, c.MaxNoProgressRounds, c.MaxRetries, c.TaskTimeoutSeconds, c.SessionTimeoutSeconds, c.CommandTimeoutSeconds}
}
func (p AttemptPolicy) Apply(c *config.Config) {
	c.MaxRepairRounds = p.MaxRepairRounds
	c.MaxNoProgressRounds = p.MaxNoProgressRounds
	c.MaxRetries = p.MaxRetries
	c.TaskTimeoutSeconds = p.TaskTimeoutSeconds
	c.SessionTimeoutSeconds = p.SessionTimeoutSeconds
	c.CommandTimeoutSeconds = p.CommandTimeoutSeconds
}
func (w WorkspaceLifecycle) IsEmpty() bool { return w.ArchivedAt == nil && w.DiscardedAt == nil }
func (w WorkspaceLifecycle) IsZero() bool  { return w.IsEmpty() }
func (r Review) Valid() bool               { return r.Completed && strings.TrimSpace(r.Summary) != "" }
func (r Review) Clean() bool               { return r.Valid() && len(r.Findings) == 0 }
func (o DefaultBranchObservation) Describes(c config.Config) bool {
	return config.EqualASCII(o.Repository, c.GitHubRepo) && o.DefaultBranch == c.DefaultBranch
}

const (
	SessionRunning     = "running"
	SessionCompleted   = "completed"
	SessionFailed      = "failed"
	SessionInterrupted = "interrupted"
	DecisionAccepted   = "accepted"
	DecisionRejected   = "rejected"
	DecisionDeferred   = "deferred"
	DecisionCandidate  = "candidate"
	CycleRunning       = "running"
	CycleCompleted     = "completed"
	CycleIdle          = "idle"
	CycleFailed        = "failed"
	CycleInterrupted   = "interrupted"
)

func Decisions() []string {
	return []string{DecisionAccepted, DecisionRejected, DecisionDeferred, DecisionCandidate}
}
func Assessments() []string { return []string{DecisionAccepted, DecisionRejected, DecisionDeferred} }
func NewSession(id, role string, route config.Route) Session {
	return Session{ID: id, Role: role, Route: route.Clone(), Status: SessionRunning, StartedAt: Now()}
}
func (s *Session) MarkRunning()                 { s.Status = SessionRunning }
func (s *Session) MarkCompleted(summary string) { s.Status = SessionCompleted; s.Summary = summary }
func (s *Session) MarkFailed(summary string)    { s.Status = SessionFailed; s.Summary = summary }
func (s *Session) MarkInterrupted()             { s.Status = SessionInterrupted }
func InterruptRunning(sessions []Session) {
	for i := range sessions {
		if sessions[i].Status == SessionRunning {
			sessions[i].MarkInterrupted()
		}
	}
}
func CompletedSessions(sessions []Session) int {
	n := 0
	for _, s := range sessions {
		if s.Status == SessionCompleted {
			n++
		}
	}
	return n
}
func FailRunning(sessions []Session, summary string) {
	for i := range sessions {
		if sessions[i].Status != SessionRunning {
			continue
		}
		sessions[i].Status = SessionFailed
		if sessions[i].Summary == "" {
			sessions[i].Summary = summary
		}
	}
}
func (t Task) AttemptReviews() uint64 {
	if uint64(len(t.Reviews)) < t.ReviewBaseline {
		return 0
	}
	return uint64(len(t.Reviews)) - t.ReviewBaseline
}
func (t Task) ExecutionConfig() config.Config {
	c := t.Config.Clone()
	if t.AttemptPolicy != nil {
		t.AttemptPolicy.Apply(&c)
	}
	return c
}
func (t Task) AllowedActions() []string {
	if t.Lifecycle.ArchivedAt != nil {
		if t.Lifecycle.DiscardedAt != nil {
			return []string{}
		}
		return []string{"discard"}
	}
	if t.Status == StatusPublished {
		return []string{"archive"}
	}
	if t.Status == StatusCancelled {
		actions := []string{"archive"}
		if t.Lifecycle.DiscardedAt == nil && t.OutputCommit == nil && !t.RediscoveryRequested && len(t.SupersededBy) == 0 {
			actions = append(actions, "supersede")
		}
		return actions
	}
	if t.Lifecycle.DiscardedAt != nil || t.Status == StatusPublishing {
		return []string{}
	}
	if !t.Status.Retryable() {
		if t.OutputCommit == nil {
			return []string{"cancel"}
		}
		return []string{}
	}
	actions := []string{}
	if t.OutputCommit == nil {
		actions = append(actions, "cancel")
	}
	actions = append(actions, "archive")
	reason := BlockedReasonUnknown
	if t.BlockedReason != nil {
		reason = *t.BlockedReason
	}
	switch reason {
	case BlockedReasonStaleBase, BlockedReasonInvalidPlan, BlockedReasonWorkspaceInvalid:
		actions = append(actions, "supersede")
	case BlockedReasonRemoteConflict, BlockedReasonPublicationUncertain:
		actions = append(actions, "reconcile")
	case BlockedReasonDependencyBlocked, BlockedReasonRunnerUnavailable:
		actions = append(actions, "retry", "supersede")
	default:
		actions = append(actions, "retry")
	}
	return actions
}
func (p PullRequest) OwnedOpen() bool { return p.Owned && p.State == "open" }
func DefaultControl() Control         { return Control{Paused: true, Mode: OperatingModePaused} }
func (c *Control) SetMode(mode OperatingMode) {
	c.Mode = mode
	c.Paused = mode == OperatingModePaused
	if mode != OperatingModeRunOnce {
		c.Batch = nil
	}
}
func (c *Control) UnmarshalJSON(data []byte) error {
	type plain Control
	var saved plain
	if err := wirejson.Decode(data, &saved, false, false); err != nil {
		return err
	}
	*c = Control(saved)
	return nil
}
func (c Control) MarshalJSON() ([]byte, error) {
	type plain Control
	return wirejson.Record(plain(c))
}
func (c Control) Clone() Control { return wirejson.Clone(c) }
