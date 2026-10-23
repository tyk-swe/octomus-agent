package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

var ErrTaskConflict = errors.New("Task changed since its last committed checkpoint")
var ErrTaskInvalid = errors.New("Task change is invalid")

// CommitTask atomically checks the predecessor, cancellation and lifecycle authority.
// A refused commit leaves the caller and all saved records unchanged.
func (s *Store) CommitTask(expected, next model.Task, change model.TaskChange) (model.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var committed model.Task
	err := s.transaction(true, func(c *sql.Conn) (err error) {
		committed, err = txCommitTask(c, expected, next, change)
		return err
	})
	if err != nil {
		return model.Task{}, err
	}
	return committed, nil
}

func txCommitTask(c *sql.Conn, expected, next model.Task, change model.TaskChange) (model.Task, error) {
	var current model.Task
	found, err := txGet(c, Tasks.Key(expected.ID), &current)
	if err != nil {
		return model.Task{}, err
	}
	if !found || next.ID != expected.ID {
		return model.Task{}, fmt.Errorf("Task %s: %w", expected.ID, ErrTaskConflict)
	}
	var marker *string
	if _, err := txGet(c, cancelMarkers.Key(expected.ID), &marker); err != nil {
		return model.Task{}, err
	}
	if marker != nil && current.OutputCommit == nil && change == model.TaskSettlement {
		next, err = model.MergeCancelledTask(expected, current, next)
		if err != nil {
			return model.Task{}, fmt.Errorf("Task %s: %w: %v", expected.ID, ErrTaskConflict, err)
		}
	} else if !wirejson.Equal(current, expected) {
		if marker != nil && current.OutputCommit == nil && (change == model.TaskCheckpoint || change == model.TaskPrepare || change == model.TaskAuthorizePublication) {
			return model.Task{}, model.ErrTaskCancelled
		}
		return model.Task{}, fmt.Errorf("Task %s: %w", expected.ID, ErrTaskConflict)
	}
	if err := model.ValidateTaskChange(current, next, change, marker != nil); err != nil {
		return model.Task{}, fmt.Errorf("%w: %w", ErrTaskInvalid, err)
	}
	if err := txPut(c, Tasks.Key(next.ID), next); err != nil {
		return model.Task{}, err
	}
	return next, nil
}

// insertTask is reserved for plan creation, which must never replace existing work.
func insertTask(c *sql.Conn, task model.Task) error {
	var existing model.Task
	found, err := txGet(c, Tasks.Key(task.ID), &existing)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("Task %s already exists: %w", task.ID, ErrTaskConflict)
	}
	return txPut(c, Tasks.Key(task.ID), task)
}

// ResetTaskCancellation commits explicit resolution and marker clearing together.
func (s *Store) ResetTaskCancellation(expected, task model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		if err := txPut(c, cancelMarkers.Key(task.ID), nil); err != nil {
			return err
		}
		_, err := txCommitTask(c, expected, task, model.TaskResolve)
		return err
	})
}

// RetryTask couples the adopted policy and cancellation clearing with the retry.
func (s *Store) RetryTask(expected, task model.Task) error {
	policy, err := wirejson.Marshal(task.AttemptPolicy)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		if err := txPut(c, cancelMarkers.Key(task.ID), nil); err != nil {
			return err
		}
		if _, err := txCommitTask(c, expected, task, model.TaskRetry); err != nil {
			return err
		}
		return txRepositoryEvent(c, s.repo, task.ID, model.EventAttemptPolicy, string(policy))
	})
}

// ArchiveTask revokes every retained merge authority in the archive transaction.
func (s *Store) ArchiveTask(expected, task model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		if _, err := txCommitTask(c, expected, task, model.TaskArchive); err != nil {
			return err
		}
		return txRevokeTaskMerges(c, task.ID, "The authorizing task was archived")
	})
}
