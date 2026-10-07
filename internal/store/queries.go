package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func statusList(statuses []model.Status) string {
	quoted := make([]string, 0, len(statuses))
	for _, s := range statuses {
		quoted = append(quoted, "'"+s.String()+"'")
	}
	return strings.Join(quoted, ",")
}

type HistoryQuery struct {
	Before *int64
	Limit  *int
	Status *string
	Q      *string
	Cycle  *string
}

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
	id      string
}

func page(c *sql.Conn, kind string, query HistoryQuery) (Page, error) {
	limit := query.limit()
	rows, err := pageRows(c, kind, query, limit+1)
	if err != nil {
		return Page{}, err
	}
	return decodePage(rows, limit), nil
}

func pageRows(c *sql.Conn, kind string, query HistoryQuery, n int) ([]pageRow, error) {
	status := filterAll(query.Status)
	from, filter := "record_meta", " AND ?3=?3"
	switch status {
	case "":
	case "active":
		from += " INDEXED BY meta_status"
		filter = fmt.Sprintf(" AND status IN (%s)", statusList(model.ActiveStatuses())) + filter
	case "attention":
		from += " INDEXED BY meta_attention"
		filter = fmt.Sprintf(" AND status IN (%s) AND archived IS NULL", statusList(model.AttentionStatuses())) + filter
	default:
		from += " INDEXED BY meta_status"
		filter = " AND status=?3"
	}
	rows, err := c.QueryContext(background,
		"SELECT summary,seq,id FROM "+from+" WHERE kind=?1 AND seq<?2"+filter+" AND (?4='' OR instr(lower(title || ' ' || target || ' ' || summary),lower(?4))>0) ORDER BY seq DESC LIMIT ?5",
		kind, query.before(), status, orEmpty(query.Q), int64(n))
	if err != nil {
		return nil, err
	}
	return collectPageRows(rows)
}

func collectPageRows(rows *sql.Rows) ([]pageRow, error) {
	return scanAll(rows, func(r *pageRow) []any { return []any{&r.summary, &r.seq, &r.id} })
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

func (s *Store) HistoryPage(kind string, query HistoryQuery) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind != "task" {
		return page(s.conn, kind, query)
	}
	var result Page
	err := s.transaction(false, func(c *sql.Conn) error {
		var err error
		result, err = page(c, kind, query)
		if err != nil {
			return err
		}
		return taskHistoryCounts(c, result.Counts)
	})
	return result, err
}

// History retains archived tasks; only the attention filter excludes them.
// Counts describe each whole filter, independent of search and pagination.
func taskHistoryCounts(c *sql.Conn, counts map[string]int64) error {
	rows, err := c.QueryContext(background, "SELECT status,sum(count),sum(CASE WHEN archived=0 THEN count ELSE 0 END) FROM record_counts WHERE kind='task' GROUP BY status HAVING sum(count)>0")
	if err != nil {
		return err
	}
	defer rows.Close()
	counts["all"], counts["active"], counts["attention"] = 0, 0, 0
	for rows.Next() {
		var status string
		var count, unarchived int64
		if err := rows.Scan(&status, &count, &unarchived); err != nil {
			return err
		}
		counts[status] = count
		counts["all"] += count
		for _, attention := range model.AttentionStatuses() {
			if status == attention.String() {
				counts["attention"] += unarchived
			}
		}
	}
	for _, active := range model.ActiveStatuses() {
		counts["active"] += counts[active.String()]
	}
	return rows.Err()
}

// proposalTextLimit bounds the problem and reason of each listed proposal; ProposalDetail returns them whole.
const proposalTextLimit = 2000

// boundProposalText scrubs a listed proposal's complete problem and reason before cutting them to proposalTextLimit
// characters, so the cut cannot leave part of a secret in a shape redaction no longer matches.
func boundProposalText(item json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"problem", "reason"} {
		var text *string
		if json.Unmarshal(fields[name], &text) != nil || text == nil {
			continue
		}
		bounded := []rune(redact.Secrets(*text))
		fields[name], _ = json.Marshal(string(bounded[:min(len(bounded), proposalTextLimit)]))
	}
	return json.Marshal(fields)
}

