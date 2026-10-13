package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

type InsightDay struct {
	Day                    string `json:"day"`
	Admissions             uint64 `json:"admissions"`
	PlanningAdmissions     uint64 `json:"planning_admissions"`
	TaskAdmissions         uint64 `json:"task_admissions"`
	UnattributedAdmissions uint64 `json:"unattributed_admissions"`
	CyclesStarted          uint64 `json:"cycles_started"`
	TasksCreated           uint64 `json:"tasks_created"`
}

type CategoryInsight struct {
	Category   string `json:"category"`
	Accepted   uint64 `json:"accepted"`
	Rejected   uint64 `json:"rejected"`
	Deferred   uint64 `json:"deferred"`
	Tasks      uint64 `json:"tasks"`
	Published  uint64 `json:"published"`
	InProgress uint64 `json:"in_progress"`
	Unresolved uint64 `json:"unresolved"`
	Cancelled  uint64 `json:"cancelled"`
}

type TierInsight struct {
	Tier       string `json:"tier"`
	Tasks      uint64 `json:"tasks"`
	Published  uint64 `json:"published"`
	Admissions uint64 `json:"admissions"`
}

type Insights struct {
	Days        int               `json:"days"`
	Since       string            `json:"since"`
	Until       string            `json:"until"`
	GeneratedAt string            `json:"generated_at"`
	Daily       []InsightDay      `json:"daily"`
	Cycles      map[string]uint64 `json:"cycles"`
	Tasks       map[string]uint64 `json:"tasks"`
	Categories  []CategoryInsight `json:"categories"`
	Tiers       []TierInsight     `json:"tiers"`
	PRs         map[string]uint64 `json:"prs"`
	Measurement string            `json:"measurement"`
}

const insightMeasurement = "Admissions reserve budget before work starts; they include failed starts and retries and are not completed turns or billed usage. Task outcomes are the current status of tasks created in the window. Proposal decisions count execution cycles started in the window. Pull-request states are the latest observation, not live GitHub state."

