package report

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
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
