package store

import (
	"database/sql"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// SaveTask checkpoints working evidence. The caller must not treat proposed
// lifecycle changes as committed until this succeeds. Projection and outbox
// triggers run in the same statement; display activity is not authorization.
func (s *Store) SaveTask(task model.Task) error {
	return Put(s, Tasks.Key(task.ID), task)
}

// ResetTaskCancellation commits the replacement task and clearing its cancellation
// marker together. It is used after an explicit operator resolution.
func (s *Store) ResetTaskCancellation(task model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		return txResetTaskCancellation(c, task)
	})
}

func txResetTaskCancellation(c *sql.Conn, task model.Task) error {
	if err := txPut(c, cancelMarkers.Key(task.ID), nil); err != nil {
		return err
	}
	return txPut(c, Tasks.Key(task.ID), task)
}

// RetryTask includes the adopted attempt policy in the durable retry decision.
// Refusing either the task or its policy evidence preserves the previous marker.
func (s *Store) RetryTask(task model.Task) error {
	policy, err := wirejson.Marshal(task.AttemptPolicy)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		if err := txResetTaskCancellation(c, task); err != nil {
			return err
		}
		return txEvent(c, task.ID, "attempt_policy", string(policy))
	})
}

// ArchiveTask revokes the task's merge authority in the same transaction as its
// archive checkpoint, including any retained unresolved merge intent.
func (s *Store) ArchiveTask(task model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		if err := txPut(c, Tasks.Key(task.ID), task); err != nil {
			return err
		}
		return txRevokeTaskMerges(c, task.ID, "The authorizing task was archived")
	})
}
