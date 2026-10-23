package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	// Repository is stamped by ReserveSession from the reserving handle's
	// scope; admissions saved before multi-repository state omit it.
	Repository string `json:"repository,omitempty" wire:"default"`
}

func NewAdmission(cycleID string, taskID *string, role string, route config.Route) Admission {
	var task *string
	if taskID != nil {
		task = new(*taskID)
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
	mu       *sync.Mutex
	db       *sql.DB
	conn     *sql.Conn
	path     string
	upgraded *Upgrade
	// repo scopes settings, budgets and list queries to one GitHub repository
	// ("owner/name"). Empty is the unscoped root handle: the registry, instance
	// settings and read-only exports live there, and legacy single-repository
	// state keeps its original record identities through it.
	repo string
	// primary is the first registered repository (or the legacy saved
	// configuration's), resolved at Open. Its settings keep the original
	// unsuffixed record identities; added repositories are suffixed.
	primary string
}

// Repository returns this handle's repository scope, or "" for the root handle.
func (s *Store) Repository() string { return s.repo }

// ForRepository returns a handle over the same database scoped to one repository.
// Handles share the connection and mutex, so admission transactions stay atomic
// across repositories. Scoping the root handle to "" returns the root handle.
func (s *Store) ForRepository(repo string) *Store {
	if repo == s.repo {
		return s
	}
	scoped := *s
	scoped.repo = repo
	return &scoped
}

// Upgraded reports the upgrade Open applied, or nil when it applied none.
func (s *Store) Upgraded() *Upgrade { return s.upgraded }

func dsn(path string, params string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + escaped + "?" + params
}

func Open(path string) (*Store, error) {
	return open(path, release)
}

func open(path string, plan schemaPlan) (*Store, error) {
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
	s := &Store{mu: &sync.Mutex{}, db: db, conn: conn, path: path}
	if err := s.initialize(plan); err != nil {
		s.Close()
		return nil, err
	}
	s.loadPrimary()
	return s, nil
}

// loadPrimary resolves the repository whose settings keep their legacy
// identities. Best-effort: an unreadable registry surfaces properly through
// Repositories, and scoped settings simply stay suffixed until then.
func (s *Store) loadPrimary() {
	if registry, err := recordAt(s.conn, repositoriesKey); err == nil && registry != nil && len(registry.Repositories) > 0 {
		s.primary = registry.Repositories[0].GitHubRepo
		return
	}
	if cfg, err := recordAt(s.conn, ConfigKey); err == nil && cfg != nil {
		s.primary = cfg.GitHubRepo
	}
}

func (s *Store) initialize(plan schemaPlan) error {
	version, fresh, err := inspect(background, s.conn, plan)
	if err != nil {
		return err
	}
	if !fresh && version < plan.version() {
		return s.migrate(plan, version)
	}
	if err := s.configureJournal(); err != nil {
		return err
	}
	if fresh {
		return createSchema(background, s.conn, plan)
	}
	return nil
}

func (s *Store) configureJournal() error {
	_, err := s.conn.ExecContext(background, "PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;")
	return err
}

func (s *Store) Close() error {
	// Scoped handles share the root's connection; only the root closes it.
	if s.repo != "" {
		return nil
	}
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
	conn *sql.Conn
	db   *sql.DB
}

func OpenReadOnly(path, what string) (*ReadOnly, error) {
	return openReadOnly(path, what, release)
}

func openReadOnly(path, what string, plan schemaPlan) (*ReadOnly, error) {
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
	if err := requireSchema(background, conn, plan, what); err != nil {
		conn.Close()
		db.Close()
		return nil, err
	}
	return &ReadOnly{conn: conn, db: db}, nil
}

func (r *ReadOnly) Close() error {
	err := r.conn.Close()
	if closeErr := r.db.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (r *ReadOnly) snapshot(fn func(c *sql.Conn) error) error {
	return runTx(r.conn, "BEGIN", fn)
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

func (s *Store) SaveControl(control model.Control) error {
	return Put(s, s.ControlKey(), control)
}

func (s *Store) ClearCancel(id string) error { return Put(s, cancelMarkers.Key(id), nil) }

func (s *Store) MarkCancel(id string) error {
	return Put(s, cancelMarkers.Key(id), new(model.Now()))
}

func (s *Store) CancelMarked(id string) (bool, error) { return s.marked(cancelMarkers.Key(id)) }

func (s *Store) MarkBaselineCancel(id string) error {
	return Put(s, baselineCancelMarkers.Key(id), new(model.Now()))
}

func (s *Store) BaselineCancelMarked(id string) (bool, error) {
	return s.marked(baselineCancelMarkers.Key(id))
}

// marked reports whether a marker record exists and is not JSON null.
func (s *Store) marked(key Key[*string]) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var marked bool
	err := s.conn.QueryRowContext(background, "SELECT data != 'null' FROM records WHERE kind=?1 AND id=?2", key.kind, key.id).Scan(&marked)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return marked, err
}

// CommitPlan uses ctx only for admission after acquiring the store mutex.
// Once admitted, cancellation does not interrupt the plan transaction: it finishes
// atomically, like other store writes.
func (s *Store) CommitPlan(ctx context.Context, cycle model.Cycle, tasks []model.Task, requests []RequestUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.transaction(false, func(c *sql.Conn) error {
		if err := txPut(c, Cycles.Key(cycle.ID), cycle); err != nil {
			return err
		}
		for _, task := range tasks {
			if err := insertTask(c, task); err != nil {
				return err
			}
		}
		var control model.Control
		hasControl, err := txGet(c, s.ControlKey(), &control)
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
			if err := txPut(c, Decisions.Key(decision.ID), decision); err != nil {
				return err
			}
		}
		for _, update := range requests {
			if err := txUpdateRequest(c, update); err != nil {
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
						old.RediscoveryResult = new(fmt.Sprintf("%s: %s", proposal.Decision, proposal.Reason))
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
			return txPut(c, s.ControlKey(), control)
		}
		return nil
	})
}

func (s *Store) AppendCycleSession(cycleID string, session model.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cycle model.Cycle
	found, err := txGet(s.conn, Cycles.Key(cycleID), &cycle)
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
	return txPut(s.conn, Cycles.Key(cycleID), cycle)
}

func (s *Store) Event(entity string, kind model.EventKind, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return txRepositoryEvent(s.conn, s.repo, entity, kind, message)
}

func txRepositoryEvent(c *sql.Conn, repository, entity string, kind model.EventKind, message string) error {
	_, err := c.ExecContext(background, "INSERT INTO events(at,entity_id,kind,message,repository) VALUES (?1,?2,?3,?4,?5)", model.Now(), entity, kind.String(), redact.Text(message), repository)
	return err
}

// txEvent also serves the migration runner on schemas predating event scopes.
// The current schema's insert trigger attributes these unscoped system events.
func txEvent(c *sql.Conn, entity string, kind model.EventKind, message string) error {
	_, err := c.ExecContext(background, "INSERT INTO events(at,entity_id,kind,message) VALUES (?1,?2,?3,?4)", model.Now(), entity, kind.String(), redact.Text(message))
	return err
}

func (s *Store) Events(entity *string) ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return repositoryEvents(s.conn, s.repo, entity)
}

func repositoryEvents(c *sql.Conn, repo string, entity *string) ([]model.Event, error) {
	query := "SELECT id,at,entity_id,kind,message FROM events WHERE (?1 IS NULL OR entity_id=?1)"
	args := []any{entity}
	if repo != "" {
		query += " AND repository=?2 COLLATE NOCASE"
		args = append(args, repo)
	}
	return queryEvents(c, query+" ORDER BY id DESC LIMIT 200", args...)
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
		var kind string
		if err := rows.Scan(&e.ID, &e.At, &e.EntityID, &kind, &e.Message); err != nil {
			return nil, err
		}
		if err := e.Kind.UnmarshalText([]byte(kind)); err != nil {
			return nil, fmt.Errorf("Saved event %d has an unreadable kind: %w", e.ID, err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) PruneEvents(retain int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.ExecContext(background, `DELETE FROM events WHERE (?1='' OR repository=?1 COLLATE NOCASE)
        AND id NOT IN (SELECT id FROM events WHERE (?1='' OR repository=?1 COLLATE NOCASE) ORDER BY id DESC LIMIT ?2)`, s.repo, retain)
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
		cfg, err := storedConfig(c, s.ConfigKey())
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
		instance, err := storedInstanceSettings(c)
		if err != nil {
			return err
		}
		if instance.MaxSessionsPerDay > 0 {
			total, err := instanceSessionsOn(c, day)
			if err != nil {
				return err
			}
			if total < 0 || uint64(total) >= instance.MaxSessionsPerDay {
				return fmt.Errorf("Instance-wide daily session ceiling exhausted; increase the configured ceiling or wait until UTC midnight: %w", model.BlockedBudgetExhausted)
			}
		}
		if limit > uint64(1<<63-1) {
			limit = 1<<63 - 1
		}
		result, err := c.ExecContext(background, `INSERT INTO usage(repository,day,sessions) VALUES (?1,?2,1)
             ON CONFLICT(repository,day) DO UPDATE SET sessions=sessions+1 WHERE sessions < ?3`, s.usageRepo(cfg), day, int64(limit))
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return fmt.Errorf("Daily session budget exhausted; increase the configured limit or wait until UTC midnight: %w", model.BlockedBudgetExhausted)
		}
		if admission.Repository == "" {
			if s.repo != "" {
				admission.Repository = s.repo
			} else {
				admission.Repository = cfg.GitHubRepo
			}
		}
		data, err := wirejson.Marshal(admission)
		if err != nil {
			return err
		}
		_, err = c.ExecContext(background, "INSERT INTO admissions(id,at,day,data) VALUES (?1,?2,?3,?4)", admission.ID, admission.At, day, string(data))
		return err
	})
}

