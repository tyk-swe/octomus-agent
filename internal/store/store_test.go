// Records, budget reservations, planning capacity, atomic plan commits and redacted exports.

package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/report"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func admission(at string) store.Admission {
	a := store.NewAdmission("cycle", str("task"), "repair", config.NewRoute("fixture", "medium"))
	a.At = at
	return a
}

func saveConfig(t *testing.T, s *store.Store, edit func(*config.Config)) config.Config {
	t.Helper()
	c := config.Default()
	edit(&c)
	must(t, s.Put("settings", "config", c))
	return c
}

func startBatch(t *testing.T, s *store.Store) model.Control {
	t.Helper()
	control := model.DefaultControl()
	capacity, started, err := s.StartBatchIfAffordable(&control, time.Now())
	must(t, err)
	if !started {
		t.Fatalf("batch did not start: capacity %+v", capacity)
	}
	return control
}

func TestDurableAndBudgetAtomic(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 1 })
	must(t, s.Put("x", "a", []int{1, 2}))
	must(t, s.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", config.NewRoute("fixture", "low"))))
	if err := s.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", config.NewRoute("fixture", "low"))); err == nil {
		t.Fatal("second reservation exceeded the daily budget")
	} else if !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("budget error is not classified: %v", err)
	}
	must(t, s.Close())
	s = open(t, path)
	value, err := store.Get[[]int](s, "x", "a")
	must(t, err)
	if value == nil || len(*value) != 2 || (*value)[0] != 1 || (*value)[1] != 2 {
		t.Fatalf("durable record differs: %v", value)
	}
	today, err := s.SessionsToday()
	must(t, err)
	if today != 1 {
		t.Fatalf("sessions today %d", today)
	}
}

func TestPlanningCapacity(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	for agents, required := range map[uint64]uint64{8: 12, 9: 13, 10: 14} {
		saveConfig(t, s, func(c *config.Config) { c.DiscoveryAgents = agents })
		capacity, err := s.PlanningCapacity()
		must(t, err)
		if capacity.Required != required || capacity.Status != model.PlanningCapacityStatusReady {
			t.Fatalf("agents %d: %+v", agents, capacity)
		}
		must(t, capacity.EnsureAvailable())
	}
	saveConfig(t, s, func(c *config.Config) { c.DiscoveryAgents = 9; c.MaxSessionsPerDay = 12 })
	capacity, err := s.PlanningCapacity()
	must(t, err)
	if capacity.Status != model.PlanningCapacityStatusLimitTooLow || !strings.Contains(capacity.Message(), "cannot fund") {
		t.Fatalf("%+v %q", capacity, capacity.Message())
	}
	if err := capacity.EnsureAvailable(); err == nil || !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("limit too low is not budget exhaustion: %v", err)
	}
	saveConfig(t, s, func(c *config.Config) { c.DiscoveryAgents = 9; c.MaxSessionsPerDay = 14 })
	for i, expected := range [][2]uint64{{1, 13}, {2, 12}} {
		must(t, s.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", config.NewRoute("fixture", "low"))))
		capacity, err := s.PlanningCapacity()
		must(t, err)
		if capacity.Used != expected[0] || capacity.Remaining != expected[1] {
			t.Fatalf("after %d reservations: %+v", i+1, capacity)
		}
		if i == 0 {
			if capacity.Status != model.PlanningCapacityStatusReady {
				t.Fatalf("%+v", capacity)
			}
			must(t, capacity.EnsureAvailable())
		} else {
			if capacity.Status != model.PlanningCapacityStatusDailyExhausted || !strings.Contains(capacity.Message(), "Wait until UTC midnight") {
				t.Fatalf("%+v %q", capacity, capacity.Message())
			}
			if capacity.EnsureAvailable() == nil {
				t.Fatal("exhausted capacity was available")
			}
		}
	}
	s2 := open(t, statePath(t))
	saveConfig(t, s2, func(c *config.Config) { c.DiscoveryAgents = 9; c.MaxSessionsPerDay = 14 })
	for range 2 {
		must(t, s2.ReserveSession(0, admission("2026-03-01T23:30:00Z")))
	}
	at := time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)
	capacity, err = s2.PlanningCapacityAt(at)
	must(t, err)
	reset := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC).Unix()
	if capacity.Day != "2026-03-01" || capacity.Used != 2 || capacity.Remaining != 12 || capacity.Status != model.PlanningCapacityStatusDailyExhausted || capacity.NextResetAt != reset {
		t.Fatalf("%+v", capacity)
	}
	capacity, err = s2.PlanningCapacityAt(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	must(t, err)
	if capacity.Day != "2026-03-02" || capacity.Used != 0 || capacity.Remaining != 14 || capacity.Status != model.PlanningCapacityStatusReady {
		t.Fatalf("%+v", capacity)
	}
}

func usageReport(t *testing.T, path string) map[string]any {
	t.Helper()
	value, err := report.UsageReport(path)
	must(t, err)
	return value
}

func daily(t *testing.T, value map[string]any) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, row := range value["daily"].([]any) {
		rows = append(rows, row.(map[string]any))
	}
	return rows
}

func number(value any) float64 {
	switch v := value.(type) {
	case json.Number:
		f, _ := v.Float64()
		return f
	case float64:
		return v
	}
	return -1
}

