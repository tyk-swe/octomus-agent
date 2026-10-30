package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
)

type HistoryQuery struct {
	Before *int64
	Limit  *int
	Status *string
	Q      *string
	Cycle  *string
	// repository is set from the store handle's scope by page entry points;
	// callers never set it directly.
	repository string
}

func (q HistoryQuery) scoped(repo string) HistoryQuery {
	q.repository = repo
	return q
}

type Page[T any] struct {
	Items      []T              `json:"items"`
	Counts     map[string]int64 `json:"counts"`
	NextCursor *int64           `json:"next_cursor"`
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

func page[T any](c *sql.Conn, kind string, query HistoryQuery) (Page[T], error) {
	limit := query.limit()
	rows, err := pageRows(c, kind, query, limit+1)
	if err != nil {
		return Page[T]{}, err
	}
	return decodePage[T](rows, limit)
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
	args := []any{kind, query.before(), status, orEmpty(query.Q), int64(n)}
	if kind == "cycle" && filterAll(query.Cycle) != "" {
		filter += " AND id=?6"
		args = append(args, *query.Cycle)
	}
	if query.repository != "" {
		args = append(args, query.repository)
		filter += fmt.Sprintf(" AND repository=?%d COLLATE NOCASE", len(args))
	}
	rows, err := c.QueryContext(background,
		"SELECT summary,seq,id FROM "+from+" WHERE kind=?1 AND seq<?2"+filter+` AND (?4='' OR CASE WHEN kind='task' THEN EXISTS (
			SELECT 1 FROM json_each(summary) AS field
			WHERE field.key IN ('id','cycle_id','title','category','tier','target','branch','status','pr_url','error','blocked_reason')
			AND field.type='text' AND instr(lower(field.value),lower(?4))>0
		) ELSE instr(lower(title || ' ' || target || ' ' || summary),lower(?4))>0 END) ORDER BY seq DESC LIMIT ?5`,
		args...)
	if err != nil {
		return nil, err
	}
	return collectPageRows(rows)
}

// record_meta retains complete source strings. Typed projections apply the
// current secret set before display bounds, so a rotated credential cannot be
// cut into a prefix that the current scrubber no longer recognizes.
func boundSummary(item any) {
	switch summary := item.(type) {
	case *TaskSummary:
		summary.Title = boundText(summary.Title, 200)
		if summary.Error != nil {
			message := boundText(*summary.Error, 512)
			summary.Error = &message
		}
	case *CycleSummary:
		if summary.Error != nil {
			message := boundText(*summary.Error, 512)
			summary.Error = &message
		}
	case *PRSummary:
		summary.PR.Title = boundText(summary.PR.Title, 200)
	case *ProposalSummary:
		summary.Problem = boundText(summary.Problem, proposalTextLimit)
		summary.Reason = boundText(summary.Reason, proposalTextLimit)
	}
}

func boundText(text string, limit int) string {
	runes := []rune(redact.Secrets(text))
	return string(runes[:min(len(runes), limit)])
}

func collectPageRows(rows *sql.Rows) ([]pageRow, error) {
	return scanAll(rows, func(r *pageRow) []any { return []any{&r.summary, &r.seq, &r.id} })
}

func decodePage[T any](rows []pageRow, limit int) (Page[T], error) {
	var next *int64
	if len(rows) > limit {
		rows = rows[:limit]
		seq := rows[len(rows)-1].seq
		next = &seq
	}
	items := make([]T, 0, len(rows))
	for _, r := range rows {
		var item T
		if err := json.Unmarshal([]byte(r.summary), &item); err != nil {
			return Page[T]{}, fmt.Errorf("Saved summary %s is unreadable: %w", r.id, err)
		}
		boundSummary(&item)
		items = append(items, item)
	}
	return Page[T]{Items: items, Counts: map[string]int64{}, NextCursor: next}, nil
}

func historyPage[T any](s *Store, kind string, query HistoryQuery) (Page[T], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query = query.scoped(s.repo)
	if kind != "task" {
		return page[T](s.conn, kind, query)
	}
	var result Page[T]
	err := s.transaction(false, func(c *sql.Conn) error {
		var err error
		result, err = page[T](c, kind, query)
		if err != nil {
			return err
		}
		return taskHistoryCounts(c, result.Counts, s.repo)
	})
	return result, err
}

func (s *Store) TaskPage(query HistoryQuery) (Page[TaskSummary], error) {
	return historyPage[TaskSummary](s, "task", query)
}

func (s *Store) CyclePage(query HistoryQuery) (Page[CycleSummary], error) {
	return historyPage[CycleSummary](s, "cycle", query)
}

func (s *Store) PRPage(query HistoryQuery) (Page[PRSummary], error) {
	return historyPage[PRSummary](s, "pr", query)
}

// History retains archived tasks; only the attention filter excludes them.
// Counts describe each whole filter, independent of search and pagination.
func taskHistoryCounts(c *sql.Conn, counts map[string]int64, repository string) error {
	query := "SELECT status,sum(count),sum(CASE WHEN archived=0 THEN count ELSE 0 END) FROM record_counts WHERE kind='task' GROUP BY status HAVING sum(count)>0"
	var args []any
	if repository != "" {
		// record_counts is instance-wide; scoped counts come from the projection.
		query = "SELECT status,count(*),sum(CASE WHEN archived IS NULL THEN 1 ELSE 0 END) FROM record_meta WHERE kind='task' AND repository=?1 COLLATE NOCASE GROUP BY status"
		args = []any{repository}
	}
	rows, err := c.QueryContext(background, query, args...)
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

func (s *Store) ProposalPage(q HistoryQuery) (Page[ProposalSummary], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := q.limit()
	from, repoFilter, args := "proposal_records p", "", []any{q.before(), filterAll(q.Status), filterAll(q.Cycle), orEmpty(q.Q), int64(limit + 1)}
	if s.repo != "" {
		from += " JOIN record_meta m ON m.kind='cycle' AND m.id=p.cycle_id"
		args = append(args, s.repo)
		repoFilter = " AND m.repository=?6 COLLATE NOCASE"
	}
	rows, err := s.conn.QueryContext(background,
		"SELECT json_set(json_remove(p.data,'$.prompt','$.evidence'),'$.content_revision',p.content_revision,'$.cycle',p.number,'$.cycle_id',p.cycle_id,'$.mode',p.mode,'$.prompt','','$.evidence',json('[]')),p.seq,p.proposal_id FROM "+from+" WHERE p.seq<?1 AND (?2='' OR p.decision=?2) AND (?3='' OR p.cycle_id=?3) AND (?4='' OR instr(lower(p.title || ' ' || json_extract(p.data,'$.problem')),lower(?4))>0)"+repoFilter+" ORDER BY p.seq DESC LIMIT ?5",
		args...)
	if err != nil {
		return Page[ProposalSummary]{}, err
	}
	collected, err := collectPageRows(rows)
	if err != nil {
		return Page[ProposalSummary]{}, err
	}
	result, err := decodePage[ProposalSummary](collected, limit)
	if err != nil {
		return Page[ProposalSummary]{}, err
	}
	countQuery, countArgs := "SELECT p.decision,count(*) FROM proposal_records p WHERE (?1='' OR p.cycle_id=?1) GROUP BY p.decision", []any{filterAll(q.Cycle)}
	if s.repo != "" {
		countQuery = "SELECT p.decision,count(*) FROM proposal_records p JOIN record_meta m ON m.kind='cycle' AND m.id=p.cycle_id WHERE (?1='' OR p.cycle_id=?1) AND m.repository=?2 COLLATE NOCASE GROUP BY p.decision"
		countArgs = append(countArgs, s.repo)
	}
	counts, err := s.conn.QueryContext(background, countQuery, countArgs...)
	if err != nil {
		return Page[ProposalSummary]{}, err
	}
	defer counts.Close()
	for counts.Next() {
		var decision string
		var count int64
		if err := counts.Scan(&decision, &count); err != nil {
			return Page[ProposalSummary]{}, err
		}
		result.Counts[decision] = count
	}
	return result, counts.Err()
}

func (s *Store) ProposalDetail(cycle, id string) (*ProposalDetail, error) {
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
	var detail ProposalDetail
	if err := json.Unmarshal([]byte(data), &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

type Dashboard struct {
	Tasks          []TaskSummary    `json:"tasks"`
	Cycles         []CycleSummary   `json:"cycles"`
	PRs            []PRSummary      `json:"prs"`
	Events         []model.Event    `json:"events"`
	Counts         map[string]int64 `json:"counts"`
	AttentionTasks []TaskSummary    `json:"attention_tasks"`
	MergedPRs      int64            `json:"merged_prs"`
	SessionsToday  int64            `json:"sessions_today"`
}

func (s *Store) Dashboard() (Dashboard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Dashboard
	err := s.transaction(false, func(c *sql.Conn) error {
		result.Counts = map[string]int64{}
		if err := statusCounts(c, result.Counts, s.repo); err != nil {
			return err
		}
		active, queued := "active", "queued"
		current, err := pageRows(c, "task", HistoryQuery{Status: &active}.scoped(s.repo), 100)
		if err != nil {
			return err
		}
		waiting, err := pageRows(c, "task", HistoryQuery{Status: &queued}.scoped(s.repo), 100)
		if err != nil {
			return err
		}
		history, err := pageRows(c, "task", HistoryQuery{}.scoped(s.repo), 300)
		if err != nil {
			return err
		}
		result.Tasks = make([]TaskSummary, 0, 300)
		listed := map[string]struct{}{}
		for _, rows := range [][]pageRow{current, waiting, history} {
			for _, row := range rows {
				if _, seen := listed[row.id]; seen || len(result.Tasks) == 300 {
					continue
				}
				listed[row.id] = struct{}{}
				var task TaskSummary
				if err := json.Unmarshal([]byte(row.summary), &task); err != nil {
					return err
				}
				boundSummary(&task)
				result.Tasks = append(result.Tasks, task)
			}
		}
		twenty := 20
		cycles, err := page[CycleSummary](c, "cycle", HistoryQuery{Limit: &twenty}.scoped(s.repo))
		if err != nil {
			return err
		}
		result.Cycles = cycles.Items
		hundred := 100
		prs, err := page[PRSummary](c, "pr", HistoryQuery{Limit: &hundred}.scoped(s.repo))
		if err != nil {
			return err
		}
		result.PRs = prs.Items
		if result.Events, err = repositoryEvents(c, s.repo, nil); err != nil {
			return err
		}
		for i := range result.Events {
			result.Events[i].Message = boundText(result.Events[i].Message, 512)
		}
		if result.SessionsToday, err = sessionsOn(c, s.repo, model.Today()); err != nil {
			return err
		}
		mergedQuery, mergedArgs := "SELECT COALESCE(sum(count),0) FROM record_counts WHERE kind='pr' AND status='merged'", []any(nil)
		if s.repo != "" {
			mergedQuery, mergedArgs = "SELECT count(*) FROM record_meta WHERE kind='pr' AND status='merged' AND repository=?1 COLLATE NOCASE", []any{s.repo}
		}
		if err := c.QueryRowContext(background, mergedQuery, mergedArgs...).Scan(&result.MergedPRs); err != nil {
			return err
		}
		attention, five := "attention", 5
		attentionPage, err := page[TaskSummary](c, "task", HistoryQuery{Status: &attention, Limit: &five}.scoped(s.repo))
		if err != nil {
			return err
		}
		result.AttentionTasks = attentionPage.Items
		return nil
	})
	return result, err
}

func statusCounts(c *sql.Conn, counts map[string]int64, repository string) error {
	query := fmt.Sprintf("SELECT status,sum(count) FROM record_counts WHERE kind='task' AND (status NOT IN (%s) OR archived=0) GROUP BY status HAVING sum(count)>0", statusList(model.AttentionStatuses()))
	var args []any
	if repository != "" {
		query = fmt.Sprintf("SELECT status,count(*) FROM record_meta WHERE kind='task' AND repository=?1 COLLATE NOCASE AND (status NOT IN (%s) OR archived IS NULL) GROUP BY status", statusList(model.AttentionStatuses()))
		args = []any{repository}
	}
	rows, err := c.QueryContext(background, query, args...)
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
