package store

// Indexed operational views. Canonical evidence remains in records.data.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// statusList quotes statuses as a SQL IN-list literal derived from the model vocabulary.
func statusList(statuses []model.Status) string {
	quoted := make([]string, 0, len(statuses))
	for _, s := range statuses {
		quoted = append(quoted, "'"+s.String()+"'")
	}
	return strings.Join(quoted, ",")
}

// HistoryQuery is the dashboard's paged history filter.
type HistoryQuery struct {
	Before *int64
	Limit  *int
	Status *string
	Q      *string
	Cycle  *string
}

// Page is one page of projected summaries as raw JSON documents.
type Page struct {
	Items      []json.RawMessage `json:"items"`
	Counts     map[string]int64  `json:"counts"`
	NextCursor *int64            `json:"next_cursor"`
}

func (q HistoryQuery) limit() int {
	limit := 50
	if q.Limit != nil {
		limit = *q.Limit
	}
	if limit < 1 {
		return 1
	}
	if limit > 100 {
		return 100
	}
	return limit
}
func (q HistoryQuery) before() int64 {
	if q.Before != nil {
		return *q.Before
	}
	return math.MaxInt64
}
func filterAll(value *string) string {
	if value == nil || *value == "all" {
		return ""
	}
	return *value
}
func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type pageRow struct {
	summary string
	seq     int64
}

func page(c *sql.Conn, kind string, query HistoryQuery) (Page, error) {
	limit := query.limit()
	sqlText := "SELECT summary,seq FROM record_meta WHERE kind=?1 AND seq<?2"
	status := filterAll(query.Status)
	if status == "attention" {
		sqlText = strings.Replace(sqlText, "FROM record_meta", "FROM record_meta INDEXED BY meta_attention", 1)
	} else if status != "" {
		sqlText = strings.Replace(sqlText, "FROM record_meta", "FROM record_meta INDEXED BY meta_status", 1)
	}
	switch {
	case status == "active":
		sqlText += fmt.Sprintf(" AND status IN (%s)", statusList(model.ActiveStatuses()))
	case status == "attention":
		sqlText += fmt.Sprintf(" AND status IN (%s) AND archived IS NULL", statusList(model.AttentionStatuses()))
	case status != "":
		sqlText += " AND status=?3"
	default:
		sqlText += " AND ?3=?3"
	}
	// Bind ?3 even for named aggregate filters.
	if status == "active" || status == "attention" {
		sqlText += " AND ?3=?3"
	}
	sqlText += " AND (?4='' OR instr(lower(title || ' ' || target || ' ' || summary),lower(?4))>0) ORDER BY seq DESC LIMIT ?5"
	rows, err := c.QueryContext(background, sqlText, kind, query.before(), status, orEmpty(query.Q), int64(limit+1))
	if err != nil {
		return Page{}, err
	}
	collected, err := collectPageRows(rows)
	if err != nil {
		return Page{}, err
	}
	return decodePage(collected, limit), nil
}

