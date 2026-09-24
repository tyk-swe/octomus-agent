// Package report is local, read-only usage reporting. It never opens the
// database through store.Open, which owns writable state.
package report

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Measurement is the one paragraph every report carries about what admissions mean.
const Measurement = "Admissions reserve budget before work starts. They include failed starts and retries; they are not completed turns or billed usage. Completed session counts describe persisted thread records; a repair thread can contain multiple turns. Cycle wall time excludes subsequent task execution. No provider charges or merge status are inferred."

type Daily struct {
	Day                    string `json:"day"`
	Admissions             uint64 `json:"admissions"`
	AttributedAdmissions   uint64 `json:"attributed_admissions"`
	UnattributedAdmissions uint64 `json:"unattributed_admissions"`
}

type CycleRow struct {
	ID                        string          `json:"id"`
	Mode                      model.CycleMode `json:"mode"`
	Number                    uint64          `json:"number"`
	Status                    string          `json:"status"`
	StartedAt                 string          `json:"started_at"`
	CompletedAt               *string         `json:"completed_at"`
	WallSeconds               *float64        `json:"wall_seconds"`
	PlanningAdmissions        uint64          `json:"planning_admissions"`
	TaskAdmissions            uint64          `json:"task_admissions"`
	RecordedCompletedSessions int             `json:"recorded_completed_sessions"`
	Decisions                 map[string]int  `json:"decisions"`
	Error                     *string         `json:"error"`
}

type TaskRow struct {
	ID                        string       `json:"id"`
	CycleID                   string       `json:"cycle_id"`
	Tier                      string       `json:"tier"`
	Status                    model.Status `json:"status"`
	Route                     config.Route `json:"route"`
	RepairRoute               config.Route `json:"repair_route"`
	Admissions                uint64       `json:"admissions"`
	RecordedCompletedSessions int          `json:"recorded_completed_sessions"`
	CreatedAt                 string       `json:"created_at"`
	UpdatedAt                 string       `json:"updated_at"`
	PRURL                     *string      `json:"pr_url"`
	Error                     *string      `json:"error"`
}

type TierRow struct {
	Tier          string `json:"tier"`
	ObservedTasks int    `json:"observed_tasks"`
	Admissions    uint64 `json:"admissions"`
}

type Report struct {
	SchemaVersion      uint32            `json:"schema_version"`
	GeneratedAt        string            `json:"generated_at"`
	HasAdmissionLedger bool              `json:"has_admission_ledger"`
	Measurement        string            `json:"measurement"`
	Daily              []Daily           `json:"daily"`
	Cycles             []CycleRow        `json:"cycles"`
	Tasks              []TaskRow         `json:"tasks"`
	Tiers              []TierRow         `json:"tiers"`
	Admissions         []store.Admission `json:"admissions"`
}