func (s *Store) ProposalPage(q HistoryQuery) (Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := q.limit()
	rows, err := s.conn.QueryContext(background,
		"SELECT json_set(json_remove(data,'$.prompt','$.evidence'),'$.content_revision',content_revision,'$.cycle',number,'$.cycle_id',cycle_id,'$.mode',mode,'$.prompt','','$.evidence',json('[]')),seq,proposal_id FROM proposal_records WHERE seq<?1 AND (?2='' OR decision=?2) AND (?3='' OR cycle_id=?3) AND (?4='' OR instr(lower(title || ' ' || json_extract(data,'$.problem')),lower(?4))>0) ORDER BY seq DESC LIMIT ?5",
		q.before(), filterAll(q.Status), filterAll(q.Cycle), orEmpty(q.Q), int64(limit+1))
	if err != nil {
		return Page{}, err
	}
	collected, err := collectPageRows(rows)
	if err != nil {
		return Page{}, err
	}
	result := decodePage(collected, limit)
	for i, item := range result.Items {
		if result.Items[i], err = boundProposalText(item); err != nil {
			return Page{}, err
		}
	}
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

// PageLimit bounds each oldest-first task scan below; a full page means more tasks may remain.
const PageLimit = 500

func (s *Store) SchedulingTasks(runID *string) ([]model.Task, error) {
	return listRecords[model.Task](s, schedulingTasksSQL(), runID, PageLimit)
}

func schedulingTasksSQL() string {
	return fmt.Sprintf(`WITH candidates AS (
                SELECT id,seq FROM record_meta WHERE kind='task' AND archived IS NULL
                    AND status IN (%s)
                UNION ALL
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM `+fromMeta+`
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued'
                            AND (?1 IS NULL OR m.run_id=?1)
                            AND (json_extract(r.data,'$.proposal.target') != json_extract(r.data,'$.config.default_branch')
                                OR EXISTS(SELECT 1 FROM pr_reservations p WHERE p.task_id=m.id))
                        ORDER BY m.seq ASC LIMIT ?2
                )
                UNION ALL
                SELECT id,seq FROM (
                    SELECT m.id,m.seq FROM `+fromMeta+`
                        WHERE m.kind='task' AND m.archived IS NULL AND m.status='queued'
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
	return listRecords[model.Task](s, "SELECT r.data FROM "+fromMeta+" WHERE m.kind='task' AND m.status IN ("+statusList(model.ActiveStatuses())+") AND m.archived IS NULL ORDER BY m.seq ASC LIMIT ?1", PageLimit)
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
        WHERE m.kind='task' AND m.status='cancelled'
            AND NOT EXISTS (SELECT 1 FROM json_each(?1) WHERE value=m.id)
            AND EXISTS (SELECT 1 FROM json_each(r.data,'$.sessions') WHERE json_extract(value,'$.status')='running')
        ORDER BY m.seq ASC LIMIT ?2`, string(ids), PageLimit)
}

// PublishingTasksExcept excludes live workers and cleanup claims before decoding their evidence.
func (s *Store) PublishingTasksExcept(excludedIDs []string) ([]model.Task, error) {
	ids, err := json.Marshal(excludedIDs)
	if err != nil {
		return nil, err
	}
	return listRecords[model.Task](s, `SELECT r.data FROM `+fromMeta+`
        WHERE m.kind='task' AND m.status='publishing' AND m.archived IS NULL AND m.discarded IS NULL
            AND NOT EXISTS (SELECT 1 FROM json_each(?1) WHERE value=m.id) AND json_extract(r.data,'$.output_commit') IS NOT NULL
        ORDER BY m.seq ASC LIMIT ?2`, string(ids), PageLimit)
}

// RunningCycles returns the running cycles other than except, the live worker's own, so its evidence is excluded before
// reading and decoding.
func (s *Store) RunningCycles(except string) ([]model.Cycle, error) {
	return listRecords[model.Cycle](s, "SELECT r.data FROM "+fromMeta+" WHERE m.kind='cycle' AND m.status='running' AND m.id!=?1", except)
}

func (s *Store) RunningBaselines() ([]model.BaselineCheck, error) {
	return listRecords[model.BaselineCheck](s, "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')='running'")
}

func (s *Store) StaleBaselines(after string) ([]model.BaselineCheck, error) {
	return listRecords[model.BaselineCheck](s, "SELECT data FROM records WHERE kind='baseline' AND json_extract(data,'$.status')!='running' AND json_extract(data,'$.workspace_removed')=0 ORDER BY rowid<=COALESCE((SELECT rowid FROM records WHERE kind='baseline' AND id=?1),0),rowid LIMIT 100", after)
}

func (s *Store) LatestBaseline() (*model.BaselineCheck, error) {
	id, err := Get[string](s, "settings", "baseline_latest")
	if err != nil || id == nil {
		return nil, err
	}
	return Get[model.BaselineCheck](s, "baseline", *id)
}

func (s *Store) TasksForCycle(id string) ([]model.Task, error) {
	return listRecords[model.Task](s, "SELECT r.data FROM "+fromMeta+" WHERE m.kind='task' AND m.cycle_id=?1 ORDER BY m.seq", id)
}

func (s *Store) Snapshot(fn func(c *sql.Conn) error) error {
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

func (s *Store) HasUnresolvedTasks() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var exists bool
	err := s.conn.QueryRowContext(background, fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM record_counts WHERE kind='task' AND status NOT IN (%s) AND archived=0 AND count>0)", statusList(model.TerminalStatuses()))).Scan(&exists)
	return exists, err
}

func txStartBatch(c *sql.Conn, control model.Control) (model.Control, error) {
	id := model.ID()
	next := control.Clone()
	next.SetMode(model.OperatingModeRunOnce)
	next.Batch = &model.RunBatch{ID: id, Phase: model.BatchPhaseDraining, CycleID: nil}
	next.Error = nil
	next.NextCycleAt = 0
	if _, err := c.ExecContext(background, "UPDATE records SET data=json_set(data,'$.run_id',?1) WHERE kind='task' AND id IN (SELECT id FROM record_meta WHERE kind='task' AND status='queued' AND archived IS NULL)", id); err != nil {
		return model.Control{}, err
	}
	if err := txPut(c, "settings", "control", next); err != nil {
		return model.Control{}, err
	}
	return next, nil
}

func (s *Store) StartBatch(control *model.Control, at time.Time) (model.PlanningCapacity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var capacity model.PlanningCapacity
	var next model.Control
	started, err := s.conditional(func(c *sql.Conn) error {
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
		if !wirejson.Equal(live, *control) || !capacity.Available() {
			return errRollback
		}
		next, err = txStartBatch(c, *control)
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
		if liveFingerprint != fingerprint || !wirejson.Equal(live, expected) || !capacity.Available() {
			return errRollback
		}
		if err := txPut(c, "cycle", cycle.ID, cycle); err != nil {
			return err
		}
		return txPut(c, "settings", "control", control)
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
		active, queued := "active", "queued"
		current, err := pageRows(c, "task", HistoryQuery{Status: &active}, 100)
		if err != nil {
			return err
		}
		waiting, err := pageRows(c, "task", HistoryQuery{Status: &queued}, 100)
		if err != nil {
			return err
		}
		history, err := pageRows(c, "task", HistoryQuery{}, 300)
		if err != nil {
			return err
		}
		result.Tasks = make([]json.RawMessage, 0, 300)
		listed := map[string]struct{}{}
		for _, rows := range [][]pageRow{current, waiting, history} {
			for _, row := range rows {
				if _, seen := listed[row.id]; seen || len(result.Tasks) == 300 {
					continue
				}
				listed[row.id] = struct{}{}
				result.Tasks = append(result.Tasks, json.RawMessage(row.summary))
			}
		}
		twenty := 20
		cycles, err := page(c, "cycle", HistoryQuery{Limit: &twenty})
		if err != nil {
			return err
		}
		result.Cycles = cycles.Items
		hundred := 100
		prs, err := page(c, "pr", HistoryQuery{Limit: &hundred})
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

const cleanupEligible = `kind=?1 AND discarded IS NULL
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
	return queryStrings(s.conn, cleanupCandidatesSQL, kind, cutoff, after)
}

// CleanupEligible rechecks a candidate after the retention pass released the operator gate.
// Archiving another candidate during a slow removal restarts that candidate's retention period.
func (s *Store) CleanupEligible(kind, id, cutoff string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var eligible bool
	err := s.conn.QueryRowContext(background, "SELECT EXISTS(SELECT 1 FROM record_meta WHERE id=?3 AND "+cleanupEligible+")", kind, cutoff, id).Scan(&eligible)
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
	return txPut(s.conn, "pr", recordID, observation)
}

func (s *Store) DecisionMemory(repository string) ([]any, error) {
	return listRecords[any](s, "SELECT data FROM records WHERE kind='decision' AND json_extract(data,'$.repository')=?1 COLLATE NOCASE ORDER BY rowid DESC LIMIT 100", repository)
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
