package report

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestUsageReportAttributesAdmissionsWallTimeAndDecisionsPerCycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	route := config.NewRoute("fixture", "low")
	session := func(id, status string) model.Session {
		saved := model.NewSession(id, "discovery", route)
		saved.Status = status
		return saved
	}
	proposal := func(id, decision string) model.Proposal {
		return model.Proposal{ID: id, Title: id, Tier: "M", Decision: decision}
	}
	completedAt := "2026-09-12T01:00:00Z"
	finished := model.Cycle{
		ID: "c1", Number: 1, Status: model.CycleCompleted,
		StartedAt: "2026-09-12T00:00:00Z", CompletedAt: &completedAt,
		Proposals: []model.Proposal{
			proposal("p1", model.DecisionAccepted),
			proposal("p2", model.DecisionAccepted),
			proposal("p3", model.DecisionRejected),
		},
		Sessions: []model.Session{
			session("s1", model.SessionCompleted),
			session("s2", model.SessionFailed),
			session("s3", model.SessionCompleted),
		},
		Repository: "fixture/project",
	}
	running := model.Cycle{
		ID: "c2", Number: 2, Status: model.CycleRunning,
		StartedAt: "2026-09-12T02:00:00Z", Repository: "fixture/project",
	}
	for _, cycle := range []model.Cycle{finished, running} {
		if err := s.Put("cycle", cycle.ID, cycle); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	task := func(id, cycleID, tier string) model.Task {
		saved := model.Task{
			ID: id, CycleID: cycleID, Proposal: proposal(id, model.DecisionAccepted),
			Status: model.StatusQueued, Route: route, Config: cfg,
			Sessions:  []model.Session{session(id+"-exec", model.SessionCompleted)},
			CreatedAt: "2026-09-12T00:30:00Z", UpdatedAt: "2026-09-12T00:30:00Z",
		}
		saved.Proposal.Tier = tier
		return saved
	}
	configured := task("t1", "c1", "M")
	unknown := task("t2", "c2", "ZZ")
	for _, saved := range []model.Task{configured, unknown} {
		if err := s.Put("task", saved.ID, saved); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(at, cycleID string, taskID *string, role string) {
		t.Helper()
		admission := store.NewAdmission(cycleID, taskID, role, route)
		admission.At = at
		if err := s.ReserveSession(0, admission); err != nil {
			t.Fatal(err)
		}
	}
	admit("2026-09-12T00:01:00Z", "c1", nil, "discovery")
	admit("2026-09-12T00:02:00Z", "c1", nil, "adversary-a")
	admit("2026-09-12T00:31:00Z", "c1", &configured.ID, "executor")
	admit("2026-09-12T00:32:00Z", "c1", &configured.ID, "reviewer")
	admit("2026-09-12T00:33:00Z", "c1", &configured.ID, "repair")
	admit("2026-09-12T02:31:00Z", "c2", &unknown.ID, "executor")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	value, err := UsageReport(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		HasAdmissionLedger bool       `json:"has_admission_ledger"`
		Daily              []Daily    `json:"daily"`
		Cycles             []CycleRow `json:"cycles"`
		Tasks              []TaskRow  `json:"tasks"`
		Tiers              []TierRow  `json:"tiers"`
		Admissions         []any      `json:"admissions"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	if !report.HasAdmissionLedger || len(report.Admissions) != 6 || len(report.Cycles) != 2 || len(report.Tasks) != 2 {
		t.Fatalf("report inventory: %s", data)
	}
	if len(report.Daily) != 1 || report.Daily[0] != (Daily{Day: "2026-09-12", Admissions: 6, AttributedAdmissions: 6}) {
		t.Fatalf("daily: %+v", report.Daily)
	}

	c1, c2 := report.Cycles[0], report.Cycles[1]
	if c1.ID != "c1" || c1.PlanningAdmissions != 2 || c1.TaskAdmissions != 3 || c1.RecordedCompletedSessions != 2 {
		t.Fatalf("completed cycle row: %+v", c1)
	}
	if c1.WallSeconds == nil || *c1.WallSeconds != 3600 {
		t.Fatalf("completed cycle wall time: %v", c1.WallSeconds)
	}
	if c2.ID != "c2" || c2.PlanningAdmissions != 0 || c2.TaskAdmissions != 1 || c2.RecordedCompletedSessions != 0 {
		t.Fatalf("running cycle row: %+v", c2)
	}
	cycles, _ := value["cycles"].([]any)
	runningRow, _ := cycles[1].(map[string]any)
	if wall, present := runningRow["wall_seconds"]; !present || wall != nil || c2.CompletedAt != nil {
		t.Fatalf("running cycle wall time: %v", runningRow)
	}
	decisions := func(counts map[string]int) map[string]int {
		all := map[string]int{}
		for _, decision := range model.Decisions() {
			all[decision] = counts[decision]
		}
		return all
	}
	if want := decisions(map[string]int{model.DecisionAccepted: 2, model.DecisionRejected: 1}); !reflect.DeepEqual(c1.Decisions, want) {
		t.Fatalf("completed cycle decisions: %v, want %v", c1.Decisions, want)
	}
	if want := decisions(nil); !reflect.DeepEqual(c2.Decisions, want) {
		t.Fatalf("running cycle decisions: %v, want %v", c2.Decisions, want)
	}

	byTask := map[string]TaskRow{}
	for _, row := range report.Tasks {
		byTask[row.ID] = row
	}
	if row := byTask["t1"]; row.Tier != "M" || row.Admissions != 3 || row.RecordedCompletedSessions != 1 {
		t.Fatalf("configured tier task row: %+v", row)
	}
	if row := byTask["t2"]; row.Tier != "ZZ" || row.Admissions != 1 {
		t.Fatalf("unknown tier task row: %+v", row)
	}
	if len(report.Tiers) != len(config.Tiers()) {
		t.Fatalf("tier rows: %+v", report.Tiers)
	}
	for i, tier := range config.Tiers() {
		want := TierRow{Tier: tier}
		if tier == "M" {
			want = TierRow{Tier: "M", ObservedTasks: 1, Admissions: 3}
		}
		if report.Tiers[i] != want {
			t.Fatalf("tier rows: %+v", report.Tiers)
		}
	}
}