func collectPageRows(rows *sql.Rows) ([]pageRow, error) {
	defer rows.Close()
	var out []pageRow
	for rows.Next() {
		var r pageRow
		if err := rows.Scan(&r.summary, &r.seq); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func decodePage(rows []pageRow, limit int) Page {
	var next *int64
	if len(rows) > limit {
		rows = rows[:limit]
		seq := rows[len(rows)-1].seq
		next = &seq
	}
	items := make([]json.RawMessage, 0, len(rows))
	for _, r := range rows {
		items = append(items, json.RawMessage(r.summary))
	}
	return Page{Items: items, Counts: map[string]int64{}, NextCursor: next}
}

// HistoryPage pages projected summaries of one record kind, newest first.
func (s *Store) HistoryPage(kind string, query HistoryQuery) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return page(s.conn, kind, query)
}

// ProposalPage pages projected proposals with private prompt/evidence stripped.
func (s *Store) ProposalPage(q HistoryQuery) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := q.limit()
	rows, err := s.conn.QueryContext(background,
		"SELECT json_set(json_remove(data,'$.prompt','$.evidence'),'$.content_revision',content_revision,'$.cycle',number,'$.cycle_id',cycle_id,'$.mode',mode,'$.prompt','','$.evidence',json('[]'),'$.problem',substr(json_extract(data,'$.problem'),1,2000),'$.reason',substr(json_extract(data,'$.reason'),1,2000)),seq FROM proposal_records WHERE seq<?1 AND (?2='' OR decision=?2) AND (?3='' OR cycle_id=?3) AND (?4='' OR instr(lower(title || ' ' || json_extract(data,'$.problem')),lower(?4))>0) ORDER BY seq DESC LIMIT ?5",
		q.before(), filterAll(q.Status), filterAll(q.Cycle), orEmpty(q.Q), int64(limit+1))
	if err != nil {
		return Page{}, err
	}
	collected, err := collectPageRows(rows)
	if err != nil {
		return Page{}, err
	}
	result := decodePage(collected, limit)
	counts, err := s.conn.QueryContext(background, "SELECT decision,count(*) FROM proposal_records WHERE (?1='' OR cycle_id=?1) GROUP BY decision", filterAll(q.Cycle))
	if err != nil {
		return Page{}, err
	}
	defer counts.Close()
	for counts.Next() {
		var decision string
		var count int64
		if err := counts.Scan(&decision, &count); err != nil {
			return Page{}, err
		}
		result.Counts[decision] = count
	}
	return result, counts.Err()
}

// ProposalDetail returns one projected proposal with its content revision.
func (s *Store) ProposalDetail(cycle, id string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var data string
	err := s.conn.QueryRowContext(background, "SELECT json_set(data,'$.content_revision',content_revision) FROM proposal_records WHERE cycle_id=?1 AND proposal_id=?2", cycle, id).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

// SchedulingTasks lists every active task plus a bounded queued window for the run.
func (s *Store) SchedulingTasks(runID *string) ([]model.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Every active writer must be visible, regardless of the queued history size or batch.
	raw, err := queryStrings(s.conn, fmt.Sprintf(`WITH candidates AS (
                SELECT id,seq FROM record_meta WHERE kind='task' AND archived IS NULL
                    AND status IN (%s)
                UNION
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM record_meta m JOIN records r ON r.kind='task' AND r.id=m.id
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued'
                            AND (?1 IS NULL OR m.run_id=?1)
                            AND (json_extract(r.data,'$.proposal.target') != json_extract(r.data,'$.config.default_branch')
                                OR EXISTS(SELECT 1 FROM pr_reservations p WHERE p.task_id=m.id))
                        ORDER BY m.seq ASC LIMIT 500
                )
                UNION
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM record_meta m JOIN records r ON r.kind='task' AND r.id=m.id
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued'
                            AND (?1 IS NULL OR m.run_id=?1)
                            AND json_extract(r.data,'$.proposal.target') = json_extract(r.data,'$.config.default_branch')
                            AND NOT EXISTS(SELECT 1 FROM pr_reservations p WHERE p.task_id=m.id)
                        ORDER BY m.seq ASC LIMIT 500
                )
            )
            SELECT r.data FROM candidates m JOIN records r ON r.kind='task' AND r.id=m.id
            ORDER BY m.seq ASC`, statusList(model.ActiveStatuses())), runID)
	if err != nil {
		return nil, err
	}
	return decodeAll[model.Task](raw)
}

// TasksWithStatus lists up to 500 unarchived tasks in the given statuses, oldest first.
func (s *Store) TasksWithStatus(statuses []string) ([]model.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := json.Marshal(statuses)
	if err != nil {
		return nil, err
	}
	raw, err := queryStrings(s.conn, "SELECT r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.status IN (SELECT value FROM json_each(?1)) AND m.archived IS NULL ORDER BY m.seq ASC LIMIT 500", string(list))
	if err != nil {
		return nil, err
	}
	return decodeAll[model.Task](raw)
}

func (s *Store) RunningCycles() ([]model.Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, "SELECT r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='cycle' AND m.status='running'")
	if err != nil {
		return nil, err
	}
	return decodeAll[model.Cycle](raw)
}

