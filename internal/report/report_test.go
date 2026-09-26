package report

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// ledgerFixture is a minimal state database whose admissions and usage rows
// are computed while SQLite steps through them: TEMP views shadow the main
// tables of the same name, and a malformed raw value fails that row's step
// (json() and json_extract() reject it), the shape of an I/O or corruption
// error partway through a scan. Primary keys match each report query's order,
// so rows stream from the index instead of a sorter that would evaluate them
// all before the first row.
func ledgerFixture(t *testing.T) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(store.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	for _, statement := range []string{
		"CREATE TABLE records(kind TEXT NOT NULL, id TEXT NOT NULL, data TEXT NOT NULL)",
		"CREATE TABLE admissions(id TEXT NOT NULL, at TEXT NOT NULL, raw TEXT NOT NULL, PRIMARY KEY(at,id))",
		"CREATE TABLE usage(day TEXT PRIMARY KEY, raw TEXT NOT NULL)",
		"CREATE TEMP VIEW admissions AS SELECT id, at, json(raw) AS data FROM main.admissions",
		"CREATE TEMP VIEW usage AS SELECT day, json_extract(raw,'$.sessions') AS sessions FROM main.usage",
	} {
		if _, err := conn.ExecContext(store.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

func insertAdmission(t *testing.T, conn *sql.Conn, id, at, raw string) {
	t.Helper()
	if _, err := conn.ExecContext(store.Background(), "INSERT INTO main.admissions(id,at,raw) VALUES(?1,?2,?3)", id, at, raw); err != nil {
		t.Fatal(err)
	}
}

func insertUsage(t *testing.T, conn *sql.Conn, day, raw string) {
	t.Helper()
	if _, err := conn.ExecContext(store.Background(), "INSERT INTO main.usage(day,raw) VALUES(?1,?2)", day, raw); err != nil {
		t.Fatal(err)
	}
}

func admissionJSON(t *testing.T, at string) string {
	t.Helper()
	admission := store.NewAdmission("cycle-a", nil, "discovery", config.NewRoute("fixture", "low"))
	admission.At = at
	data, err := json.Marshal(admission)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A step error partway through the admissions or usage scan fails the report
// instead of returning a ledger or daily table cut short at that row.
func TestAssembleFailsWhenALedgerScanStopsWithAnError(t *testing.T) {
	t.Run("admissions", func(t *testing.T) {
		conn := ledgerFixture(t)
		insertAdmission(t, conn, "a1", "2026-09-01T00:00:00Z", admissionJSON(t, "2026-09-01T00:00:00Z"))
		insertAdmission(t, conn, "a2", "2026-09-01T00:00:01Z", "not json")
		insertAdmission(t, conn, "a3", "2026-09-01T00:00:02Z", admissionJSON(t, "2026-09-01T00:00:02Z"))
		report, err := assemble(conn)
		if err == nil || !strings.Contains(err.Error(), "malformed JSON") {
			t.Fatalf("assemble: err=%v admissions=%d", err, len(report.Admissions))
		}
	})
	t.Run("usage", func(t *testing.T) {
		conn := ledgerFixture(t)
		insertUsage(t, conn, "2026-09-01", `{"sessions":1}`)
		insertUsage(t, conn, "2026-09-02", "not json")
		insertUsage(t, conn, "2026-09-03", `{"sessions":3}`)
		report, err := assemble(conn)
		if err == nil || !strings.Contains(err.Error(), "malformed JSON") {
			t.Fatalf("assemble: err=%v daily=%d", err, len(report.Daily))
		}
	})
}

// A readable ledger is reported in full, and an empty one stays an empty JSON
// array rather than null.
func TestAssembleReportsCompleteAndEmptyLedgers(t *testing.T) {
	conn := ledgerFixture(t)
	report, err := assemble(conn)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"admissions": report.Admissions, "daily": report.Daily, "cycles": report.Cycles, "tasks": report.Tasks})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"admissions":[],"cycles":[],"daily":[],"tasks":[]}` || !report.HasAdmissionLedger {
		t.Fatalf("empty ledger: %s %v", data, report.HasAdmissionLedger)
	}

	insertAdmission(t, conn, "a1", "2026-09-01T00:00:00Z", admissionJSON(t, "2026-09-01T00:00:00Z"))
	insertAdmission(t, conn, "a2", "2026-09-01T00:00:01Z", admissionJSON(t, "2026-09-01T00:00:01Z"))
	insertUsage(t, conn, "2026-09-01", `{"sessions":3}`)
	insertUsage(t, conn, "2026-09-02", `{"sessions":1}`)
	report, err = assemble(conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Admissions) != 2 || len(report.Daily) != 2 {
		t.Fatalf("admissions=%d daily=%d", len(report.Admissions), len(report.Daily))
	}
	if first := report.Daily[0]; first.Day != "2026-09-01" || first.Admissions != 3 || first.AttributedAdmissions != 2 || first.UnattributedAdmissions != 1 {
		t.Fatalf("daily: %+v", report.Daily)
	}
	if second := report.Daily[1]; second.Admissions != 1 || second.AttributedAdmissions != 0 || second.UnattributedAdmissions != 1 {
		t.Fatalf("daily: %+v", report.Daily)
	}
}

// An admission time the ledger cannot parse fails the report rather than
// dropping that admission from the daily, cycle and task counts.
func TestAssembleFailsOnAnUnparseableAdmissionTime(t *testing.T) {
	conn := ledgerFixture(t)
	insertAdmission(t, conn, "a1", "2026-09-01T00:00:00Z", admissionJSON(t, "2026-09-01T00:00:00Z"))
	insertAdmission(t, conn, "a2", "2026-09-01T00:00:01Z", admissionJSON(t, "not-a-time"))
	report, err := assemble(conn)
	if err == nil || !strings.Contains(err.Error(), `"not-a-time"`) {
		t.Fatalf("assemble: err=%v admissions=%d", err, len(report.Admissions))
	}
}

// Per-cycle rows split each cycle's admissions into planning (no task) and
// task admissions, report wall time only for a completed cycle, name every
// decision even when no proposal has it, and count completed sessions. Tier
// rows cover the configured tiers only.
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
	// A running cycle has no wall time: the field is present and null.
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
	// A task outside the configured tiers keeps its own row and admissions.
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
