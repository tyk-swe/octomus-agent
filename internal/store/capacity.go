package store

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type PRReservation struct {
	TaskID     string
	Repository string
	Branch     string
	AdmittedAt string
}

var errRollback = errors.New("rollback")

// conditional runs fn in an immediate transaction. fn returns errRollback when a precondition no longer holds, which
// rolls back and reports false without an error.
func (s *Store) conditional(fn func(c *sql.Conn) error) (bool, error) {
	err := s.transaction(true, fn)
	if err == errRollback {
		return false, nil
	}
	return err == nil, err
}

func reservationRows(c *sql.Conn, repository string) ([]PRReservation, error) {
	rows, err := c.QueryContext(background, "SELECT task_id,repository,branch,admitted_at FROM pr_reservations WHERE repository=?1", strings.ToLower(repository))
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r *PRReservation) []any { return []any{&r.TaskID, &r.Repository, &r.Branch, &r.AdmittedAt} })
}

// insertReservation records a PR slot for a task; orIgnore keeps an existing reservation instead of failing.
func insertReservation(c *sql.Conn, taskID, repository, branch, admittedAt string, orIgnore bool) error {
	verb := "INSERT"
	if orIgnore {
		verb = "INSERT OR IGNORE"
	}
	_, err := c.ExecContext(background, verb+" INTO pr_reservations(task_id,repository,branch,admitted_at) VALUES (?1,?2,?3,?4)", taskID, repository, branch, admittedAt)
	return err
}

func releaseReservation(c *sql.Conn, taskID string) error {
	_, err := c.ExecContext(background, "DELETE FROM pr_reservations WHERE task_id=?1", taskID)
	return err
}

func savedInventory(c *sql.Conn) (*model.OpenPRInventory, error) {
	var inventory model.OpenPRInventory
	found, err := txGet(c, "settings", "pr_inventory", &inventory)
	if err != nil || !found {
		return nil, err
	}
	return &inventory, nil
}

func (s *Store) OpenPRInventory() (*model.OpenPRInventory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return savedInventory(s.conn)
}

func PRUnion(inventory model.OpenPRInventory, reservations []PRReservation, limit uint64) (uint64, uint64, uint64) {
	numbers := map[uint64]struct{}{}
	for _, p := range inventory.PRs {
		if p.OwnedOpen() {
			numbers[p.Number] = struct{}{}
		}
	}
	branches := inventory.OwnedBranches()
	unrepresented := uint64(0)
	for _, r := range reservations {
		if _, ok := branches[r.Branch]; !ok {
			unrepresented++
		}
	}
	observed := uint64(len(numbers))
	remaining := uint64(0)
	if limit > observed+unrepresented {
		remaining = limit - observed - unrepresented
	}
	return observed, unrepresented, remaining
}

func (s *Store) HasPRReservation(taskID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var one int64
	err := s.conn.QueryRowContext(background, "SELECT 1 FROM pr_reservations WHERE task_id=?1", taskID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) PRReservations(repository string) ([]PRReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return reservationRows(s.conn, repository)
}

func (s *Store) ReservableTasks() ([]model.Task, error) {
	return listRecords[model.Task](s, fmt.Sprintf(`SELECT r.data FROM `+fromMeta+`
                WHERE m.kind='task' AND (
                    m.status IN (%s)
                    OR (m.status='queued' AND json_extract(r.data,'$.execution_session') IS NOT NULL)
                    OR (m.status!='published' AND json_extract(r.data,'$.output_commit') IS NOT NULL)
                )`, statusList(model.ActiveStatuses())))
}

func (s *Store) SeedPRReservation(task model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return insertReservation(s.conn, task.ID, strings.ToLower(task.Config.GitHubRepo), task.Branch, model.Now(), true)
}

func (s *Store) AdmitNewPRTask(task *model.Task, inventory model.OpenPRInventory) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next model.Task
	admitted, err := s.conditional(func(c *sql.Conn) error {
		cfg, err := storedConfig(c)
		if err != nil {
			return err
		}
		if !config.EqualASCII(inventory.Repository, cfg.GitHubRepo) {
			return errRollback
		}
		saved, err := savedInventory(c)
		if err != nil {
			return err
		}
		if saved == nil || !wirejson.Equal(*saved, inventory) {
			return errRollback
		}
		if task.Proposal.Target != task.Config.DefaultBranch || !config.SamePRPolicy(task.Config, cfg) {
			return errRollback
		}
		var canonical model.Task
		found, err := txGet(c, "task", task.ID, &canonical)
		if err != nil {
			return err
		}
		if !found || canonical.Status != model.StatusQueued || !wirejson.Equal(canonical, *task) {
			return errRollback
		}
		reservations, err := reservationRows(c, cfg.GitHubRepo)
		if err != nil {
			return err
		}
		if _, _, remaining := PRUnion(inventory, reservations, cfg.MaxOpenPRs); remaining == 0 {
			return errRollback
		}
		next = task.Clone()
		next.Status = model.StatusExecuting
		next.UpdatedAt = model.Now()
		if err := txPut(c, "task", next.ID, next); err != nil {
			return err
		}
		if err := insertReservation(c, next.ID, strings.ToLower(cfg.GitHubRepo), next.Branch, model.Now(), false); err != nil {
			return err
		}
		return txEvent(c, next.ID, "status", "Executing")
	})
	if admitted {
		*task = next
	}
	return admitted, err
}

func (s *Store) PersistPRInventory(inventory model.OpenPRInventory, released []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conditional(func(c *sql.Conn) error {
		cfg, err := storedConfig(c)
		if err != nil {
			return err
		}
		if !config.EqualASCII(inventory.Repository, cfg.GitHubRepo) {
			return errRollback
		}
		existing, err := savedInventory(c)
		if err != nil {
			return err
		}
		if existing != nil && config.EqualASCII(existing.Repository, inventory.Repository) {
			previous, err := time.Parse(time.RFC3339, existing.ObservedAt)
			if err != nil {
				return fmt.Errorf("Saved PR inventory timestamp is invalid: %s", invalidTimestamp)
			}
			candidate, err := time.Parse(time.RFC3339, inventory.ObservedAt)
			if err != nil {
				return fmt.Errorf("PR inventory timestamp is invalid: %s", invalidTimestamp)
			}
			if candidate.Before(previous) {
				return errRollback
			}
		}
		if err := txPut(c, "settings", "pr_inventory", inventory); err != nil {
			return err
		}
		represented := inventory.OwnedBranches()
		reservations, err := reservationRows(c, cfg.GitHubRepo)
		if err != nil {
			return err
		}
		for _, reservation := range reservations {
			if slices.Contains(released, reservation.TaskID) {
				var task model.Task
				found, err := txGet(c, "task", reservation.TaskID, &task)
				if err != nil {
					return err
				}
				confirmed := found && task.OutputCommit != nil && (task.Status == model.StatusPublished || task.Status == model.StatusCancelled)
				if confirmed {
					if err := releaseReservation(c, reservation.TaskID); err != nil {
						return err
					}
					continue
				}
			}
			if _, ok := represented[reservation.Branch]; ok {
				var one int64
				err := c.QueryRowContext(background, "SELECT 1 FROM record_meta WHERE kind='task' AND id=?1 AND status='published'", reservation.TaskID).Scan(&one)
				if err != nil && err != sql.ErrNoRows {
					return err
				}
				if err == nil {
					if err := releaseReservation(c, reservation.TaskID); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

const invalidTimestamp = "input contains invalid characters"
