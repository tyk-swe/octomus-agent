package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func statusList(statuses []model.Status) string {
	quoted := make([]string, 0, len(statuses))
	for _, s := range statuses {
		quoted = append(quoted, "'"+s.String()+"'")
	}
	return strings.Join(quoted, ",")
}

// PageLimit bounds each oldest-first task scan below; a full page means more tasks may remain.
const PageLimit = 500

func (s *Store) SchedulingTasks(runID *string) ([]model.Task, error) {
	return listRecords[model.Task](s, schedulingTasksSQL(), runID, PageLimit, s.repo)
}

func schedulingTasksSQL() string {
	return fmt.Sprintf(`WITH candidates AS (
                SELECT id,seq FROM record_meta WHERE kind='task' AND archived IS NULL AND (?3='' OR repository=?3 COLLATE NOCASE)
                    AND status IN (%s)
                UNION ALL
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM `+fromMeta+`
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued' AND (?3='' OR m.repository=?3 COLLATE NOCASE)
                            AND (?1 IS NULL OR m.run_id=?1)
                            AND (json_extract(r.data,'$.proposal.target') != json_extract(r.data,'$.config.default_branch')
                                OR EXISTS(SELECT 1 FROM pr_reservations p WHERE p.task_id=m.id))
                        ORDER BY m.seq ASC LIMIT ?2
                )
                UNION ALL
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM `+fromMeta+`
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued' AND (?3='' OR m.repository=?3 COLLATE NOCASE)
                            AND (?1 IS NULL OR m.run_id=?1)
                            AND json_extract(r.data,'$.proposal.target') = json_extract(r.data,'$.config.default_branch')
                            AND NOT EXISTS(SELECT 1 FROM pr_reservations p WHERE p.task_id=m.id)
                        ORDER BY m.seq ASC LIMIT ?2
                )
            )
            SELECT r.data FROM candidates CROSS JOIN records r ON r.kind='task' AND r.id=candidates.id
            ORDER BY candidates.seq ASC`, statusList(model.ActiveStatuses()))
}

// ActiveTasks returns the unarchived tasks whose status is still active, oldest first.
func (s *Store) ActiveTasks() ([]model.Task, error) {
	return listRecords[model.Task](s, "SELECT r.data FROM "+fromMeta+" WHERE m.kind='task' AND m.status IN ("+statusList(model.ActiveStatuses())+") AND m.archived IS NULL AND (?2='' OR m.repository=?2 COLLATE NOCASE) ORDER BY m.seq ASC LIMIT ?1", PageLimit, s.repo)
}

// CancelledWithLiveSessions returns unfinished cancellation evidence
// after its worker exits. Exclude ownership claims before decoding records, and
// filter completed evidence before the page limit so old tasks cannot hide it.
func (s *Store) CancelledWithLiveSessions(excludedIDs []string) ([]model.Task, error) {
	ids, err := json.Marshal(excludedIDs)
	if err != nil {
		return nil, err
	}
	return listRecords[model.Task](s, `SELECT r.data FROM `+fromMeta+`
        WHERE m.kind='task' AND m.status='cancelled' AND (?3='' OR m.repository=?3 COLLATE NOCASE)
            AND NOT EXISTS (SELECT 1 FROM json_each(?1) WHERE value=m.id)
            AND EXISTS (SELECT 1 FROM json_each(r.data,'$.sessions') WHERE json_extract(value,'$.status')='running')
        ORDER BY m.seq ASC LIMIT ?2`, string(ids), PageLimit, s.repo)
}

