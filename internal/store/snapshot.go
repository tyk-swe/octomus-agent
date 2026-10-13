package store

import (
	"database/sql"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// Snapshot reads canonical records through domain queries in one transaction.
// It is valid only during the callback passed to Store.Snapshot or ReadOnly.Snapshot.
// Storage layout, ordering and index selection stay owned by the store.
type Snapshot struct {
	conn *sql.Conn
}

func (s *Store) Snapshot(read func(*Snapshot) error) error {
	return s.snapshot(func(c *sql.Conn) error { return read(&Snapshot{conn: c}) })
}

func (r *ReadOnly) Snapshot(read func(*Snapshot) error) error {
	return r.snapshot(func(c *sql.Conn) error { return read(&Snapshot{conn: c}) })
}

func (s *Snapshot) Cycle(id string) (*model.Cycle, error) {
	return recordAt(s.conn, Cycles.Key(id))
}

const snapshotCycleTasksQuery = "SELECT r.data FROM record_meta m INDEXED BY meta_cycle JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.cycle_id=?1 ORDER BY r.id"

// TasksForCycle returns every task linked to a cycle, in stable identity order.
// Selection uses the cycle index before decoding the full evidence records.
func (s *Snapshot) TasksForCycle(id string) ([]model.Task, error) {
	return queryRecords[model.Task](s.conn, snapshotCycleTasksQuery, id)
}

const snapshotPRObservationsQuery = `SELECT r.data
    FROM record_meta m INDEXED BY meta_pr_identity
    JOIN records r ON r.kind=m.kind AND r.id=m.id
    WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE
        AND json_extract(m.summary,'$.pr.number')=?2
    ORDER BY m.seq ASC LIMIT 2`

// PRObservations returns at most two records for an exact remote identity. Two
// records expose ambiguous historical evidence rather than silently choosing one.
func (s *Snapshot) PRObservations(repository string, number uint64) ([]model.PRObservation, error) {
	return queryRecords[model.PRObservation](s.conn, snapshotPRObservationsQuery, repository, number)
}

// Cycles and Tasks read the complete histories for the usage export. Operational
// views use their bounded projections instead of these full-record queries.
func (s *Snapshot) Cycles() ([]model.Cycle, error) {
	return queryRecords[model.Cycle](s.conn, "SELECT data FROM records WHERE kind='cycle' ORDER BY id")
}

func (s *Snapshot) Tasks() ([]model.Task, error) {
	return queryRecords[model.Task](s.conn, "SELECT data FROM records WHERE kind='task' ORDER BY id")
}

func (s *Snapshot) Admissions() ([]Admission, error) {
	return queryRecords[Admission](s.conn, "SELECT data FROM admissions ORDER BY at,id")
}

func (s *Snapshot) DailySessions() ([]DaySessions, error) {
	rows, err := s.conn.QueryContext(background, "SELECT day,sessions FROM usage ORDER BY day")
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(day *DaySessions) []any { return []any{&day.Day, &day.Sessions} })
}