func (s *Store) RunningBaselines() ([]model.BaselineCheck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')='running'")
	if err != nil {
		return nil, err
	}
	return decodeAll[model.BaselineCheck](raw)
}

func (s *Store) BaselineCleanupCandidates() ([]model.BaselineCheck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')!='running' AND json_extract(data,'$.workspace_removed')=0 ORDER BY rowid LIMIT 100")
	if err != nil {
		return nil, err
	}
	return decodeAll[model.BaselineCheck](raw)
}

func (s *Store) LatestBaseline() (*model.BaselineCheck, error) {
	var id string
	found, err := s.Get("settings", "baseline_latest", &id)
	if err != nil || !found {
		return nil, err
	}
	return Get[model.BaselineCheck](s, "baseline", id)
}

func (s *Store) TasksForCycle(id string) ([]model.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, "SELECT r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.cycle_id=?1 ORDER BY m.seq", id)
	if err != nil {
		return nil, err
	}
	return decodeAll[model.Task](raw)
}

// Snapshot runs fn inside one deferred transaction on the service connection.
// Callers use it for reads that must observe a single consistent state.
func (s *Store) Snapshot(fn func(c *sql.Conn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(false, fn)
}

// DuplicateTasks finds saved tasks whose title or problem identity matches an
// accepted proposal for the same repository and target.
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
	found := map[string]struct{}{}
	var ids []string
	for _, target := range order {
		identities := targets[target]
		// Read the covering index once per target, regardless of proposal count
		// or evidence size. SQLite lower() cannot normalize Unicode identities.
		rows, err := s.conn.QueryContext(background, "SELECT id,json_extract(data,'$.proposal.title'),COALESCE(json_extract(data,'$.proposal.problem_key'),'') FROM records INDEXED BY task_problem_identity WHERE kind='task' AND json_extract(data,'$.config.github_repo')=?1 COLLATE NOCASE AND json_extract(data,'$.proposal.target')=?2 AND json_extract(data,'$.status')!='cancelled' AND json_extract(data,'$.lifecycle.archived_at') IS NULL", repository, target)
		if err != nil {
			return nil, err
		}
		// rows.Err, not a later rows.Close, reports a step error: Next closes
		// the rows when it fails, and Close then returns nil.
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
					if equalASCIIFold(strings.TrimSpace(title.String), proposed.title) || saved == proposed.identity {
						if _, dup := found[id]; !dup {
							found[id] = struct{}{}
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
		found, err := txGet(s.conn, "task", id, &task)
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

// equalASCIIFold folds only ASCII letters.
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func (s *Store) HasUnresolvedTasks() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists bool
	err := s.conn.QueryRowContext(background, fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM record_counts WHERE kind='task' AND status NOT IN (%s) AND archived=0 AND count>0)", statusList(model.TerminalStatuses()))).Scan(&exists)
	return exists, err
}

// StartBatch opens a run-once batch over every queued task and saves the control.
func (s *Store) StartBatch(control *model.Control) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(false, func(c *sql.Conn) error {
		id := model.ID()
		control.SetMode(model.OperatingModeRunOnce)
		control.Batch = &model.RunBatch{ID: id, Phase: model.BatchPhaseDraining, CycleID: nil}
		control.Error = nil
		control.NextCycleAt = 0
		if _, err := c.ExecContext(background, "UPDATE records SET data=json_set(data,'$.run_id',?1) WHERE kind='task' AND id IN (SELECT id FROM record_meta WHERE kind='task' AND status='queued' AND archived IS NULL)", id); err != nil {
			return err
		}
		return txPut(c, "settings", "control", *control)
	})
}

// StartBatchIfAffordable starts a run-once batch only when the live
// configuration can still fund a complete planning pass. The affordability
// decision and every RunOnce side effect share one transaction.
func (s *Store) StartBatchIfAffordable(control *model.Control, at time.Time) (model.PlanningCapacity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var capacity model.PlanningCapacity
	var next model.Control
	started := false
	err := s.transaction(true, func(c *sql.Conn) error {
		var live model.Control
		found, err := txGet(c, "settings", "control", &live)
		if err != nil {
			return err
		}
		if !found {
			live = model.DefaultControl()
		}
		capacity, err = planningCapacityAt(c, at)
		if err != nil {
			return err
		}
		if !sameJSON(live, *control) || !capacity.Available() {
			return errRollback
		}
		id := model.ID()
		next = control.Clone()
		next.SetMode(model.OperatingModeRunOnce)
		next.Batch = &model.RunBatch{ID: id, Phase: model.BatchPhaseDraining, CycleID: nil}
		next.Error = nil
		next.NextCycleAt = 0
		if _, err := c.ExecContext(background, "UPDATE records SET data=json_set(data,'$.run_id',?1) WHERE kind='task' AND id IN (SELECT id FROM record_meta WHERE kind='task' AND status='queued' AND archived IS NULL)", id); err != nil {
			return err
		}
		if err := txPut(c, "settings", "control", next); err != nil {
			return err
		}
		started = true
		return nil
	})
	if err == errRollback {
		return capacity, false, nil
	}
	if err == nil && started {
		*control = next
	}
	return capacity, started, err
}

// BeginCycleIfAffordable atomically revalidates the exact live configuration,
// the expected control record, and planning affordability before exposing a
// running cycle or changing the RunOnce phase.
func (s *Store) BeginCycleIfAffordable(cycle model.Cycle, control model.Control, expected model.Control, fingerprint string, at time.Time) (model.PlanningCapacity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var capacity model.PlanningCapacity
	started := false
	err := s.transaction(true, func(c *sql.Conn) error {
		cfg, err := storedConfig(c)
		if err != nil {
			return err
		}
		liveFingerprint, err := cfg.Fingerprint()
		if err != nil {
			return err
		}
		var live model.Control
		found, err := txGet(c, "settings", "control", &live)
		if err != nil {
			return err
		}
		if !found {
			live = model.DefaultControl()
		}
		capacity, err = planningCapacityAt(c, at)
		if err != nil {
			return err
		}
		if liveFingerprint != fingerprint || !sameJSON(live, expected) || !capacity.Available() {
			return errRollback
		}
		if err := txPut(c, "cycle", cycle.ID, cycle); err != nil {
			return err
		}
		if err := txPut(c, "settings", "control", control); err != nil {
			return err
		}
		started = true
		return nil
	})
	if err == errRollback {
		return capacity, false, nil
	}
	return capacity, started, err
}

// BatchCounts returns (pending, unresolved) member counts for a run.
func (s *Store) BatchCounts(id string) (uint64, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Pending work is queued plus every active status; the second list holds the
	// unresolved-terminal statuses.
	pending := "'queued'," + statusList(model.ActiveStatuses())
	var p, u int64
	err := s.conn.QueryRowContext(background, fmt.Sprintf("SELECT COALESCE(sum(status IN (%s)),0), COALESCE(sum(status IN (%s)),0) FROM batch_members WHERE run_id=?1", pending, statusList(model.UnresolvedStatuses())), id).Scan(&p, &u)
	return uint64(p), uint64(u), err
}

// Dashboard is the one-transaction summary the dashboard polls.
type Dashboard struct {
	Tasks          []json.RawMessage `json:"tasks"`
	Cycles         []json.RawMessage `json:"cycles"`
	PRs            []json.RawMessage `json:"prs"`
	Events         []model.Event     `json:"events"`
	Counts         map[string]int64  `json:"counts"`
	AttentionTasks []json.RawMessage `json:"attention_tasks"`
	MergedPRs      int64             `json:"merged_prs"`
	SessionsToday  int64             `json:"sessions_today"`
}

func (s *Store) Dashboard() (Dashboard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Dashboard
	err := s.transaction(false, func(c *sql.Conn) error {
		result.Counts = map[string]int64{}
		if err := statusCounts(c, result.Counts); err != nil {
			return err
		}
		hundred := 100
		query := HistoryQuery{Limit: &hundred}
		tasks, err := page(c, "task", query)
		if err != nil {
			return err
		}
		for i := 0; i < 2; i++ {
			if tasks.NextCursor == nil {
				break
			}
			before := *tasks.NextCursor
			more, err := page(c, "task", HistoryQuery{Before: &before, Limit: &hundred})
			if err != nil {
				return err
			}
			tasks.Items = append(tasks.Items, more.Items...)
			tasks.NextCursor = more.NextCursor
		}
		// Keep old retried work visible even when it predates the recent history window.
		active, queued := "active", "queued"
		current, err := page(c, "task", HistoryQuery{Status: &active, Limit: &hundred})
		if err != nil {
			return err
		}
		queuedPage, err := page(c, "task", HistoryQuery{Status: &queued, Limit: &hundred})
		if err != nil {
			return err
		}
		items := append(current.Items, queuedPage.Items...)
		ids := map[string]struct{}{}
		for _, item := range items {
			if id := summaryID(item); id != "" {
				ids[id] = struct{}{}
			}
		}
		for _, item := range tasks.Items {
			if id := summaryID(item); id != "" {
				if _, seen := ids[id]; seen {
					continue
				}
			}
			items = append(items, item)
		}
		if len(items) > 300 {
			items = items[:300]
		}
		result.Tasks = items
		twenty := 20
		cycles, err := page(c, "cycle", HistoryQuery{Limit: &twenty})
		if err != nil {
			return err
		}
		result.Cycles = cycles.Items
		prs, err := page(c, "pr", query)
		if err != nil {
			return err
		}
		result.PRs = prs.Items
		if result.Events, err = queryEvents(c, "SELECT id,at,entity_id,kind,substr(message,1,512) FROM events ORDER BY id DESC LIMIT 200"); err != nil {
			return err
		}
		if result.SessionsToday, err = sessionsOn(c, model.Today()); err != nil {
			return err
		}
		if err := c.QueryRowContext(background, "SELECT COALESCE(sum(count),0) FROM record_counts WHERE kind='pr' AND status='merged'").Scan(&result.MergedPRs); err != nil {
			return err
		}
		attention, five := "attention", 5
		attentionPage, err := page(c, "task", HistoryQuery{Status: &attention, Limit: &five})
		if err != nil {
			return err
		}
		result.AttentionTasks = attentionPage.Items
		return nil
	})
	return result, err
}

// statusCounts adds the dashboard's task count per status to counts: archived
// tasks count only outside the attention statuses.
func statusCounts(c *sql.Conn, counts map[string]int64) error {
	rows, err := c.QueryContext(background, fmt.Sprintf("SELECT status,sum(count) FROM record_counts WHERE kind='task' AND (status NOT IN (%s) OR archived=0) GROUP BY status HAVING sum(count)>0", statusList(model.AttentionStatuses())))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		counts[status] = count
	}
	return rows.Err()
}

func summaryID(item json.RawMessage) string {
	var summary struct {
		ID *string `json:"id"`
	}
	if err := json.Unmarshal(item, &summary); err != nil || summary.ID == nil {
		return ""
	}
	return *summary.ID
}

// CleanupCandidates lists retained record ids older than the cutoff.
func (s *Store) CleanupCandidates(kind, cutoff string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, "SELECT id FROM record_meta WHERE kind=?1 AND discarded IS NULL AND (archived IS NOT NULL OR (?1='task' AND status='published') OR (?1='cycle' AND status IN ('completed','idle'))) AND julianday(COALESCE(archived,json_extract(summary,'$.updated_at'),json_extract(summary,'$.started_at')))<julianday(?2) ORDER BY seq LIMIT 100", kind, cutoff)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(raw))
	for _, id := range raw {
		ids = append(ids, string(id))
	}
	return ids, nil
}

