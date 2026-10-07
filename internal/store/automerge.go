package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

const (
	MergeResultConfirmed = "confirmed"
	MergeResultObserved  = "observed"
)

func (s *Store) CompletePublication(task model.Task, p model.PullRequest, merge *model.AutoMergeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		repository := task.Config.GitHubRepo
		var stored model.Task
		found, err := txGet(c, "task", task.ID, &stored)
		if err != nil {
			return err
		}
		if !found || stored.Status != model.StatusPublishing || stored.OutputCommit == nil ||
			task.OutputCommit == nil || *stored.OutputCommit != *task.OutputCommit {
			return fmt.Errorf("Task %s is not at its durable publishing checkpoint", task.ID)
		}
		previousID, previous, err := prObservationAt(c, repository, p.Number)
		if err != nil {
			return err
		}
		if previous != nil && previous.AutoMerge != nil &&
			(previous.AutoMerge.Status == model.AutoMergeMerging ||
				previous.AutoMerge.Status == model.AutoMergeUncertain) {
			return fmt.Errorf("Pull request %d still carries an unresolved merge intent", p.Number)
		}
		if task.AutoMergeSnapshot == nil {
			task.AutoMergeSnapshot = stored.AutoMergeSnapshot
		}
		if previous != nil && previous.AutoMerge != nil && previous.AutoMerge.TaskID != task.ID {
			state := previous.AutoMerge.Clone()
			var old model.Task
			oldFound, err := txGet(c, "task", previous.AutoMerge.TaskID, &old)
			if err != nil {
				return err
			}
			if oldFound && config.EqualASCII(old.Config.GitHubRepo, repository) &&
				old.PRNumber != nil && *old.PRNumber == p.Number &&
				old.OutputCommit != nil && *old.OutputCommit == previous.AutoMerge.Head &&
				old.ID == previous.AutoMerge.TaskID &&
				old.ComparisonBase == state.ComparisonBase &&
				old.Branch == state.HeadBranch &&
				old.Config.DefaultBranch == state.BaseBranch {
				old.AutoMergeSnapshot = &model.AutoMergeSnapshot{
					Repository: previous.Repository,
					PRNumber:   previous.PR.Number,
					State:      state,
				}
				if err := txPut(c, "task", old.ID, old); err != nil {
					return err
				}
			}
		}
		recordID := previousID
		if previous == nil {
			recordID = fmt.Sprintf("%s:%d", strings.ToLower(repository), p.Number)
		}
		head := p.Head
		observation := model.PRObservation{
			Repository:    repository,
			ObservedAt:    model.Now(),
			DeliveredHead: &head,
			PR:            p,
			AutoMerge:     merge,
		}
		if err := txPut(c, "task", task.ID, task); err != nil {
			return err
		}
		if err := txPut(c, "pr", recordID, observation); err != nil {
			return err
		}
		return txEvent(c, task.ID, "status", "Published")
	})
}