func records[T any](c *sql.Conn, kind string) ([]T, error) {
	rows, err := c.QueryContext(store.Background(), "SELECT data FROM records WHERE kind=?1 ORDER BY id", kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []T{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var value T
		if err := json.Unmarshal([]byte(data), &value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// UsageReport reads one consistent snapshot of the state database and returns
// the redacted report as generic JSON.
func UsageReport(path string) (map[string]any, error) {
	r, err := store.OpenReadOnly(path, "usage reporting")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var report Report
	// One consistent snapshot even while the service is admitting work.
	err = r.Snapshot(func(c *sql.Conn) error {
		var err error
		report, err = assemble(c)
		return err
	})
	if err != nil {
		return nil, err
	}
	return store.RedactedValue(report)
}

func assemble(c *sql.Conn) (Report, error) {
	ctx := store.Background()
	var hasLedger bool
	if err := c.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='admissions')").Scan(&hasLedger); err != nil {
		return Report{}, err
	}
	admissions := []store.Admission{}
	if hasLedger {
		rows, err := c.QueryContext(ctx, "SELECT data FROM admissions ORDER BY at,id")
		if err != nil {
			return Report{}, err
		}
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				rows.Close()
				return Report{}, err
			}
			var admission store.Admission
			if err := json.Unmarshal([]byte(data), &admission); err != nil {
				rows.Close()
				return Report{}, err
			}
			admissions = append(admissions, admission)
		}
		if err := rows.Close(); err != nil {
			return Report{}, err
		}
	}
	cycles, err := records[model.Cycle](c, "cycle")
	if err != nil {
		return Report{}, err
	}
	tasks, err := records[model.Task](c, "task")
	if err != nil {
		return Report{}, err
	}
	dailyCounts := map[string]uint64{}
	type pair struct{ planning, task uint64 }
	cycleCounts := map[string]pair{}
	taskCounts := map[string]uint64{}
	for _, admission := range admissions {
		at, err := time.Parse(time.RFC3339, admission.At)
		if err != nil {
			return Report{}, err
		}
		dailyCounts[model.UTCDay(at)]++
		counts := cycleCounts[admission.CycleID]
		if admission.TaskID != nil {
			counts.task++
			taskCounts[*admission.TaskID]++
		} else {
			counts.planning++
		}
		cycleCounts[admission.CycleID] = counts
	}
	daily := []Daily{}
	rows, err := c.QueryContext(ctx, "SELECT day,sessions FROM usage ORDER BY day")
	if err != nil {
		return Report{}, err
	}
	for rows.Next() {
		var day string
		var total int64
		if err := rows.Scan(&day, &total); err != nil {
			rows.Close()
			return Report{}, err
		}
		recorded := dailyCounts[day]
		unattributed := uint64(0)
		if uint64(total) > recorded {
			unattributed = uint64(total) - recorded
		}
		daily = append(daily, Daily{Day: day, Admissions: uint64(total), AttributedAdmissions: recorded, UnattributedAdmissions: unattributed})
	}
	if err := rows.Close(); err != nil {
		return Report{}, err
	}
	cycleRows := make([]CycleRow, 0, len(cycles))
	for _, cycle := range cycles {
		counts := cycleCounts[cycle.ID]
		var wall *float64
		if cycle.CompletedAt != nil {
			start, startErr := time.Parse(time.RFC3339, cycle.StartedAt)
			end, endErr := time.Parse(time.RFC3339, *cycle.CompletedAt)
			if startErr == nil && endErr == nil {
				seconds := float64(end.Sub(start).Milliseconds()) / 1000.0
				wall = &seconds
			}
		}
		decisions := map[string]int{}
		for _, decision := range model.Decisions() {
			decisions[decision] = 0
			for _, p := range cycle.Proposals {
				if p.Decision == decision {
					decisions[decision]++
				}
			}
		}
		cycleRows = append(cycleRows, CycleRow{
			ID: cycle.ID, Mode: cycle.Mode, Number: cycle.Number, Status: cycle.Status,
			StartedAt: cycle.StartedAt, CompletedAt: cycle.CompletedAt, WallSeconds: wall,
			PlanningAdmissions: counts.planning, TaskAdmissions: counts.task,
			RecordedCompletedSessions: model.CompletedSessions(cycle.Sessions),
			Decisions:                 decisions, Error: cycle.Error,
		})
	}
	taskRows := make([]TaskRow, 0, len(tasks))
	for _, task := range tasks {
		taskRows = append(taskRows, TaskRow{
			ID: task.ID, CycleID: task.CycleID, Tier: task.Proposal.Tier,
			Status: task.Status, Route: task.Route, RepairRoute: task.Config.RepairRoute,
			Admissions:                taskCounts[task.ID],
			RecordedCompletedSessions: model.CompletedSessions(task.Sessions),
			CreatedAt:                 task.CreatedAt, UpdatedAt: task.UpdatedAt,
			PRURL: task.PRURL, Error: task.Error,
		})
	}
	tiers := []TierRow{}
	for _, tier := range config.Tiers() {
		row := TierRow{Tier: tier}
		for _, t := range tasks {
			if t.Proposal.Tier == tier {
				row.ObservedTasks++
				row.Admissions += taskCounts[t.ID]
			}
		}
		tiers = append(tiers, row)
	}
	return Report{
		SchemaVersion: 1, GeneratedAt: model.Now(), HasAdmissionLedger: hasLedger,
		Measurement: Measurement, Daily: daily, Cycles: cycleRows, Tasks: taskRows,
		Tiers: tiers, Admissions: admissions,
	}, nil
}