func (s *Store) LatestPrOutput(repository string, number uint64) (*string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return latestPrOutputAt(s.conn, repository, number)
}

// PrObservation returns the saved record id and observation for a PR number.
func (s *Store) PrObservation(repository string, number uint64) (string, *model.PrObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return prObservationAt(s.conn, repository, number)
}

// RecordPrObservation records a PR observation atomically with the
// delivery-baseline merge. The previous observation and latest published output
// are read under the same store lock as the write, so a stale poll can never
// overwrite a newer `delivered_head` recorded by a concurrent publication.
func (s *Store) RecordPrObservation(repository string, p model.PullRequest, deliveredNow bool) error {
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
		if delivered, err = latestPrOutputAt(s.conn, repository, p.Number); err != nil {
			return err
		}
	}
	observation := model.PrObservation{
		Repository:           repository,
		ObservedAt:           model.Now(),
		ExternalHeadMovement: delivered != nil && *delivered != p.Head,
		DeliveredHead:        delivered,
		PR:                   p,
	}
	return txPut(s.conn, "pr", recordID, observation)
}

// DecisionMemory lists the newest 100 saved decisions for a repository.
func (s *Store) DecisionMemory(repository string) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, "SELECT data FROM records WHERE kind='decision' AND json_extract(data,'$.repository')=?1 COLLATE NOCASE ORDER BY rowid DESC LIMIT 100", repository)
	if err != nil {
		return nil, err
	}
	return decodeAll[any](raw)
}