// DaySessions is one day's durable session counter.
type DaySessions struct {
	Day      string
	Sessions uint64
}

// usageRepo is the lowercase usage-counter identity for this handle: its own
// scope, or for the root handle the saved configuration's repository.
func (s *Store) usageRepo(cfg config.Config) string {
	if s.repo != "" {
		return strings.ToLower(s.repo)
	}
	return strings.ToLower(cfg.GitHubRepo)
}

// sessionsOn reads one repository's day counter; an empty repository reads the
// instance total across every repository.
func sessionsOn(c *sql.Conn, repository, day string) (int64, error) {
	if repository == "" {
		return instanceSessionsOn(c, day)
	}
	var sessions int64
	err := c.QueryRowContext(background, "SELECT sessions FROM usage WHERE repository=?1 AND day=?2", strings.ToLower(repository), day).Scan(&sessions)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return sessions, err
}

func instanceSessionsOn(c *sql.Conn, day string) (int64, error) {
	var sessions sql.NullInt64
	if err := c.QueryRowContext(background, "SELECT SUM(sessions) FROM usage WHERE day=?1", day).Scan(&sessions); err != nil {
		return 0, err
	}
	return sessions.Int64, nil
}

func (s *Store) PlanningCapacity(at time.Time) (model.PlanningCapacity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.planningCapacityAt(s.conn, at)
}