// Insights reads existing projections into one read-only window summary; every query runs in a single read transaction.
func (s *Store) Insights(now time.Time, days int) (Insights, error) {
	out := Insights{
		Daily:      []InsightDay{},
		Cycles:     map[string]uint64{},
		Tasks:      map[string]uint64{},
		Categories: []CategoryInsight{},
		Tiers:      []TierInsight{},
		PRs:        map[string]uint64{},
	}
	if days != 7 && days != 30 && days != 90 {
		return out, errors.New("Insights cover 7, 30 or 90 days")
	}
	out.Days = days
	out.Until = model.UTCDay(now)
	out.Since = model.UTCDay(now.AddDate(0, 0, -(days - 1)))
	out.GeneratedAt = model.Timestamp(now)
	since := out.Since
	byDay := map[string]int{}
	for day := now.UTC().AddDate(0, 0, -(days - 1)); ; day = day.AddDate(0, 0, 1) {
		name := model.UTCDay(day)
		if name > out.Until {
			break
		}
		byDay[name] = len(out.Daily)
		out.Daily = append(out.Daily, InsightDay{Day: name})
	}
	for _, name := range []string{"execution", "audit", "running", "completed", "idle", "failed", "interrupted"} {
		out.Cycles[name] = 0
	}
	for status := model.Status(0); status.String() != ""; status++ {
		out.Tasks[status.String()] = 0
	}
	for _, category := range config.Categories() {
		out.Categories = append(out.Categories, CategoryInsight{Category: category})
	}
	for _, tier := range config.Tiers() {
		out.Tiers = append(out.Tiers, TierInsight{Tier: tier})
	}
	for _, name := range []string{"open", "merged", "closed", "unobserved"} {
		out.PRs[name] = 0
	}
	categories := map[string]*CategoryInsight{}
	for i := range out.Categories {
		categories[out.Categories[i].Category] = &out.Categories[i]
	}
	tiers := map[string]*TierInsight{}
	for i := range out.Tiers {
		tiers[out.Tiers[i].Tier] = &out.Tiers[i]
	}
	active := map[string]struct{}{}
	for _, status := range model.ActiveStatuses() {
		active[status.String()] = struct{}{}
	}
	unresolved := map[string]struct{}{model.StatusBlocked.String(): {}, model.StatusFailed.String(): {}}

	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.transaction(false, func(c *sql.Conn) error {
		each := func(query string, scan func(*sql.Rows) error) error {
			rows, err := c.QueryContext(background, query, since)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if err := scan(rows); err != nil {
					return err
				}
			}
			return rows.Err()
		}
		if err := each("SELECT day, sessions FROM usage WHERE day >= ?1", func(rows *sql.Rows) error {
			var day string
			var sessions uint64
			if err := rows.Scan(&day, &sessions); err != nil {
				return err
			}
			if entry, ok := byDay[day]; ok {
				out.Daily[entry].Admissions = sessions
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each("SELECT day, sum(json_extract(data,'$.task_id') IS NULL), sum(json_extract(data,'$.task_id') IS NOT NULL) FROM admissions WHERE day >= ?1 GROUP BY day", func(rows *sql.Rows) error {
			var day string
			var planning, task uint64
			if err := rows.Scan(&day, &planning, &task); err != nil {
				return err
			}
			if entry, ok := byDay[day]; ok {
				out.Daily[entry].PlanningAdmissions = planning
				out.Daily[entry].TaskAdmissions = task
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each("SELECT substr(json_extract(summary,'$.started_at'),1,10), json_extract(summary,'$.mode'), status, count(*) FROM record_meta WHERE kind='cycle' AND json_extract(summary,'$.started_at') >= ?1 GROUP BY 1,2,3", func(rows *sql.Rows) error {
			var day, mode, status string
			var count uint64
			if err := rows.Scan(&day, &mode, &status, &count); err != nil {
				return err
			}
			if entry, ok := byDay[day]; ok {
				out.Daily[entry].CyclesStarted += count
			}
			if _, ok := out.Cycles[mode]; ok {
				out.Cycles[mode] += count
			}
			if _, ok := out.Cycles[status]; ok {
				out.Cycles[status] += count
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each("SELECT substr(json_extract(summary,'$.created_at'),1,10), json_extract(summary,'$.category'), json_extract(summary,'$.tier'), status, count(*) FROM record_meta WHERE kind='task' AND json_extract(summary,'$.created_at') >= ?1 GROUP BY 1,2,3,4", func(rows *sql.Rows) error {
			var day, category, tier, status string
			var count uint64
			if err := rows.Scan(&day, &category, &tier, &status, &count); err != nil {
				return err
			}
			if entry, ok := byDay[day]; ok {
				out.Daily[entry].TasksCreated += count
			}
			if _, ok := out.Tasks[status]; ok {
				out.Tasks[status] += count
			}
			if bucket, ok := categories[category]; ok {
				bucket.Tasks += count
				switch status {
				case model.StatusPublished.String():
					bucket.Published += count
				case model.StatusCancelled.String():
					bucket.Cancelled += count
				}
				if _, ok := unresolved[status]; ok {
					bucket.Unresolved += count
				}
				if _, ok := active[status]; ok || status == model.StatusQueued.String() {
					bucket.InProgress += count
				}
			}
			if bucket, ok := tiers[tier]; ok {
				bucket.Tasks += count
				if status == model.StatusPublished.String() {
					bucket.Published += count
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each("SELECT COALESCE(json_extract(p.data,'$.category'),''), p.decision, count(*) FROM proposal_records p JOIN record_meta m ON m.kind='cycle' AND m.id=p.cycle_id WHERE p.mode='execution' AND json_extract(m.summary,'$.started_at') >= ?1 AND p.decision IN ('accepted','rejected','deferred') GROUP BY 1,2", func(rows *sql.Rows) error {
			var category, decision string
			var count uint64
			if err := rows.Scan(&category, &decision, &count); err != nil {
				return err
			}
			if bucket, ok := categories[category]; ok {
				switch decision {
				case model.DecisionAccepted:
					bucket.Accepted += count
				case model.DecisionRejected:
					bucket.Rejected += count
				case model.DecisionDeferred:
					bucket.Deferred += count
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each("SELECT json_extract(m.summary,'$.tier'), count(*) FROM admissions a JOIN record_meta m ON m.kind='task' AND m.id=json_extract(a.data,'$.task_id') WHERE a.day >= ?1 GROUP BY 1", func(rows *sql.Rows) error {
			var tier string
			var count uint64
			if err := rows.Scan(&tier, &count); err != nil {
				return err
			}
			if bucket, ok := tiers[tier]; ok {
				bucket.Admissions += count
			}
			return nil
		}); err != nil {
			return err
		}
		return each(`SELECT COALESCE((
				SELECT json_extract(p.summary,'$.pr.state') FROM record_meta p
				WHERE p.kind='pr' AND p.repository = delivered.repository COLLATE NOCASE
					AND json_extract(p.summary,'$.pr.number') = delivered.number
				ORDER BY p.seq DESC LIMIT 1
			), ''), count(*) FROM (
				SELECT DISTINCT m.repository, json_extract(m.summary,'$.pr_number') AS number
				FROM record_meta m
				WHERE m.kind='task' AND m.status='published' AND json_extract(m.summary,'$.created_at') >= ?1
					AND json_extract(m.summary,'$.pr_number') IS NOT NULL
			) delivered GROUP BY 1`, func(rows *sql.Rows) error {
			var state string
			var count uint64
			if err := rows.Scan(&state, &count); err != nil {
				return err
			}
			if _, ok := out.PRs[state]; ok {
				out.PRs[state] += count
			} else {
				out.PRs["unobserved"] += count
			}
			return nil
		})
	})
	if err != nil {
		return Insights{}, err
	}
	for i := range out.Daily {
		attributed := out.Daily[i].PlanningAdmissions + out.Daily[i].TaskAdmissions
		if out.Daily[i].Admissions > attributed {
			out.Daily[i].UnattributedAdmissions = out.Daily[i].Admissions - attributed
		}
	}
	out.Measurement = insightMeasurement
	return out, nil
}
