// Package store owns the SQLite state database: JSON records, indexed projections and the transactions that
// keep admissions, plans, lineage and reservations all-or-nothing.
//
// One process holds one connection. The service store pins a single physical
// connection for its lifetime and serializes every method on a mutex.
// Read-only reporting opens its own connection with SQLITE_OPEN_READONLY.
package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
	_ "modernc.org/sqlite"
)

// WebhookEnv names the notification destination variable; its value is a secret.
const WebhookEnv = "OCTOMUS_NOTIFICATION_WEBHOOK_URL"

// Admission is a budget admission, not a completed turn or a provider charge.
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
func (v *Admission) UnmarshalJSON(data []byte) error {
	type plain Admission
	decoded := plain{}
	if err := wirejson.Decode(data, &decoded, false, false); err != nil {
		return err
	}
	*v = Admission(decoded)
	return nil
}
func (v Admission) MarshalJSON() ([]byte, error) {
	type plain Admission
	return wirejson.Record(plain(v))
}

var background = context.Background()

// Background is the context every store statement runs under; read-only
// callers outside the package use it for their own snapshot queries.
func Background() context.Context { return background }

// Store is the service's single writable handle on the state database.
type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	conn *sql.Conn
	path string
}

// dsn builds a SQLite URI for path with the connection settings every physical
// connection needs. The driver applies `_pragma` values on each connect, so a
// replaced connection can never run without the busy timeout, WAL or FULL sync.
func dsn(path string, params string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + params
}