func preserveAutoMerge(previous *model.PRObservation, p model.PullRequest) *model.AutoMergeState {
	if previous == nil || previous.AutoMerge == nil {
		return nil
	}
	merge := previous.AutoMerge.Clone()
	pending := merge.Status == model.AutoMergeMerging || merge.Status == model.AutoMergeUncertain
	bound := p.Owned && p.Head == merge.Head &&
		merge.HeadBranch != "" && merge.BaseBranch != "" &&
		p.Branch == merge.HeadBranch && p.Base == merge.BaseBranch
	switch {
	case p.State == "merged" || p.State == "closed":
		if pending {
			if !bound {
				merge.Authorized = false
				merge.Reason = "The pull request is " + p.State + " after its head, branch, base or ownership changed; the recorded intent remains for reconciliation"
				merge.ObservedAt = model.Now()
			}
			break
		}
		outcome := model.AutoMergeMerged
		why := "The pull request is merged on the remote"
		if p.State == "closed" {
			outcome = model.AutoMergeClosed
			why = "The pull request was closed without merging"
		}
		if merge.Status == outcome ||
			(!merge.Status.Pending() && merge.Status != model.AutoMergeManual) {
			break
		}
		if !bound {
			merge.Authorized = false
			merge.Status = model.AutoMergeManual
			merge.Reason = "The pull request is " + p.State + " after its head or ownership changed; the outcome does not apply to the recorded reviewed head"
			merge.ObservedAt = model.Now()
			break
		}
		merge.Status = outcome
		merge.ResultSource = new(MergeResultObserved)
		merge.Reason = why
		merge.ObservedAt = model.Now()
	case pending:
		if !bound {
			merge.Authorized = false
			merge.Reason = "The pull request head, branch, base or ownership changed; the recorded intent remains for reconciliation"
			merge.ObservedAt = model.Now()
		}
	case merge.Status == model.AutoMergeWaiting || merge.Status == model.AutoMergeManual && merge.Authorized:
		if !sameDeliveryHead(merge, p) {
			merge.Authorized = false
			merge.Status = model.AutoMergeManual
			merge.Reason = "The pull request head, ownership or open state changed; automatic merge authority is revoked"
			merge.ObservedAt = model.Now()
		}
	}
	return &merge
}

func sameDeliveryHead(merge model.AutoMergeState, p model.PullRequest) bool {
	return p.State == "open" && p.Owned && p.Head == merge.Head &&
		merge.HeadBranch != "" && merge.BaseBranch != "" &&
		p.Branch == merge.HeadBranch && p.Base == merge.BaseBranch
}

type MergeCandidate struct {
	Seq         int64
	Observation model.PRObservation
}