func (s *Store) planningCapacityAt(c *sql.Conn, at time.Time) (model.PlanningCapacity, error) {
	at = at.UTC()
	day := model.UTCDay(at)
	cfg, err := storedConfig(c, s.ConfigKey())
	if err != nil {
		return model.PlanningCapacity{}, err
	}
	repo := s.usageRepo(cfg)
	used, err := sessionsOn(c, repo, day)
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
	// The instance ceiling bounds every repository's remaining room.
	instance, err := storedInstanceSettings(c)
	if err != nil {
		return model.PlanningCapacity{}, err
	}
	if instance.MaxSessionsPerDay > 0 {
		total, err := instanceSessionsOn(c, day)
		if err != nil {
			return model.PlanningCapacity{}, err
		}
		instanceRemaining := uint64(0)
		if total >= 0 && instance.MaxSessionsPerDay > uint64(total) {
			instanceRemaining = instance.MaxSessionsPerDay - uint64(total)
		}
		if instanceRemaining < remaining {
			remaining = instanceRemaining
		}
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

// CancelTask settles the task after the separately durable marker commits.
func (s *Store) CancelTask(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	err := s.transaction(true, func(c *sql.Conn) error {
		var current model.Task
		found, err := txGet(c, Tasks.Key(id), &current)
		if err != nil {
			return err
		}
		if !found || current.OutputCommit != nil || current.Status == model.StatusPublishing || current.Status == model.StatusPublished {
			return nil
		}
		next := current.Clone()
		next.Status, next.UpdatedAt = model.StatusCancelled, model.Now()
		next.Error, next.BlockedReason = new("Cancelled by the operator"), nil
		if _, err := txCommitTask(c, current, next, model.TaskCancel); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed && err == nil, err
}

func storedConfig(c *sql.Conn, key Key[config.Config]) (config.Config, error) {
	cfg := config.Default()
	if _, err := txGet(c, key, &cfg); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func updateLineageTask(c *sql.Conn, id string, check func(*model.Task) error) error {
	var task model.Task
	found, err := txGet(c, Tasks.Key(id), &task)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Missing lineage task %s", id)
	}
	if task.Lifecycle.ArchivedAt != nil {
		return fmt.Errorf("Rediscovery request %s was archived before the plan committed", id)
	}
	expected := task.Clone()
	if err := check(&task); err != nil {
		return err
	}
	_, err = txCommitTask(c, expected, task, model.TaskLineage)
	return err
}

func StorageLimitError(measuredBytes uint64) error {
	return fmt.Errorf("Workspace storage limit reached (%d bytes). Resolve retained tasks or increase the limit: %w", measuredBytes, model.BlockedStorageLimit)
}