// RediscoveryRequests lists cancelled tasks awaiting rediscovery for a repository.
func (s *Store) RediscoveryRequests(repository string) ([]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := queryStrings(s.conn, "SELECT json_object('id',r.id,'title',json_extract(r.data,'$.proposal.title'),'target',json_extract(r.data,'$.proposal.target'),'problem',json_extract(r.data,'$.proposal.problem'),'scope',json_extract(r.data,'$.proposal.scope')) FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.status='cancelled' AND json_extract(r.data,'$.rediscovery_requested')=1 AND json_array_length(r.data,'$.superseded_by')=0 ORDER BY m.seq DESC LIMIT 100", repository)
	if err != nil {
		return nil, err
	}
	return decodeAll[any](raw)
}

func latestPrOutputAt(c *sql.Conn, repository string, number uint64) (*string, error) {
	var output *string
	err := c.QueryRowContext(background, "SELECT json_extract(r.data,'$.output_commit') FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='task' AND m.repository=?1 COLLATE NOCASE AND m.status='published' AND json_extract(m.summary,'$.pr_number')=?2 ORDER BY json_extract(m.summary,'$.updated_at') DESC LIMIT 1", repository, int64(number)).Scan(&output)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return output, err
}

func prObservationAt(c *sql.Conn, repository string, number uint64) (string, *model.PrObservation, error) {
	var id, data string
	err := c.QueryRowContext(background, "SELECT m.id,r.data FROM record_meta m JOIN records r ON r.kind=m.kind AND r.id=m.id WHERE m.kind='pr' AND m.repository=?1 COLLATE NOCASE AND json_extract(m.summary,'$.pr.number')=?2 ORDER BY m.seq DESC LIMIT 1", repository, int64(number)).Scan(&id, &data)
	if err == sql.ErrNoRows {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var observation model.PrObservation
	if err := decodeJSON([]byte(data), &observation); err != nil {
		return "", nil, err
	}
	return id, &observation, nil
}

// MarshalJSON renders a page with compact canonical formatting.
func (p Page) MarshalJSON() ([]byte, error) {
	type plain Page
	return wirejson.Marshal(plain(p))
}