func (s *Store) MergeCandidates(afterSeq, limit int64) ([]MergeCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.conn.QueryContext(background, maintenanceCandidatesQuery, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := []MergeCandidate{}
	for rows.Next() {
		var candidate MergeCandidate
		var data string
		if err := rows.Scan(&candidate.Seq, &data); err != nil {
			return nil, err
		}
		if err := decodeJSON([]byte(data), &candidate.Observation); err != nil {
			return nil, fmt.Errorf("Saved pr record is unreadable: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) MergeCounts(repository string) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.conn.QueryContext(background, maintenanceCountsQuery, repository)
	if err != nil {
		return nil, err
	}
	pairs, err := scanAll(rows, func(pair *[2]any) []any { return []any{&pair[0], &pair[1]} })
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for _, pair := range pairs {
		status, _ := pair[0].(string)
		count, _ := pair[1].(int64)
		counts[status] = count
	}
	return counts, nil
}

func (s *Store) BatchMergeWait(runID string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pending int64
	err := s.conn.QueryRowContext(background, maintenanceBatchWaitQuery, runID).Scan(&pending)
	return uint64(pending), err
}

func (s *Store) UnfinishedBranchWork(repository, branch, exceptTaskID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists bool
	err := s.conn.QueryRowContext(background, maintenanceUnfinishedBranchQuery, repository, branch, exceptTaskID).Scan(&exists)
	return exists, err
}

func (s *Store) ClaimMerge(repository string, number uint64, intent model.AutoMergeState, allowManual bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if intent.Status != model.AutoMergeMerging || !intent.Authorized ||
		intent.HeadBranch == "" || intent.BaseBranch == "" ||
		intent.AttemptID == nil || *intent.AttemptID == "" ||
		intent.AttemptedAt == nil || *intent.AttemptedAt == "" {
		return false, nil
	}
	return s.conditional(func(c *sql.Conn) error {
		id, observation, err := prObservationAt(c, repository, number)
		if err != nil {
			return err
		}
		if observation == nil || observation.AutoMerge == nil {
			return errRollback
		}
		current := observation.AutoMerge
		eligible := current.Status == model.AutoMergeWaiting ||
			(allowManual && current.Status == model.AutoMergeManual)
		if !current.Authorized || !eligible ||
			current.TaskID != intent.TaskID || current.Head != intent.Head ||
			current.ComparisonBase != intent.ComparisonBase ||
			current.HeadBranch != intent.HeadBranch || current.BaseBranch != intent.BaseBranch ||
			current.PolicyRevision != intent.PolicyRevision {
			return errRollback
		}
		observation.AutoMerge = &intent
		if err := txPut(c, "pr", id, *observation); err != nil {
			return err
		}
		return txEvent(c, intent.TaskID, "automerge",
			fmt.Sprintf("Automatic squash merge attempt %s recorded for %s", *intent.AttemptID, intent.Head))
	})
}

func (s *Store) SettleMerge(repository string, number uint64, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conditional(func(c *sql.Conn) error {
		id, observation, err := prObservationAt(c, repository, number)
		if err != nil {
			return err
		}
		if observation == nil || observation.AutoMerge == nil {
			return errRollback
		}
		merge := observation.AutoMerge
		if merge.TaskID != expected.TaskID || merge.Head != expected.Head ||
			merge.ComparisonBase != expected.ComparisonBase ||
			merge.HeadBranch != expected.HeadBranch || merge.BaseBranch != expected.BaseBranch ||
			merge.PolicyRevision != expected.PolicyRevision {
			return errRollback
		}
		if (merge.AttemptID == nil) != (expected.AttemptID == nil) ||
			(merge.AttemptID != nil && *merge.AttemptID != *expected.AttemptID) {
			return errRollback
		}
		settleable := merge.Status.Pending() || merge.Status == model.AutoMergeManual && merge.Authorized
		terminal := merge.Status == model.AutoMergeMerged || merge.Status == model.AutoMergeClosed
		switch {
		case settleable:
		case terminal && status == merge.Status:
			if commit != nil {
				if merge.MergeCommit != nil && *merge.MergeCommit != *commit {
					return errRollback
				}
				merge.MergeCommit = commit
			}
			confirmed := merge.ResultSource != nil && *merge.ResultSource == MergeResultConfirmed
			if source == MergeResultConfirmed && !confirmed {
				merge.ResultSource = &source
				merge.Reason = reason
				merge.ObservedAt = model.Now()
			} else if !confirmed && source != "" && merge.ResultSource == nil {
				merge.ResultSource = &source
				merge.Reason = reason
				merge.ObservedAt = model.Now()
			}
			return txPut(c, "pr", id, *observation)
		default:
			return errRollback
		}
		merge.Status = status
		merge.Reason = reason
		merge.ObservedAt = model.Now()
		if revoke {
			merge.Authorized = false
		}
		if status == model.AutoMergeWaiting {
			merge.AttemptID = nil
		}
		if source != "" {
			merge.ResultSource = &source
		}
		if commit != nil {
			merge.MergeCommit = commit
		}
		if status == model.AutoMergeMerged || status == model.AutoMergeClosed {
			observation.PR.State = status.String()
		}
		return txPut(c, "pr", id, *observation)
	})
}

func (s *Store) RevokeTaskMerges(taskID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.conn.QueryContext(background, maintenanceRevokeQuery, taskID)
	if err != nil {
		return err
	}
	type pending struct {
		id          string
		observation model.PRObservation
	}
	var records []pending
	err = func() error {
		defer rows.Close()
		for rows.Next() {
			var record pending
			var data string
			if err := rows.Scan(&record.id, &data); err != nil {
				return err
			}
			if err := decodeJSON([]byte(data), &record.observation); err != nil {
				return fmt.Errorf("Saved pr %s is unreadable: %w", record.id, err)
			}
			records = append(records, record)
		}
		return rows.Err()
	}()
	if err != nil {
		return err
	}
	for _, record := range records {
		merge := record.observation.AutoMerge
		merge.Authorized = false
		merge.Reason = reason
		merge.ObservedAt = model.Now()
		if merge.Status != model.AutoMergeMerging && merge.Status != model.AutoMergeUncertain {
			merge.Status = model.AutoMergeManual
		}
		if err := txPut(s.conn, "pr", record.id, record.observation); err != nil {
			return err
		}
	}
	return nil
}
