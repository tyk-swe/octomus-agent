package store

import (
	"bytes"
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

// PrReservation holds one admitted default-branch task's share of the owned-PR
// limit until its pull request is observed or the task can no longer publish.
type PrReservation struct {
	TaskID     string
	Repository string
	Branch     string
	AdmittedAt string
}

// PrIdentity is the configuration a PR inventory was observed under. A task
// admitted under a different identity cannot consume that inventory's capacity.
type PrIdentity struct {
	Repository    string
	GitHubRepo    string
	DefaultBranch string
	BranchPrefix  string
}

func PrIdentityOf(c config.Config) PrIdentity {
	return PrIdentity{Repository: c.Repository, GitHubRepo: strings.ToLower(c.GitHubRepo), DefaultBranch: c.DefaultBranch, BranchPrefix: c.BranchPrefix}
}
func (p PrIdentity) Matches(c config.Config) bool {
	return p.Repository == c.Repository && config.EqualASCII(c.GitHubRepo, p.GitHubRepo) && p.DefaultBranch == c.DefaultBranch && p.BranchPrefix == c.BranchPrefix
}

// errRollback aborts a transaction that ends without a caller-visible error.
var errRollback = errors.New("rollback")

func reservationRows(c *sql.Conn, repository string) ([]PrReservation, error) {
	rows, err := c.QueryContext(background, "SELECT task_id,repository,branch,admitted_at FROM pr_reservations WHERE repository=?1", strings.ToLower(repository))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reservations := []PrReservation{}
	for rows.Next() {
		var r PrReservation
		if err := rows.Scan(&r.TaskID, &r.Repository, &r.Branch, &r.AdmittedAt); err != nil {
			return nil, err
		}
		reservations = append(reservations, r)
	}
	return reservations, rows.Err()
}

// insertReservation records the admission reservation for a task that does not hold one yet.
func insertReservation(c *sql.Conn, taskID, repository, branch, admittedAt string) error {
	_, err := c.ExecContext(background, "INSERT INTO pr_reservations(task_id,repository,branch,admitted_at) VALUES (?1,?2,?3,?4)", taskID, repository, branch, admittedAt)
	return err
}

// seedReservation seeds the reservation for a task that may already hold one.
func seedReservation(c *sql.Conn, taskID, repository, branch, admittedAt string) error {
	_, err := c.ExecContext(background, "INSERT OR IGNORE INTO pr_reservations(task_id,repository,branch,admitted_at) VALUES (?1,?2,?3,?4)", taskID, repository, branch, admittedAt)
	return err
}

// releaseReservation drops the reservation of a task that can no longer publish.
func releaseReservation(c *sql.Conn, taskID string) error {
	_, err := c.ExecContext(background, "DELETE FROM pr_reservations WHERE task_id=?1", taskID)
	return err
}

func savedInventory(c *sql.Conn) (*model.OpenPrInventory, error) {
	var inventory model.OpenPrInventory
	found, err := txGet(c, "settings", "pr_inventory", &inventory)
	if err != nil || !found {
		return nil, err
	}
	return &inventory, nil
}

// OpenPrInventory returns the latest complete persisted observation. The
// scheduler still requires its own fresh-process authority before admission.
func (s *Store) OpenPrInventory() (*model.OpenPrInventory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return savedInventory(s.conn)
}

// PrUnion combines the observed owned-open inventory with reservations no open
// PR represents yet: (observed, unrepresented reservations, remaining).
func PrUnion(inventory model.OpenPrInventory, reservations []PrReservation, limit uint64) (uint64, uint64, uint64) {
	numbers := map[uint64]struct{}{}
	branches := map[string]struct{}{}
	for _, p := range inventory.PRs {
		if p.OwnedOpen() {
			numbers[p.Number] = struct{}{}
			branches[p.Branch] = struct{}{}
		}
	}
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

func (s *Store) HasPrReservation(taskID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var one int64
	err := s.conn.QueryRowContext(background, "SELECT 1 FROM pr_reservations WHERE task_id=?1", taskID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) PrReservations(repository string) ([]PrReservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return reservationRows(s.conn, repository)
}

// PrReservationCandidates lists tasks that may still hold or need a reservation.
func (s *Store) PrReservationCandidates() ([]model.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, fmt.Sprintf(`SELECT r.data FROM record_meta m JOIN records r ON r.kind='task' AND r.id=m.id
                WHERE m.kind='task' AND (
                    m.status IN (%s)
                    OR (m.status='queued' AND json_extract(r.data,'$.execution_session') IS NOT NULL)
                    OR (m.status!='published' AND json_extract(r.data,'$.output_commit') IS NOT NULL)
                )`, statusList(model.ActiveStatuses())))
	if err != nil {
		return nil, err
	}
	return decodeTasks(raw)
}

func (s *Store) SeedPrReservation(task model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return seedReservation(s.conn, task.ID, strings.ToLower(task.Config.GitHubRepo), task.Branch, model.Now())
}

// AdmitNewPrTask moves a queued default-branch task to executing when the saved
// inventory still matches the caller's and capacity remains, reserving its PR
// slot in the same transaction. The task is updated in place on success.
func (s *Store) AdmitNewPrTask(task *model.Task, inventory model.OpenPrInventory) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	admitted := false
	var next model.Task
	err := s.transaction(true, func(c *sql.Conn) error {
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
		if saved == nil || !sameJSON(*saved, inventory) {
			return errRollback
		}
		if task.Proposal.Target != task.Config.DefaultBranch || !PrIdentityOf(task.Config).Matches(cfg) {
			return errRollback
		}
		var canonical model.Task
		found, err := txGet(c, "task", task.ID, &canonical)
		if err != nil {
			return err
		}
		if !found || canonical.Status != model.StatusQueued || !sameJSON(canonical, *task) {
			return errRollback
		}
		reservations, err := reservationRows(c, cfg.GitHubRepo)
		if err != nil {
			return err
		}
		if _, _, remaining := PrUnion(inventory, reservations, cfg.MaxOpenPRs); remaining == 0 {
			return errRollback
		}
		next = task.Clone()
		next.Status = model.StatusExecuting
		next.UpdatedAt = model.Now()
		if err := txPut(c, "task", next.ID, next); err != nil {
			return err
		}
		if err := insertReservation(c, next.ID, strings.ToLower(cfg.GitHubRepo), next.Branch, model.Now()); err != nil {
			return err
		}
		if err := txEvent(c, next.ID, "status", "Executing"); err != nil {
			return err
		}
		admitted = true
		return nil
	})
	if err == errRollback {
		return false, nil
	}
	if err == nil && admitted {
		*task = next
	}
	return admitted, err
}

// PersistPrInventory saves a newer inventory observation and releases
// reservations the caller closed or whose PRs are now published.
func (s *Store) PersistPrInventory(inventory model.OpenPrInventory, released []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.transaction(true, func(c *sql.Conn) error {
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
		represented := map[string]struct{}{}
		for _, p := range inventory.PRs {
			if p.OwnedOpen() {
				represented[p.Branch] = struct{}{}
			}
		}
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
	if err == errRollback {
		return false, nil
	}
	return err == nil, err
}

// sameJSON reports whether two values have the same canonical wirejson
// serialization. A value that cannot be serialized never matches.
func sameJSON(a, b any) bool {
	left, err := wirejson.Marshal(a)
	if err != nil {
		return false
	}
	right, err := wirejson.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(left, right)
}

// invalidTimestamp is the operator-facing reason for an unparseable inventory
// timestamp, kept short and free of Go's parse-layout diagnostics.
const invalidTimestamp = "input contains invalid characters"

func decodeTasks(raw [][]byte) ([]model.Task, error) {
	tasks := make([]model.Task, 0, len(raw))
	for _, data := range raw {
		var task model.Task
		if err := decodeJSON(data, &task); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}