// Open creates fresh state or opens an existing version-7 database at path.
func Open(path string) (*Store, error) {
	// Only the busy timeout is applied at connect time: the journal mode and
	// synchronous setting follow the schema-version check so a database this
	// executable must refuse is never modified. The store pins its single
	// physical connection for its whole lifetime, so the settings run on every
	// connection it ever uses.
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

// Path is the database file this store opened.
func (s *Store) Path() string { return s.path }

// Close releases the pinned connection. The store is unusable afterwards.
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

// ReadOnly is a reporting connection that cannot migrate, create directories or
// take the service lock. Callers own its transactions.
type ReadOnly struct {
	Conn *sql.Conn
	db   *sql.DB
}

// OpenReadOnly opens an existing state database with SQLITE_OPEN_READONLY.
// `what` names the caller in the failure so operators see which path refused.
func OpenReadOnly(path, what string) (*ReadOnly, error) {
	fail := func(err error) error {
		return fmt.Errorf("Cannot open existing state database for read-only %s: %w", what, err)
	}
	// Refuse before the driver touches the path: a read-only open of a missing
	// file must never leave the data directory or an empty database behind.
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

// Snapshot runs fn inside one deferred read transaction so every query observes
// the same committed state, including pages still in the WAL.
func (r *ReadOnly) Snapshot(fn func(c *sql.Conn) error) error {
	if _, err := r.Conn.ExecContext(background, "BEGIN"); err != nil {
		return err
	}
	if err := fn(r.Conn); err != nil {
		_, _ = r.Conn.ExecContext(background, "ROLLBACK")
		return err
	}
	_, err := r.Conn.ExecContext(background, "COMMIT")
	return err
}

// transaction runs fn between BEGIN [IMMEDIATE] and COMMIT on the pinned
// connection, rolling back on any error. The caller holds the store mutex.
func (s *Store) transaction(immediate bool, fn func(c *sql.Conn) error) error {
	begin := "BEGIN"
	if immediate {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err := s.conn.ExecContext(background, begin); err != nil {
		return err
	}
	if err := fn(s.conn); err != nil {
		_, _ = s.conn.ExecContext(background, "ROLLBACK")
		return err
	}
	if _, err := s.conn.ExecContext(background, "COMMIT"); err != nil {
		_, _ = s.conn.ExecContext(background, "ROLLBACK")
		return err
	}
	return nil
}

// Put upserts one canonical JSON record.
func (s *Store) Put(kind, id string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return txPut(s.conn, kind, id, value)
}

// SaveControl persists the operator control record; the single durable scheduling state.
func (s *Store) SaveControl(control model.Control) error {
	return s.Put("settings", "control", control)
}

// ClearCancel clears an operator-cancel marker without touching the task record.
func (s *Store) ClearCancel(id string) error { return s.Put("cancel", id, nil) }

// MarkCancel writes the durable operator-cancel marker: the running task never
// writes this kind, so its final save cannot clobber the intent.
func (s *Store) MarkCancel(id string) error { return s.Put("cancel", id, model.Now()) }

// MarkerSet reports whether an operator marker is set. Clearing writes a JSON
// null rather than deleting the row, so a present row is not enough.
func (s *Store) MarkerSet(kind, id string) (bool, error) {
	value, found, err := s.GetValue(kind, id)
	return found && value != nil, err
}

// CommitPlan saves a planned cycle with its tasks, lineage updates, decision
// memory and control transitions in one transaction.
func (s *Store) CommitPlan(cycle model.Cycle, tasks []model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if found, err := txGet(c, "settings", "control", &control); err != nil {
			return err
		} else if found && control.Batch != nil && cycle.RunID != nil && *cycle.RunID == control.Batch.ID && control.Batch.CycleID != nil && *control.Batch.CycleID == cycle.ID {
			control.Batch.Phase = model.BatchPhaseExecuting
			if err := txPut(c, "settings", "control", control); err != nil {
				return err
			}
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
			object, _ := decision.(map[string]any)
			id, ok := object["id"].(string)
			if !ok {
				return errors.New("Missing decision identity")
			}
			if err := txPut(c, "decision", id, decision); err != nil {
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
			var control model.Control
			if found, err := txGet(c, "settings", "control", &control); err != nil {
				return err
			} else if found {
				if len(tasks) == 0 {
					if control.IdleStreak < ^uint32(0) {
						control.IdleStreak++
					}
				} else {
					control.IdleStreak = 0
				}
				if err := txPut(c, "settings", "control", control); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// AppendCycleSession appends a terminal planning session to the cycle record
// under the store lock. Role evidence becomes durable before its workspace may
// be cleaned up, and concurrent roles merge per-session rather than overwriting
// a shared full-cycle snapshot.
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

// Get decodes one record into dst and reports whether it existed.
func (s *Store) Get(kind, id string, dst any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return txGet(s.conn, kind, id, dst)
}

// GetValue reads one record as generic JSON (numbers as json.Number).
func (s *Store) GetValue(kind, id string) (any, bool, error) {
	var value any
	found, err := s.Get(kind, id, &value)
	return value, found, err
}

// GetRaw reads one record's saved bytes verbatim.
func (s *Store) GetRaw(kind, id string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return txGetRaw(s.conn, kind, id)
}

// Get decodes one record of type T.
func Get[T any](s *Store, kind, id string) (*T, error) {
	var value T
	found, err := s.Get(kind, id, &value)
	if err != nil || !found {
		return nil, err
	}
	return &value, nil
}

// ListRaw returns every record of a kind, newest first.
func (s *Store) ListRaw(kind string) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return queryStrings(s.conn, "SELECT data FROM records WHERE kind=?1 ORDER BY rowid DESC", kind)
}

// List decodes every record of a kind, newest first.
func List[T any](s *Store, kind string) ([]T, error) {
	raw, err := s.ListRaw(kind)
	if err != nil {
		return nil, err
	}
	values := make([]T, 0, len(raw))
	for _, data := range raw {
		var value T
		if err := decodeJSON(data, &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

// Event appends a redacted operator-visible event.
func (s *Store) Event(entity, kind, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.ExecContext(background, "INSERT INTO events(at,entity_id,kind,message) VALUES (?1,?2,?3,?4)", model.Now(), entity, kind, Redact(message))
	return err
}

// Events lists the newest 200 events, optionally for one entity.
func (s *Store) Events(entity *string) ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.conn.QueryContext(background, "SELECT id,at,entity_id,kind,message FROM events WHERE (?1 IS NULL OR entity_id=?1) ORDER BY id DESC LIMIT 200", entity)
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

// PruneEvents keeps only the newest `retain` events.
func (s *Store) PruneEvents(retain int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.ExecContext(background, "DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?1)", retain)
	return err
}

// ReserveSession admits one session against the live daily budget and records
// the admission in the same write transaction. A failed ledger insert rolls
// back the counter increment too.
func (s *Store) ReserveSession(measuredBytes uint64, admission Admission) error {
	// Derive both timestamps from the same instant, including across UTC midnight.
	at, err := time.Parse(time.RFC3339, admission.At)
	if err != nil {
		return err
	}
	day := model.UTCDay(at)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		// Policy is read under the same write transaction as the reservation. Callers
		// cannot accidentally supply a queued task's historical admission limits.
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
			return fmt.Errorf("Daily session budget exhausted; increase the configured limit or wait until UTC midnight: %w", model.BlockedReasonBudgetExhausted)
		}
		data, err := wirejson.Marshal(admission)
		if err != nil {
			return err
		}
		_, err = c.ExecContext(background, "INSERT INTO admissions(id,at,day,data) VALUES (?1,?2,?3,?4)", admission.ID, admission.At, day, string(data))
		return err
	})
}

// SessionsToday is the admission counter for the current UTC day.
func (s *Store) SessionsToday() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sessions int64
	err := s.conn.QueryRowContext(background, "SELECT sessions FROM usage WHERE day=?1", model.Today()).Scan(&sessions)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return uint64(sessions), nil
}

// PlanningCapacity reports whether today's remaining budget funds a planning cycle.
func (s *Store) PlanningCapacity() (model.PlanningCapacity, error) {
	return s.PlanningCapacityAt(time.Now())
}

// PlanningCapacityAt is PlanningCapacity for the UTC day containing `at`.
func (s *Store) PlanningCapacityAt(at time.Time) (model.PlanningCapacity, error) {
	at = at.UTC()
	s.mu.Lock()
	capacity, err := planningCapacityAt(s.conn, at)
	s.mu.Unlock()
	return capacity, err
}

func planningCapacityAt(c *sql.Conn, at time.Time) (model.PlanningCapacity, error) {
	at = at.UTC()
	day := model.UTCDay(at)
	cfg, err := storedConfig(c)
	if err != nil {
		return model.PlanningCapacity{}, err
	}
	var used int64
	err = c.QueryRowContext(background, "SELECT sessions FROM usage WHERE day=?1", day).Scan(&used)
	if err == sql.ErrNoRows {
		used = 0
		err = nil
	}
	if err != nil {
		return model.PlanningCapacity{}, err
	}
	if used < 0 {
		used = 0
	}
	limit := cfg.MaxSessionsPerDay
	required := cfg.PlanningAdmissionsRequired()
	remaining := uint64(0)
	if limit > uint64(used) {
		remaining = limit - uint64(used)
	}
	nextReset := time.Date(at.Year(), at.Month(), at.Day()+1, 0, 0, 0, 0, time.UTC).Unix()
	status := model.PlanningCapacityStatusReady
	if limit < required {
		status = model.PlanningCapacityStatusLimitTooLow
	} else if remaining < required {
		status = model.PlanningCapacityStatusDailyExhausted
	}
	return model.PlanningCapacity{Day: day, Limit: limit, Used: uint64(used), Remaining: remaining, Required: required, NextResetAt: nextReset, Status: status}, nil
}

// CancelTask is the targeted status write for the operator-cancel path: a
// full-record save from a stale task copy could resurrect fields the running
// worker already updated. Publication checkpoints must remain recoverable even
// if the worker has already saved a final blocked status by the time
// cancellation reaches us.
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

// storedConfig reads the live operator config inside a caller-owned transaction
// or connection. Defaults when none is stored, matching Get callers.
func storedConfig(c *sql.Conn) (config.Config, error) {
	cfg := config.Default()
	if _, err := txGet(c, "settings", "config", &cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func txGetRaw(c *sql.Conn, kind, id string) ([]byte, bool, error) {
	var data string
	err := c.QueryRowContext(background, "SELECT data FROM records WHERE kind=?1 AND id=?2", kind, id).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(data), true, nil
}

// txGet reads one record inside a caller-owned transaction. dst is left alone
// when the record is absent.
func txGet(c *sql.Conn, kind, id string, dst any) (bool, error) {
	data, found, err := txGetRaw(c, kind, id)
	if err != nil || !found {
		return false, err
	}
	return true, decodeJSON(data, dst)
}

// txPut upserts one record inside a caller-owned transaction.
func txPut(c *sql.Conn, kind, id string, value any) error {
	data, err := wirejson.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(background, `INSERT INTO records VALUES (?1,?2,?3)
         ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data`, kind, id, string(data))
	return err
}

// updateLineageTask reads a task inside the commit transaction, checks its
// lineage eligibility and writes the caller's mutation back, all-or-nothing
// with the plan.
func updateLineageTask(c *sql.Conn, id string, check func(*model.Task) error) error {
	var task model.Task
	found, err := txGet(c, "task", id, &task)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Missing lineage task %s", id)
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

// decodeJSON reads saved JSON with exact numbers; generic destinations receive
// json.Number rather than float64 so re-encoding preserves the saved spelling.
// RedactedValue serializes a typed export, decodes it as generic JSON (numbers
// kept verbatim) and scrubs every string in place. Exports use it so redaction
// happens after the facts are computed from the saved records.
func RedactedValue(value any) (map[string]any, error) {
	data, err := wirejson.Marshal(value)
	if err != nil {
		return nil, err
	}
	var generic map[string]any
	if err := decodeJSON(data, &generic); err != nil {
		return nil, err
	}
	RedactJSON(generic)
	return generic, nil
}

func decodeJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing JSON data")
	}
	return nil
}

// ErrorMessage renders an error for an operator, with secrets scrubbed. Errors
// reach operators through saved records and API responses, so every stored
// error message is built here rather than formatted at each site.
func ErrorMessage(err error) string { return Redact(err.Error()) }

// StorageLimitError is the refusal an admission gets when the workspace already
// holds more bytes than the configured limit allows. Shared by task admission
// and the baseline check.
func StorageLimitError(measuredBytes uint64) error {
	return fmt.Errorf("Workspace storage limit reached (%d bytes). Resolve retained tasks or increase the limit: %w", measuredBytes, model.BlockedReasonStorageLimit)
}

// Whitespace includes Unicode White_Space, TAB–CR and NEL.
// Go's \s is ASCII-only, so use the equivalent class in every whitespace match.
const tokenWhitespace = `\p{Z}\x{0009}-\x{000D}\x{0085}`

var tokenPattern = regexp.MustCompile(`(?i)(bearer[` + tokenWhitespace + `]+)[A-Za-z0-9._~+/=-]+|(?:gh[pousr]_|github_pat_|sk-)[A-Za-z0-9_-]{10,}|[a-z]+://[^` + tokenWhitespace + `/@]+:[^` + tokenWhitespace + `/@]+@`)

var (
	secretsOnce sync.Once
	secrets     []string
)

func environmentSecrets() []string {
	secretsOnce.Do(func() {
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || len(value) < 8 {
				continue
			}
			if key == WebhookEnv || strings.Contains(key, "TOKEN") || strings.Contains(key, "SECRET") || strings.Contains(key, "PASSWORD") || strings.Contains(key, "API_KEY") {
				secrets = append(secrets, value)
			}
		}
	})
	return secrets
}

// RedactSecrets scrubs tokens and secret-bearing environment values without any
// length limit. Persisted results must be bounded by the caller so shortening is
// always flagged.
func RedactSecrets(input string) string {
	s := tokenPattern.ReplaceAllString(input, "[redacted]")
	for _, value := range environmentSecrets() {
		s = strings.ReplaceAll(s, value, "[redacted]")
	}
	return s
}

// Redact scrubs secrets and bounds the text to 16384 characters.
func Redact(input string) string {
	s := RedactSecrets(input)
	count := 0
	for i := range s {
		if count == 16384 {
			return s[:i]
		}
		count++
	}
	return s
}

// RedactJSON scrubs every string inside a generic JSON value in place.
func RedactJSON(value any) any {
	switch v := value.(type) {
	case string:
		return Redact(v)
	case []any:
		for i := range v {
			v[i] = RedactJSON(v[i])
		}
		return v
	case map[string]any:
		for k := range v {
			v[k] = RedactJSON(v[k])
		}
		return v
	default:
		return value
	}
}
