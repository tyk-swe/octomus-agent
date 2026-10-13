package model

import (
	"errors"
	"fmt"

	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// TaskChange names the authority for a task update, without changing its wire format.
type TaskChange uint8

const (
	TaskCheckpoint TaskChange = iota
	TaskPrepare
	TaskAdmission
	TaskAuthorizePublication
	TaskPublication
	TaskSettlement
	TaskRecovery
	TaskRetry
	TaskResolve
	TaskInspect
	TaskCancel
	TaskArchive
	TaskDiscard
	TaskLineage
	TaskMergeSnapshot
)

var ErrTaskCancelled = errors.New("Task was cancelled by the operator")

// ValidateTaskChange is the lifecycle policy. Storage also compares the complete
// predecessor, and the engine checks that the worker still owns its handle.
func ValidateTaskChange(current, next Task, change TaskChange, cancelled bool) error {
	if current.ID != next.ID || current.CycleID != next.CycleID || current.CreatedAt != next.CreatedAt ||
		current.Branch != next.Branch || current.DefaultRevision != next.DefaultRevision ||
		!wirejson.Equal(current.Proposal, next.Proposal) || !wirejson.Equal(current.Config, next.Config) || !wirejson.Equal(current.Route, next.Route) {
		return errors.New("Task change would replace its planning identity")
	}
	if current.OutputCommit != nil && !wirejson.Equal(current.OutputCommit, next.OutputCommit) {
		return errors.New("Task change would replace its authorized output commit")
	}
	if cancelled && current.OutputCommit == nil &&
		(change == TaskCheckpoint || change == TaskPrepare || change == TaskAdmission || change == TaskAuthorizePublication) {
		return ErrTaskCancelled
	}
	if (current.Lifecycle.ArchivedAt != nil || current.Lifecycle.DiscardedAt != nil) &&
		change != TaskSettlement && change != TaskDiscard && change != TaskMergeSnapshot {
		return errors.New("Task workspaces are no longer eligible for this change")
	}
	valid := false
	switch change {
	case TaskCheckpoint:
		valid = current.Status.Active() && current.Status == next.Status
		if current.Status != next.Status {
			switch current.Status {
			case StatusExecuting, StatusRepairing:
				valid = next.Status == StatusReviewing
			case StatusReviewing:
				valid = next.Status == StatusVerifying || next.Status == StatusRepairing
			case StatusVerifying:
				valid = next.Status == StatusRepairing
			}
		}
	case TaskPrepare:
		valid = current.Status == StatusExecuting && next.Status == current.Status && current.ExecutionSession == nil
	case TaskAdmission:
		valid = current.Status == StatusQueued && next.Status == StatusExecuting
	case TaskAuthorizePublication:
		valid = current.Status.Active() && next.Status == StatusPublishing && next.OutputCommit != nil && next.ReviewAuthorizes(*next.OutputCommit)
	case TaskPublication:
		valid = current.Status == StatusPublishing && next.Status == StatusPublished && current.OutputCommit != nil
	case TaskSettlement:
		valid = next.Status == current.Status || ((current.Status.Active() || current.Status == StatusQueued) &&
			(next.Status == StatusBlocked || next.Status == StatusFailed || next.Status == StatusCancelled))
		if cancelled && current.OutputCommit == nil {
			valid = next.Status == StatusCancelled
		}
	case TaskRecovery:
		valid = current.Status.Active() && (next.Status == StatusQueued || next.Status == StatusBlocked || next.Status == StatusCancelled)
		if cancelled && current.OutputCommit == nil {
			valid = next.Status == StatusCancelled
		}
	case TaskRetry:
		valid = current.Status.Retryable() && next.Status == StatusQueued && next.Attempts == current.Attempts+1
	case TaskResolve:
		valid = (current.Status.Retryable() || current.Status == StatusCancelled) &&
			(next.Status == current.Status || next.Status == StatusCancelled || (next.Status == StatusPublishing && current.OutputCommit != nil))
	case TaskInspect:
		valid = current.Status.Retryable() && next.Status == current.Status
	case TaskCancel:
		valid = current.OutputCommit == nil && current.Status != StatusPublishing && current.Status != StatusPublished && next.Status == StatusCancelled
	case TaskArchive:
		valid = !current.Status.Active() && current.Status != StatusQueued && next.Lifecycle.ArchivedAt != nil &&
			(next.Status == current.Status || current.Status.Retryable() && next.Status == StatusCancelled)
	case TaskDiscard:
		valid = !current.Status.Active() && current.Status != StatusQueued && next.Status == current.Status && next.Lifecycle.DiscardedAt != nil
	case TaskLineage, TaskMergeSnapshot:
		valid = next.Status == current.Status
	}
	if !valid {
		return fmt.Errorf("Task transition from %s to %s is not authorized", current.Status, next.Status)
	}
	allowed := current.Clone()
	allowed.Status, allowed.UpdatedAt = next.Status, next.UpdatedAt
	allowed.Error, allowed.BlockedReason = next.Error, next.BlockedReason
	switch change {
	case TaskCheckpoint, TaskPrepare, TaskAuthorizePublication, TaskSettlement:
		copyTaskEvidence(&allowed, next)
		allowed.ExecutionSession, allowed.RepairSession = next.ExecutionSession, next.RepairSession
		allowed.MaintenanceFootprint = next.MaintenanceFootprint
		if change == TaskPrepare {
			allowed.SourceRevision, allowed.Workspace, allowed.ComparisonBase = next.SourceRevision, next.Workspace, next.ComparisonBase
		}
		if change == TaskAuthorizePublication {
			allowed.OutputCommit = next.OutputCommit
		}
	case TaskPublication:
		allowed.PRNumber, allowed.PRURL, allowed.AutoMergeSnapshot = next.PRNumber, next.PRURL, next.AutoMergeSnapshot
	case TaskRecovery:
		allowed.Sessions, allowed.Attempts = next.Sessions, next.Attempts
	case TaskRetry:
		allowed.Attempts, allowed.AttemptPolicy, allowed.RunID = next.Attempts, next.AttemptPolicy, next.RunID
		allowed.ReviewBaseline, allowed.RepairRounds, allowed.RepairProgress = next.ReviewBaseline, next.RepairRounds, next.RepairProgress
	case TaskInspect:
		allowed.AttemptPolicy = next.AttemptPolicy
	case TaskResolve:
		allowed.RediscoveryRequested, allowed.RediscoveryResult = next.RediscoveryRequested, next.RediscoveryResult
	case TaskArchive:
		allowed.Lifecycle.ArchivedAt = next.Lifecycle.ArchivedAt
	case TaskDiscard:
		allowed.Lifecycle.DiscardedAt = next.Lifecycle.DiscardedAt
	case TaskLineage:
		allowed.SupersededBy, allowed.RediscoveryRequested, allowed.RediscoveryResult = next.SupersededBy, next.RediscoveryRequested, next.RediscoveryResult
	case TaskMergeSnapshot:
		allowed.AutoMergeSnapshot = next.AutoMergeSnapshot
	}
	if change == TaskCheckpoint || change == TaskPrepare || change == TaskAuthorizePublication || change == TaskSettlement {
		if err := validateTaskEvidence(current, next); err != nil {
			return err
		}
	}
	if !wirejson.Equal(allowed, next) {
		return errors.New("Task change would replace fields owned by another operation")
	}

	return nil
}

// MergeCancelledTask retains evidence only when cancellation is the sole
// competing edit. Lifecycle, policy, source and publication fields stay canonical.
func MergeCancelledTask(expected, current, pending Task) (Task, error) {
	baseline := current.Clone()
	baseline.Status, baseline.UpdatedAt = expected.Status, expected.UpdatedAt
	baseline.Error, baseline.BlockedReason = expected.Error, expected.BlockedReason
	if current.OutputCommit != nil || !wirejson.Equal(baseline, expected) {
		return Task{}, errors.New("Task evidence changed independently of cancellation")
	}
	if err := validateTaskEvidence(expected, pending); err != nil {
		return Task{}, err
	}

	next, evidence := current.Clone(), pending.Clone()
	copyTaskEvidence(&next, evidence)
	next.Status = StatusCancelled
	next.Error, next.BlockedReason = new("Cancelled by the operator"), nil
	next.UpdatedAt = pending.UpdatedAt
	FailRunning(next.Sessions, *next.Error)
	return next, nil
}

// SettleTaskEvidence drops uncommitted preparation and publication intent while
// retaining worker observations. The store still validates the predecessor.
func SettleTaskEvidence(expected, pending Task) Task {
	next := expected.Clone()
	evidence := pending.Clone()
	copyTaskEvidence(&next, evidence)
	next.ExecutionSession, next.RepairSession = evidence.ExecutionSession, evidence.RepairSession
	next.Status, next.UpdatedAt = pending.Status, pending.UpdatedAt
	next.Error, next.BlockedReason = evidence.Error, evidence.BlockedReason
	next.MaintenanceFootprint = evidence.MaintenanceFootprint
	return next
}

func copyTaskEvidence(next *Task, evidence Task) {
	next.Sessions, next.Reviews, next.Verification = evidence.Sessions, evidence.Reviews, evidence.Verification
	next.RepairRounds, next.RepairProgress = evidence.RepairRounds, evidence.RepairProgress
}

func taskHistoryPrefix[T any](before, after []T) bool {
	if len(after) < len(before) {
		return false
	}
	for i := range before {
		if !wirejson.Equal(before[i], after[i]) {
			return false
		}
	}
	return true
}

func validateTaskEvidence(expected, pending Task) error {
	if len(pending.Sessions) < len(expected.Sessions) || !taskHistoryPrefix(expected.Reviews, pending.Reviews) || !taskHistoryPrefix(expected.Verification, pending.Verification) {
		return errors.New("Task checkpoint would replace committed evidence")
	}
	repairs := uint64(0)
	seen := make(map[string]bool, len(pending.Sessions))
	for i, session := range pending.Sessions {
		if session.ID == "" || seen[session.ID] {
			return errors.New("Pending evidence repeats or omits a session identity")
		}
		seen[session.ID] = true
		completed := false
		if i < len(expected.Sessions) {
			old := expected.Sessions[i]
			if old.ID != session.ID || old.Role != session.Role || old.StartedAt != session.StartedAt || !wirejson.Equal(old.Route, session.Route) {
				return errors.New("Pending evidence replaced a session identity")
			}
			if old.FirstTurnStarted != nil && *old.FirstTurnStarted && (session.FirstTurnStarted == nil || !*session.FirstTurnStarted) {
				return errors.New("Pending evidence forgot a started runner turn")
			}
			completed = old.Status == SessionCompleted
		}
		if session.Role == "repair" && session.Status == SessionCompleted && !completed {
			repairs++
		}
	}
	if expected.RepairRounds != nil {
		if pending.RepairRounds == nil || *pending.RepairRounds != *expected.RepairRounds+repairs {
			return errors.New("Pending repair accounting does not match completed sessions")
		}
	} else if pending.RepairRounds != nil {
		if *pending.RepairRounds != expected.AttemptReviews()+repairs {
			return errors.New("Legacy repair accounting was not initialized conservatively")
		}
	} else if repairs != 0 {
		return errors.New("Completed repair evidence requires its accounting checkpoint")
	}
	return nil
}