// ActiveTasksExcept excludes live workers and cleanup claims before decoding
// their evidence. An exited worker may leave any active state behind when its
// terminal checkpoint fails, not only publication.
func (s *Store) ActiveTasksExcept(excludedIDs []string) ([]model.Task, error) {
	ids, err := json.Marshal(excludedIDs)
	if err != nil {
		return nil, err
	}
	return listRecords[model.Task](s, `SELECT r.data FROM `+fromMeta+`
        WHERE m.kind='task' AND m.status IN (`+statusList(model.ActiveStatuses())+`) AND m.archived IS NULL AND m.discarded IS NULL AND (?3='' OR m.repository=?3 COLLATE NOCASE)
            AND NOT EXISTS (SELECT 1 FROM json_each(?1) WHERE value=m.id)
        ORDER BY m.seq ASC LIMIT ?2`, string(ids), PageLimit, s.repo)
}

// RunningCycles returns the running cycles other than except, the live worker's own, so its evidence is excluded before
// reading and decoding.
func (s *Store) RunningCycles(except string) ([]model.Cycle, error) {
	return listRecords[model.Cycle](s, "SELECT r.data FROM "+fromMeta+" WHERE m.kind='cycle' AND m.status='running' AND m.id!=?1 AND (?2='' OR m.repository=?2 COLLATE NOCASE)", except, s.repo)
}

func (s *Store) RunningBaselines() ([]model.BaselineCheck, error) {
	return listRecords[model.BaselineCheck](s, "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')='running' AND (?1='' OR json_extract(data,'$.config.github_repo')=?1 COLLATE NOCASE)", s.repo)
}

func (s *Store) StaleBaselines(after string) ([]model.BaselineCheck, error) {
	return listRecords[model.BaselineCheck](s, "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')!='running' AND json_extract(data,'$.workspace_removed')=0 AND (?2='' OR json_extract(data,'$.config.github_repo')=?2 COLLATE NOCASE) ORDER BY rowid<=COALESCE((SELECT rowid FROM records WHERE kind='baseline' AND id=?1),0),rowid LIMIT 100", after, s.repo)
}

func (s *Store) LatestBaseline() (*model.BaselineCheck, error) {
	id, err := Get(s, s.latestBaselineKey())
	if err != nil || id == nil {
		return nil, err
	}
	return Get(s, Baselines.Key(*id))
}

func (s *Store) TasksForCycle(id string) ([]model.Task, error) {
	return listRecords[model.Task](s, "SELECT r.data FROM "+fromMeta+" WHERE m.kind='task' AND m.cycle_id=?1 ORDER BY m.seq", id)
}