func TestAdmissionLedger(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 2 })
	first := admission("2026-09-09T23:59:59Z")
	must(t, s.ReserveSession(0, first))
	if err := s.ReserveSession(0, first); err == nil {
		t.Fatal("duplicate admission id was accepted")
	}
	must(t, s.ReserveSession(0, admission("2026-09-09T23:59:59.500Z")))
	if err := s.ReserveSession(0, admission("2026-09-09T23:59:59.900Z")); err == nil || !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("third same-day admission: %v", err)
	}
	must(t, s.ReserveSession(0, admission("2026-09-10T00:00:00Z")))
	must(t, s.Close())
	s = open(t, path)
	must(t, s.ReserveSession(0, admission("2026-09-10T09:00:01+09:00")))
	must(t, s.Close())
	value := usageReport(t, path)
	if len(value["admissions"].([]any)) != 4 {
		t.Fatal(canonical(t, value["admissions"]))
	}
	rows := daily(t, value)
	if len(rows) != 2 {
		t.Fatal(canonical(t, rows))
	}
	for i, day := range []string{"2026-09-09", "2026-09-10"} {
		row := rows[i]
		if row["day"] != day || number(row["admissions"]) != 2 || number(row["attributed_admissions"]) != 2 || number(row["unattributed_admissions"]) != 0 {
			t.Fatal(canonical(t, row))
		}
	}
	if value["has_admission_ledger"] != true {
		t.Fatal(canonical(t, value))
	}
}

func TestCommitPlanAtomicity(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	control := map[string]any{
		"paused": false, "mode": "run_once", "cycle_number": 1, "next_cycle_at": 0,
		"error": nil, "idle_streak": 3, "context_fingerprint": "",
		"batch": map[string]any{"id": "run-1", "phase": "planning", "cycle_id": "cycle-1"},
	}
	must(t, s.Put("settings", "control", control))
	queued := reviewTask()
	queued.CycleID = "cycle-1"
	plan := cycleFor(queued)
	plan.ID = "cycle-1"
	plan.RunID = str("run-1")
	plan.Proposals[0].Reconsiders = []string{"missing-task-id"}
	plan.DecisionMemory = []any{map[string]any{"id": "decision-1", "repository": "Fixture/Project"}}
	if err := s.CommitPlan(plan, []model.Task{queued}); err == nil {
		t.Fatal("plan with a missing rediscovery target committed")
	}
	assertEmpty := func(taskID string) {
		t.Helper()
		if c, _ := store.Get[model.Cycle](s, "cycle", "cycle-1"); c != nil {
			t.Fatal("cycle survived the failed commit")
		}
		if tk, _ := store.Get[model.Task](s, "task", taskID); tk != nil {
			t.Fatal("task survived the failed commit")
		}
		if d, _, _ := s.GetValue("decision", "decision-1"); d != nil {
			t.Fatal("decision survived the failed commit")
		}
		saved, _, err := s.GetValue("settings", "control")
		must(t, err)
		if !equalJSON(t, saved, control) {
			t.Fatal(canonical(t, saved))
		}
	}
	assertEmpty(queued.ID)
	superseding := reviewTask()
	superseding.CycleID = "cycle-1"
	superseding.Supersedes = []string{"missing"}
	plan = cycleFor(superseding)
	plan.ID = "cycle-1"
	plan.RunID = str("run-1")
	if err := s.CommitPlan(plan, []model.Task{superseding}); err == nil {
		t.Fatal("plan with a missing superseded task committed")
	}
	assertEmpty(superseding.ID)
	valid := reviewTask()
	valid.CycleID = "cycle-1"
	plan = cycleFor(valid)
	plan.ID = "cycle-1"
	plan.RunID = str("run-1")
	plan.DecisionMemory = []any{map[string]any{"id": "decision-1", "repository": "Fixture/Project"}}
	must(t, s.CommitPlan(plan, []model.Task{valid}))
	saved, err := store.Get[model.Control](s, "settings", "control")
	must(t, err)
	if saved.Batch == nil || saved.Batch.Phase != model.BatchPhaseExecuting || saved.IdleStreak != 0 {
		t.Fatalf("%+v", saved)
	}
	if d, _, _ := s.GetValue("decision", "decision-1"); d == nil {
		t.Fatal("decision memory missing")
	}
	empty := cycleFor(valid)
	empty.ID = "cycle-2"
	must(t, s.CommitPlan(empty, nil))
	saved, err = store.Get[model.Control](s, "settings", "control")
	must(t, err)
	if saved.IdleStreak != 1 || saved.Batch == nil || saved.Batch.Phase != model.BatchPhaseExecuting {
		t.Fatalf("%+v", saved)
	}
}

func TestRedactedValue(t *testing.T) {
	t.Parallel()
	whitespace := "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"
	for _, separator := range whitespace {
		t.Run(fmt.Sprintf("U+%04X", separator), func(t *testing.T) {
			input := "before bEaReR" + string(separator) + "\t" + "synthetic-private-credential after"
			value, err := store.RedactedValue(map[string]any{"nested": []any{input}, "count": 7})
			must(t, err)
			if got := canonical(t, value); got != `{"count":7,"nested":["before [redacted] after"]}` {
				t.Fatalf("redacted export = %s", got)
			}
		})
	}
}
