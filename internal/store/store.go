package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	_ "modernc.org/sqlite"
)

type Admission struct {
	ID      string       `json:"id"`
	At      string       `json:"at"`
	CycleID string       `json:"cycle_id"`
	TaskID  *string      `json:"task_id"`
	Role    string       `json:"role"`
	Route   config.Route `json:"route"`
}

func NewAdmission(cycleID string, taskID *string, role string, route config.Route) Admission {
	var task *string
	if taskID != nil {
		copied := *taskID
		task = &copied
	}
	return Admission{ID: model.ID(), At: model.Now(), CycleID: cycleID, TaskID: task, Role: role, Route: route.Clone()}
}
func (v *Admission) UnmarshalJSON(data []byte) error { return wirejson.DecodeRecord(data, v) }
func (v Admission) MarshalJSON() ([]byte, error) {
	type plain Admission
	return wirejson.Record(plain(v))
}

var background = context.Background()

// fromMeta joins the indexed projection of a record to the record itself.
const fromMeta = "record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id"

type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	conn *sql.Conn
	path string
}

func dsn(path string, params string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + params
}

func Open(path string) (*Store, error) {
	// Journal and synchronous pragmas run only after the schema-version check, so a refused database is never modified.
	db, err := sql.Open("sqlite", dsn(path, "_pragma=busy_timeout(5000)"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	conn, err := db.Conn(background)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, conn: conn, path: path}
	if err := s.initialize(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	fresh, err := schemaStatus(background, s.conn)
	if err != nil {
		return err
	}
	if _, err := s.conn.ExecContext(background, "PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;"); err != nil {
		return err
	}
	if fresh {
		return createSchema(background, s.conn)
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	if s.conn != nil {
		first = s.conn.Close()
		s.conn = nil
	}
	if s.db != nil {
		if err := s.db.Close(); first == nil {
			first = err
		}
		s.db = nil
	}
	return first
}

type ReadOnly struct {
	Conn *sql.Conn
	db   *sql.DB
}

func OpenReadOnly(path, what string) (*ReadOnly, error) {
	fail := func(err error) error {
		return fmt.Errorf("Cannot open existing state database for read-only %s: %w", what, err)
	}
	// Refuse before the driver touches the path: a read-only open must not leave a data directory or empty database behind.
	if info, err := os.Stat(path); err != nil {
		return nil, fail(err)
	} else if info.IsDir() {
		return nil, fail(fmt.Errorf("%s is a directory", path))
	}
	db, err := sql.Open("sqlite", dsn(path, "mode=ro&_pragma=busy_timeout(5000)"))
	if err != nil {
		return nil, fail(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(background)
	if err != nil {
		db.Close()
		return nil, fail(err)
	}
	if err := requireSchema(background, conn); err != nil {
		conn.Close()
		db.Close()
		return nil, err
	}
	return &ReadOnly{Conn: conn, db: db}, nil
}

func (r *ReadOnly) Close() error {
	err := r.Conn.Close()
	if closeErr := r.db.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (r *ReadOnly) Snapshot(fn func(c *sql.Conn) error) error {
	return runTx(r.Conn, "BEGIN", fn)
}

func (s *Store) transaction(immediate bool, fn func(c *sql.Conn) error) error {
	begin := "BEGIN"
	if immediate {
		begin = "BEGIN IMMEDIATE"
	}
	return runTx(s.conn, begin, fn)
}

// runTx rolls back on any error, failed COMMIT or panic, so the connection never stays inside a silent open transaction.
func runTx(c *sql.Conn, begin string, fn func(c *sql.Conn) error) error {
	if _, err := c.ExecContext(background, begin); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = c.ExecContext(background, "ROLLBACK")
		}
	}()
	if err := fn(c); err != nil {
		return err
	}
	if _, err := c.ExecContext(background, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *Store) Put(kind, id string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return txPut(s.conn, kind, id, value)
}

func (s *Store) SaveControl(control model.Control) error {
	return s.Put("settings", "control", control)
}

func (s *Store) ClearCancel(id string) error { return s.Put("cancel", id, nil) }

func (s *Store) MarkCancel(id string) error { return s.Put("cancel", id, model.Now()) }

// Marked reports whether a marker record exists and is not JSON null.
func (s *Store) Marked(kind, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var marked bool
	err := s.conn.QueryRowContext(background, "SELECT data != 'null' FROM records WHERE kind=?1 AND id=?2", kind, id).Scan(&marked)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return marked, err
}

// CommitPlan uses ctx only for admission after acquiring the store mutex.
// Once admitted, cancellation does not interrupt the plan transaction: it finishes
// atomically, like other store writes.
func (s *Store) CommitPlan(ctx context.Context, cycle model.Cycle, tasks []model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.transaction(false, func(c *sql.Conn) error {
		if err := txPut(c, "cycle", cycle.ID, cycle); err != nil {
			return err
		}
		for _, task := range tasks {
			if err := txPut(c, "task", task.ID, task); err != nil {
				return err
			}
		}
		var control model.Control
		hasControl, err := txGet(c, "settings", "control", &control)
		if err != nil {
			return err
		}
		controlChanged := false
		if hasControl && control.Batch != nil && cycle.RunID != nil && *cycle.RunID == control.Batch.ID && control.Batch.CycleID != nil && *control.Batch.CycleID == cycle.ID {
			control.Batch.Phase = model.BatchPhaseExecuting
			controlChanged = true
		}
		for _, task := range tasks {
			for _, oldID := range task.Supersedes {
				err := updateLineageTask(c, oldID, func(old *model.Task) error {
					if !(old.RediscoveryRequested && config.EqualASCII(old.Config.GitHubRepo, task.Config.GitHubRepo)) {
						return errors.New("Invalid rediscovery lineage")
					}
					old.SupersededBy = append(old.SupersededBy, task.ID)
					return nil
				})
				if err != nil {
					return err
				}
			}
		}
		for _, decision := range cycle.DecisionMemory {
			if decision.ID == "" {
				return errors.New("Missing decision identity")
			}
			if err := txPut(c, "decision", decision.ID, decision); err != nil {
				return err
			}
		}
		if cycle.Mode == model.CycleModeExecution {
			for _, proposal := range cycle.Proposals {
				for _, id := range proposal.Reconsiders {
					err := updateLineageTask(c, id, func(old *model.Task) error {
						if !(old.RediscoveryRequested && old.Status == model.StatusCancelled && config.EqualASCII(old.Config.GitHubRepo, cycle.Repository) && old.Proposal.Target == proposal.Target) {
							return errors.New("Rediscovery decisions must reference an eligible request in this repository and target")
						}
						old.RediscoveryRequested = false
						result := fmt.Sprintf("%s: %s", proposal.Decision, proposal.Reason)
						old.RediscoveryResult = &result
						return nil
					})
					if err != nil {
						return err
					}
				}
			}
			if hasControl {
				if len(tasks) == 0 {
					if control.IdleStreak < ^uint32(0) {
						control.IdleStreak++
					}
				} else {
					control.IdleStreak = 0
				}
				controlChanged = true
			}
		}
		if controlChanged {
			return txPut(c, "settings", "control", control)
		}
		return nil
	})
}

func (s *Store) AppendCycleSession(cycleID string, session model.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cycle model.Cycle
	found, err := txGet(s.conn, "cycle", cycleID, &cycle)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Missing cycle %s", cycleID)
	}
	for _, existing := range cycle.Sessions {
		if existing.ID == session.ID {
			return nil
		}
	}
	cycle.Sessions = append(cycle.Sessions, session.Clone())
	return txPut(s.conn, "cycle", cycleID, cycle)
}

// Get returns the saved record, or nil when none exists.
func Get[T any](s *Store, kind, id string) (*T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RecordAt[T](s.conn, kind, id)
}

func List[T any](s *Store, kind string) ([]T, error) {
	return listRecords[T](s, "SELECT data FROM records WHERE kind=?1 ORDER BY rowid DESC", kind)
}

func RecordAt[T any](c *sql.Conn, kind, id string) (*T, error) {
	var value T
	found, err := txGet(c, kind, id, &value)
	if err != nil || !found {
		return nil, err
	}
	return &value, nil
}

func QueryRecords[T any](c *sql.Conn, query string, args ...any) ([]T, error) {
	rows, err := c.QueryContext(background, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []T{}
	var data sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		value, err := decodeRecord[T](data)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func listRecords[T any](s *Store, query string, args ...any) ([]T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return QueryRecords[T](s.conn, query, args...)
}

func (s *Store) Event(entity, kind, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return txEvent(s.conn, entity, kind, message)
}

func txEvent(c *sql.Conn, entity, kind, message string) error {
	_, err := c.ExecContext(background, "INSERT INTO events(at,entity_id,kind,message) VALUES (?1,?2,?3,?4)", model.Now(), entity, kind, redact.Text(message))
	return err
}

func (s *Store) Events(entity *string) ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return queryEvents(s.conn, "SELECT id,at,entity_id,kind,message FROM events WHERE (?1 IS NULL OR entity_id=?1) ORDER BY id DESC LIMIT 200", entity)
}

func queryEvents(c *sql.Conn, query string, args ...any) ([]model.Event, error) {
	rows, err := c.QueryContext(background, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []model.Event{}
	for rows.Next() {
		var e model.Event
		if err := rows.Scan(&e.ID, &e.At, &e.EntityID, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) PruneEvents(retain int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.ExecContext(background, "DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?1)", retain)
	return err
}

func (s *Store) ReserveSession(measuredBytes uint64, admission Admission) error {
	at, err := time.Parse(time.RFC3339, admission.At)
	if err != nil {
		return err
	}
	day := model.UTCDay(at)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		cfg, err := storedConfig(c)
		if err != nil {
			return err
		}
		limit := cfg.MaxSessionsPerDay
		if limit == 0 {
			return errors.New("Daily session budget must be positive")
		}
		if measuredBytes >= cfg.MaxWorkspaceBytes {
			return StorageLimitError(measuredBytes)
		}
		if limit > uint64(1<<63-1) {
			limit = 1<<63 - 1
		}
		result, err := c.ExecContext(background, `INSERT INTO usage(day,sessions) VALUES (?1,1)
             ON CONFLICT(day) DO UPDATE SET sessions=sessions+1 WHERE sessions < ?2`, day, int64(limit))
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return fmt.Errorf("Daily session budget exhausted; increase the configured limit or wait until UTC midnight: %w", model.BlockedBudgetExhausted)
		}
		data, err := wirejson.Marshal(admission)
		if err != nil {
			return err
		}
		_, err = c.ExecContext(background, "INSERT INTO admissions(id,at,day,data) VALUES (?1,?2,?3,?4)", admission.ID, admission.At, day, string(data))
		return err
	})
}

// Admissions is the whole admission ledger in order, for the usage report.
func Admissions(c *sql.Conn) ([]Admission, error) {
	return QueryRecords[Admission](c, "SELECT data FROM admissions ORDER BY at,id")
}

// DaySessions is one day's durable session counter.
type DaySessions struct {
	Day      string
	Sessions uint64
}

// DailySessions is every day's session counter in day order, for the usage report.
func DailySessions(c *sql.Conn) ([]DaySessions, error) {
	rows, err := c.QueryContext(background, "SELECT day,sessions FROM usage ORDER BY day")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := []DaySessions{}
	for rows.Next() {
		var day DaySessions
		if err := rows.Scan(&day.Day, &day.Sessions); err != nil {
			return nil, err
		}
		days = append(days, day)
	}
	return days, rows.Err()
}

func sessionsOn(c *sql.Conn, day string) (int64, error) {
	var sessions int64
	err := c.QueryRowContext(background, "SELECT sessions FROM usage WHERE day=?1", day).Scan(&sessions)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return sessions, err
}

func (s *Store) PlanningCapacity(at time.Time) (model.PlanningCapacity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return planningCapacityAt(s.conn, at)
}

func planningCapacityAt(c *sql.Conn, at time.Time) (model.PlanningCapacity, error) {
	at = at.UTC()
	day := model.UTCDay(at)
	cfg, err := storedConfig(c)
	if err != nil {
		return model.PlanningCapacity{}, err
	}
	used, err := sessionsOn(c, day)
	if err != nil {
		return model.PlanningCapacity{}, err
	}
	if used < 0 {
		used = 0
	}
	limit := cfg.MaxSessionsPerDay
	required := cfg.PlanningCost()
	remaining := uint64(0)
	if limit > uint64(used) {
		remaining = limit - uint64(used)
	}
	nextReset := time.Date(at.Year(), at.Month(), at.Day()+1, 0, 0, 0, 0, time.UTC).Unix()
	status := model.CapacityReady
	if limit < required {
		status = model.CapacityTooLow
	} else if remaining < required {
		status = model.CapacityExhausted
	}
	return model.PlanningCapacity{Day: day, Limit: limit, Used: uint64(used), Remaining: remaining, Required: required, NextResetAt: nextReset, Status: status}, nil
}

func (s *Store) CancelTask(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.conn.ExecContext(background, `UPDATE records SET data=json_set(data,'$.status','cancelled','$.updated_at',?2)
             WHERE kind='task' AND id=?1
               AND json_extract(data,'$.status') NOT IN ('publishing','published')
               AND json_extract(data,'$.output_commit') IS NULL`, id, model.Now())
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func storedConfig(c *sql.Conn) (config.Config, error) {
	cfg := config.Default()
	if _, err := txGet(c, "settings", "config", &cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func txGet(c *sql.Conn, kind, id string, dst any) (bool, error) {
	var data string
	err := c.QueryRowContext(background, "SELECT data FROM records WHERE kind=?1 AND id=?2", kind, id).Scan(&data)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := decodeJSON([]byte(data), dst); err != nil {
		return true, fmt.Errorf("Saved %s %s is unreadable: %w", kind, id, err)
	}
	return true, nil
}

func txPut(c *sql.Conn, kind, id string, value any) error {
	data, err := wirejson.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(background, `INSERT INTO records VALUES (?1,?2,?3)
         ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data`, kind, id, string(data))
	return err
}

func updateLineageTask(c *sql.Conn, id string, check func(*model.Task) error) error {
	var task model.Task
	found, err := txGet(c, "task", id, &task)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Missing lineage task %s", id)
	}
	if task.Lifecycle.ArchivedAt != nil {
		return fmt.Errorf("Rediscovery request %s was archived before the plan committed", id)
	}
	if err := check(&task); err != nil {
		return err
	}
	return txPut(c, "task", id, task)
}

func queryStrings(c *sql.Conn, query string, args ...any) ([][]byte, error) {
	rows, err := c.QueryContext(background, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		out = append(out, []byte(data))
	}
	return out, rows.Err()
}

func decodeJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func decodeRecord[T any](data []byte) (T, error) {
	var value T
	if err := decodeJSON(data, &value); err != nil {
		return value, fmt.Errorf("Saved record %s is unreadable: %w", savedRecordID(data), err)
	}
	return value, nil
}

func savedRecordID(data []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(bytes.NewReader(data)).Decode(&probe) != nil || probe.ID == "" {
		return "(unknown id)"
	}
	return probe.ID
}

func StorageLimitError(measuredBytes uint64) error {
	return fmt.Errorf("Workspace storage limit reached (%d bytes). Resolve retained tasks or increase the limit: %w", measuredBytes, model.BlockedStorageLimit)
}