func (s *Store) snapshot(fn func(c *sql.Conn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(false, fn)
}

func (s *Store) DuplicateTasks(repository string, proposals []model.Proposal) ([]model.Task, error) {
	type identity struct {
		title    string
		identity string
	}
	targets := map[string][]identity{}
	order := []string{}
	for _, p := range proposals {
		if p.Decision != model.DecisionAccepted {
			continue
		}
		if _, seen := targets[p.Target]; !seen {
			order = append(order, p.Target)
		}
		targets[p.Target] = append(targets[p.Target], identity{strings.TrimSpace(p.Title), p.ProblemIdentity()})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	matched := map[string]struct{}{}
	var ids []string
	for _, target := range order {
		identities := targets[target]
		rows, err := s.conn.QueryContext(background, "SELECT id,json_extract(data,'$.proposal.title'),COALESCE(json_extract(data,'$.proposal.problem_key'),'') FROM records INDEXED BY task_problem_identity WHERE kind='task' AND json_extract(data,'$.config.github_repo')=?1 COLLATE NOCASE AND json_extract(data,'$.proposal.target')=?2 AND json_extract(data,'$.status')!='cancelled' AND json_extract(data,'$.lifecycle.archived_at') IS NULL", repository, target)
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer rows.Close()
			for rows.Next() {
				var id string
				var title, key sql.NullString
				if err := rows.Scan(&id, &title, &key); err != nil {
					return err
				}
				if !title.Valid {
					return fmt.Errorf("Task %s has no saved proposal title", id)
				}
				saved := model.ProblemIdentity(title.String, key.String)
				for _, proposed := range identities {
					if config.EqualASCII(strings.TrimSpace(title.String), proposed.title) || saved == proposed.identity {
						if _, dup := matched[id]; !dup {
							matched[id] = struct{}{}
							ids = append(ids, id)
						}
						break
					}
				}
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	tasks := make([]model.Task, 0, len(ids))
	for _, id := range ids {
		var task model.Task
		found, err := txGet(s.conn, Tasks.Key(id), &task)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("Task %s vanished during duplicate lookup", id)
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (s *Store) HasUnresolvedTasks() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists bool
	query := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM record_counts WHERE kind='task' AND status NOT IN (%s) AND archived=0 AND count>0)", statusList(model.TerminalStatuses()))
	var args []any
	if s.repo != "" {
		query = fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM record_meta WHERE kind='task' AND repository=?1 COLLATE NOCASE AND status NOT IN (%s) AND archived IS NULL)", statusList(model.TerminalStatuses()))
		args = []any{s.repo}
	}
	err := s.conn.QueryRowContext(background, query, args...).Scan(&exists)
	return exists, err
}

func txStartBatch(c *sql.Conn, key Key[model.Control], repository string, control model.Control) (model.Control, error) {
	id := model.ID()
	next := control.Clone()
	next.SetMode(model.OperatingModeRunOnce)
	next.Batch = &model.RunBatch{ID: id, Phase: model.BatchPhaseDraining, CycleID: nil}
	next.Error = nil
	next.NextCycleAt = 0
	if _, err := c.ExecContext(background, "UPDATE records SET data=json_set(data,'$.run_id',?1) WHERE kind='task' AND id IN (SELECT id FROM record_meta WHERE kind='task' AND status='queued' AND archived IS NULL AND (?2='' OR repository=?2 COLLATE NOCASE))", id, repository); err != nil {
		return model.Control{}, err
	}
	if err := txPut(c, key, next); err != nil {
		return model.Control{}, err
	}
	return next, nil
}

func liveControl(c *sql.Conn, key Key[model.Control]) (model.Control, error) {
	var live model.Control
	found, err := txGet(c, key, &live)
	if err == nil && !found {
		live = model.DefaultControl()
	}
	return live, err
}

func (s *Store) StartBatch(control *model.Control, at time.Time) (model.PlanningCapacity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var capacity model.PlanningCapacity
	var next model.Control
	started, err := s.conditional(func(c *sql.Conn) error {
		live, err := liveControl(c, s.ControlKey())
		if err != nil {
			return err
		}
		capacity, err = s.planningCapacityAt(c, at)
		if err != nil {
			return err
		}
		if !wirejson.Equal(live, *control) || !capacity.Available() {
			return errRollback
		}
		next, err = txStartBatch(c, s.ControlKey(), s.repo, *control)
		return err
	})
	if started {
		*control = next
	}
	return capacity, started, err
}

func (s *Store) BeginCycle(cycle model.Cycle, control model.Control, expected model.Control, fingerprint string, at time.Time) (model.PlanningCapacity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var capacity model.PlanningCapacity
	started, err := s.conditional(func(c *sql.Conn) error {
		cfg, err := storedConfig(c, s.ConfigKey())
		if err != nil {
			return err
		}
		liveFingerprint, err := cfg.Fingerprint()
		if err != nil {
			return err
		}
		live, err := liveControl(c, s.ControlKey())
		if err != nil {
			return err
		}
		capacity, err = s.planningCapacityAt(c, at)
		if err != nil {
			return err
		}
		if liveFingerprint != fingerprint || !wirejson.Equal(live, expected) || !capacity.Available() {
			return errRollback
		}
		if err := txPut(c, Cycles.Key(cycle.ID), cycle); err != nil {
			return err
		}
		// Commit the full planning allowance with the cycle, before any
		// grounding or model work can race another repository's admission.
		if _, err := c.ExecContext(background, "INSERT INTO planning_reservations(cycle_id,sessions) VALUES (?1,?2)", cycle.ID, capacity.Required); err != nil {
			return err
		}
		control.BudgetWaiting = false
		return txPut(c, s.ControlKey(), control)
	})
	return capacity, started, err
}

func (s *Store) BatchCounts(id string) (uint64, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := "'queued'," + statusList(model.ActiveStatuses())
	var p, u int64
	err := s.conn.QueryRowContext(background, fmt.Sprintf("SELECT COALESCE(sum(status IN (%s)),0), COALESCE(sum(status IN (%s)),0) FROM batch_members WHERE run_id=?1", pending, statusList(model.UnresolvedStatuses())), id).Scan(&p, &u)
	return uint64(p), uint64(u), err
}

const cleanupEligible = `kind=?1 AND discarded IS NULL AND (?4='' OR repository=?4 COLLATE NOCASE)
    AND (archived IS NOT NULL OR (?1='task' AND status='published') OR (?1='cycle' AND status IN ('completed','idle')))
    AND julianday(COALESCE(archived,json_extract(summary,'$.completed_at'),json_extract(summary,'$.updated_at'),json_extract(summary,'$.started_at')))<julianday(?2)`

// Walk each side of the cursor in index order, then sort only the bounded page.
// Materialize following so the wrap skips its scan when the page is full. A single
// statement keeps the cursor, both ranges and eligibility in one read snapshot.
const cleanupCandidatesSQL = `WITH cursor AS (
    SELECT COALESCE((SELECT seq FROM record_meta WHERE kind=?1 AND id=?3),0) AS seq
), following AS MATERIALIZED (
    SELECT id,seq FROM record_meta WHERE ` + cleanupEligible + `
    AND seq>(SELECT seq FROM cursor) ORDER BY seq LIMIT 100
), wrapped AS (
    SELECT id,seq FROM record_meta WHERE ` + cleanupEligible + `
    AND seq<=(SELECT seq FROM cursor) ORDER BY seq
    LIMIT (SELECT 100-count(*) FROM following)
)
SELECT id FROM (
    SELECT id,seq,0 AS phase FROM following
    UNION ALL
    SELECT id,seq,1 AS phase FROM wrapped
) ORDER BY phase,seq LIMIT 100`

func (s *Store) CleanupCandidates(kind, cutoff, after string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return queryStrings(s.conn, cleanupCandidatesSQL, kind, cutoff, after, s.repo)
}

// CleanupEligible rechecks a candidate after the retention pass released the operator gate.
// Archiving another candidate during a slow removal restarts that candidate's retention period.
func (s *Store) CleanupEligible(kind, id, cutoff string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var eligible bool
	err := s.conn.QueryRowContext(background, "SELECT EXISTS(SELECT 1 FROM record_meta WHERE id=?3 AND "+cleanupEligible+")", kind, cutoff, id, s.repo).Scan(&eligible)
	return eligible, err
}

func (s *Store) RecordPRObservation(repository string, p model.PullRequest, deliveredNow bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previousID, previous, err := prObservationAt(s.conn, repository, p.Number)
	if err != nil {
		return err
	}
	recordID := previousID
	if previous == nil {
		recordID = fmt.Sprintf("%s:%d", strings.ToLower(repository), p.Number)
	}
	var delivered *string
	switch {
	case deliveredNow:
		head := p.Head
		delivered = &head
	case previous != nil && previous.DeliveredHead != nil:
		delivered = previous.DeliveredHead
	default:
		if delivered, err = latestPROutputAt(s.conn, repository, p.Number); err != nil {
			return err
		}
	}
	observation := model.PRObservation{
		Repository:           repository,
		ObservedAt:           model.Now(),
		ExternalHeadMovement: delivered != nil && *delivered != p.Head,
		DeliveredHead:        delivered,
		PR:                   p,
		AutoMerge:            preserveAutoMerge(previous, p),
	}
	return txPut(s.conn, PRs.Key(recordID), observation)
}

// decisionRecords excludes alternatives absorbed by an accepted proposal in the same cycle,
// including legacy records written before recordDecisions omitted those alternatives.
const decisionRecords = ` WHERE d.kind='decision'
	AND json_extract(d.data,'$.repository')=?1 COLLATE NOCASE
	AND COALESCE(json_extract(d.data,'$.problem_key'),'')!=''
	AND (json_extract(d.data,'$.decision')='accepted' OR NOT EXISTS (
		SELECT 1 FROM records a WHERE a.kind='decision'
		AND json_extract(a.data,'$.repository')=json_extract(d.data,'$.repository') COLLATE NOCASE
		AND json_extract(a.data,'$.target')=json_extract(d.data,'$.target')
		AND json_extract(a.data,'$.problem_key')=json_extract(d.data,'$.problem_key')
		AND json_extract(a.data,'$.cycle_id')=json_extract(d.data,'$.cycle_id')
		AND json_extract(a.data,'$.decision')='accepted'))`

// DecisionMemory bounds prompt context after selecting targets planning can actually use.
func (s *Store) DecisionMemory(repository string, targets []string) ([]model.DecisionRecord, error) {
	encoded, err := wirejson.Marshal(targets)
	if err != nil {
		return nil, err
	}
	return listRecords[model.DecisionRecord](s, "SELECT d.data FROM records d"+decisionRecords+
		" AND json_extract(d.data,'$.target') IN (SELECT value FROM json_each(?2)) ORDER BY d.rowid DESC LIMIT 100", repository, string(encoded))
}

// DecisionsForProblem is the authoritative history for admission; unrelated decisions
// and prompt limits must not evict a still-current decision for this exact identity.
func (s *Store) DecisionsForProblem(repository, target, problem string) ([]model.DecisionRecord, error) {
	return listRecords[model.DecisionRecord](s, "SELECT d.data FROM records d INDEXED BY decision_identity"+decisionRecords+
		" AND json_extract(d.data,'$.target')=?2 AND json_extract(d.data,'$.problem_key')=?3 ORDER BY d.rowid DESC", repository, target, problem)
}

func (s *Store) RediscoveryRequests(repository string) ([]any, error) {
	return listRecords[any](s, "SELECT json_object('id',r.id,'title',json_extract(r.data,'$.proposal.title'),'target',json_extract(r.data,'$.proposal.target'),'problem',json_extract(r.data,'$.proposal.problem'),'scope',json_extract(r.data,'$.proposal.scope')) FROM "+fromMeta+" WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.status='cancelled' AND m.archived IS NULL AND json_extract(r.data,'$.rediscovery_requested')=1 AND json_array_length(r.data,'$.superseded_by')=0 ORDER BY m.seq DESC LIMIT 100", repository)
}

func latestPROutputAt(c *sql.Conn, repository string, number uint64) (*string, error) {
	var output *string
	err := c.QueryRowContext(background, "SELECT json_extract(r.data,'$.output_commit') FROM "+fromMeta+" WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.status='published' AND json_extract(m.summary,'$.pr_number')=?2 ORDER BY json_extract(m.summary,'$.updated_at') DESC LIMIT 1", repository, int64(number)).Scan(&output)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return output, err
}

func prObservationAt(c *sql.Conn, repository string, number uint64) (string, *model.PRObservation, error) {
	var id, data string
	err := c.QueryRowContext(background, "SELECT m.id,r.data FROM "+fromMeta+" WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE AND json_extract(m.summary,'$.pr.number')=?2 ORDER BY m.seq DESC LIMIT 1", repository, int64(number)).Scan(&id, &data)
	if err == sql.ErrNoRows {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var observation model.PRObservation
	if err := decodeJSON([]byte(data), &observation); err != nil {
		return "", nil, fmt.Errorf("Saved pr %s is unreadable: %w", id, err)
	}
	return id, &observation, nil
}
