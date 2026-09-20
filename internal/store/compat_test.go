package store_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/evidence"
	"github.com/tyk-swe/octomus-agent/internal/report"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// frozenState is the Rust-captured fixture tests/compatibility_capture.py wrote
// from the frozen reference: synthetic legacy records, the legacy and current
// sqlite_master rows, and the reference's read-only exports before and after
// its own migration.
type frozenState struct {
	Reference       string          `json:"reference"`
	Records         [][]any         `json:"records"`
	LegacySchema    [][]string      `json:"legacy_schema"`
	LegacyUsage     json.RawMessage `json:"legacy_usage"`
	LegacyEvidence  json.RawMessage `json:"legacy_evidence"`
	CurrentSchema   [][]string      `json:"current_schema"`
	CurrentUsage    json.RawMessage `json:"current_usage"`
	CurrentEvidence json.RawMessage `json:"current_evidence"`
}

const (
	frozenCycle = "11111111-1111-4111-8111-111111111111"
	frozenTask  = "22222222-2222-4222-8222-222222222222"
)

func loadFrozenState(t *testing.T) frozenState {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "compatibility", "state.json"))
	must(t, err)
	var state frozenState
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	must(t, dec.Decode(&state))
	if state.Reference != "3c2b5cd50924033873d7f740f9df44daee5685db" {
		t.Fatalf("unexpected fixture reference %s", state.Reference)
	}
	return state
}

// frozenLegacyDatabase rebuilds the reference's legacy database: its captured
// schema statements, the synthetic records and the pre-ledger usage counter.
func frozenLegacyDatabase(t *testing.T, path string, state frozenState) {
	t.Helper()
	db := raw(t, path)
	for _, row := range state.LegacySchema {
		exec(t, db, row[3])
	}
	for _, record := range state.Records {
		exec(t, db, "INSERT INTO records VALUES(?,?,?)", record[0], record[1], canonical(t, record[2]))
	}
	exec(t, db, "INSERT INTO usage VALUES('2026-01-01',3)")
	db.Close()
}

func schemaRows(t *testing.T, db *sql.DB) [][]string {
	t.Helper()
	rows, err := db.Query("SELECT type,name,tbl_name,sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type,name")
	must(t, err)
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var a, b, c, d string
		must(t, rows.Scan(&a, &b, &c, &d))
		out = append(out, []string{a, b, c, d})
	}
	return out
}

func assertSchema(t *testing.T, label string, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d schema rows, reference has %d", label, len(got), len(want))
	}
	for i := range want {
		if strings.Join(got[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Fatalf("%s: schema row %d differs\nreference: %v\ngo:        %v", label, i, want[i], got[i])
		}
	}
}

// exportsMatch compares a Go export with the reference's, ignoring only the
// generation time the capture already replaced.
func exportsMatch(t *testing.T, label string, got map[string]any, want json.RawMessage) {
	t.Helper()
	if _, ok := got["generated_at"].(string); !ok {
		t.Fatalf("%s: no generated_at", label)
	}
	got["generated_at"] = "<export-time>"
	var reference any
	dec := json.NewDecoder(strings.NewReader(string(want)))
	dec.UseNumber()
	must(t, dec.Decode(&reference))
	if canonical(t, got) != canonical(t, reference) {
		t.Fatalf("%s differs from the frozen reference\nreference: %s\ngo:        %s", label, canonical(t, reference), canonical(t, got))
	}
}

func recordsByKey(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query("SELECT kind,id,data FROM records ORDER BY kind,id")
	must(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var kind, id, data string
		must(t, rows.Scan(&kind, &id, &data))
		out[kind+"/"+id] = data
	}
	return out
}

// Acceptance criteria 1, 2, 5 and 8: Go reads the Rust-created legacy fixture
// read-only, migrates it to the reference schema, keeps every canonical record,
// and its exports match the frozen reference before and after migration.
func TestFrozenStateContracts(t *testing.T) {
	state := loadFrozenState(t)
	path := statePath(t)
	frozenLegacyDatabase(t, path, state)
	before, err := os.ReadFile(path)
	must(t, err)
	usage, err := report.UsageReport(path)
	must(t, err)
	exportsMatch(t, "legacy usage", usage, state.LegacyUsage)
	run, err := evidence.ExportRun(path, frozenCycle)
	must(t, err)
	exportsMatch(t, "legacy evidence", run, state.LegacyEvidence)
	after, err := os.ReadFile(path)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("read-only exports rewrote the legacy database")
	}
	for _, sidecar := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(path + sidecar); !os.IsNotExist(err) {
			t.Fatalf("read-only export left %s", sidecar)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("read-only exports created files: %v", entries)
	}
	db := raw(t, path)
	original := recordsByKey(t, db)
	s := open(t, path)
	must(t, s.Close())
	assertSchema(t, "migrated", schemaRows(t, db), state.CurrentSchema)
	if v := queryInt(t, db, "PRAGMA user_version"); v != store.SupportedSchemaVersion {
		t.Fatalf("user_version %d", v)
	}
	migrated := recordsByKey(t, db)
	for key, data := range original {
		if migrated[key] != data {
			t.Fatalf("migration rewrote %s", key)
		}
	}
	for _, record := range state.Records {
		key := record[0].(string) + "/" + record[1].(string)
		var saved any
		must(t, json.Unmarshal([]byte(migrated[key]), &saved))
		if canonical(t, saved) != canonical(t, record[2]) {
			t.Fatalf("%s no longer matches the fixture", key)
		}
	}
	if len(migrated) != len(state.Records) {
		t.Fatalf("migration added records: %d", len(migrated))
	}
	if n := queryInt(t, db, "SELECT count(*) FROM admissions"); n != 0 {
		t.Fatalf("migration invented %d admissions", n)
	}
	if n := queryInt(t, db, "SELECT count(*) FROM record_meta"); n != 2 {
		t.Fatalf("%d projected records", n)
	}
	usage, err = report.UsageReport(path)
	must(t, err)
	exportsMatch(t, "current usage", usage, state.CurrentUsage)
	run, err = evidence.ExportRun(path, frozenCycle)
	must(t, err)
	exportsMatch(t, "current evidence", run, state.CurrentEvidence)
	// Reopening an upgraded database is a no-op for saved history.
	schema := schemaRows(t, db)
	s = open(t, path)
	must(t, s.Close())
	assertSchema(t, "reopened", schemaRows(t, db), schema)
	if again := recordsByKey(t, db); canonical(t, again) != canonical(t, migrated) {
		t.Fatal("reopening rewrote records")
	}
	if n := queryInt(t, db, "SELECT count(*) FROM admissions"); n != 0 {
		t.Fatalf("reopening invented %d admissions", n)
	}
	if n := queryInt(t, db, "SELECT sessions FROM usage WHERE day='2026-01-01'"); n != 3 {
		t.Fatalf("reopening changed historical usage to %d", n)
	}
}

// Acceptance criterion 1: a fresh Go database has the reference schema. The
// fixture's records and usage tables were created by the legacy capture script,
// so their DDL text differs from the service's own only in whitespace.
func TestFreshDatabaseMatchesReferenceSchema(t *testing.T) {
	state := loadFrozenState(t)
	path := statePath(t)
	s := open(t, path)
	must(t, s.Close())
	db := raw(t, path)
	squeeze := func(rows [][]string) [][]string {
		var out [][]string
		for _, row := range rows {
			out = append(out, []string{row[0], row[1], row[2], strings.Join(strings.Fields(row[3]), "")})
		}
		return out
	}
	assertSchema(t, "fresh", squeeze(schemaRows(t, db)), squeeze(state.CurrentSchema))
	if v := queryInt(t, db, "PRAGMA user_version"); v != store.SupportedSchemaVersion {
		t.Fatalf("user_version %d", v)
	}
	if mode := queryString(t, db, "PRAGMA journal_mode"); mode != "wal" {
		t.Fatalf("journal mode %s persisted", mode)
	}
}

// Acceptance criterion 7: a future schema is refused before any migration runs,
// for both the service store and the read-only exports.
func TestFutureSchemaIsRefusedWithoutRepair(t *testing.T) {
	state := loadFrozenState(t)
	path := statePath(t)
	frozenLegacyDatabase(t, path, state)
	db := raw(t, path)
	exec(t, db, "PRAGMA user_version=7")
	exec(t, db, "CREATE TABLE future_only(x)")
	before := schemaRows(t, db)
	bytes, err := os.ReadFile(path)
	must(t, err)
	if _, err := store.Open(path); err == nil || !strings.Contains(err.Error(), "schema version 7 is newer than this executable supports (6)") {
		t.Fatalf("future schema opened: %v", err)
	}
	if _, err := report.UsageReport(path); err == nil || !strings.Contains(err.Error(), "schema version 7") {
		t.Fatalf("future schema reported: %v", err)
	}
	if _, err := evidence.ExportRun(path, frozenCycle); err == nil || !strings.Contains(err.Error(), "schema version 7") {
		t.Fatalf("future schema exported: %v", err)
	}
	assertSchema(t, "refused", schemaRows(t, db), before)
	after, err := os.ReadFile(path)
	must(t, err)
	if string(bytes) != string(after) || queryInt(t, db, "PRAGMA user_version") != 7 {
		t.Fatal("refusal changed the database")
	}
}

// Acceptance criterion 5: read-only access never creates missing state and
// refuses writes.
func TestReadOnlyAccessNeverCreatesOrWritesState(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nested", "state.db")
	if _, err := store.OpenReadOnly(missing, "probe"); err == nil || !strings.Contains(err.Error(), "Cannot open existing state database for read-only probe") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatal("read-only open created the directory")
	}
	path := statePath(t)
	s := open(t, path)
	must(t, s.Put("x", "a", 1))
	must(t, s.Close())
	r, err := store.OpenReadOnly(path, "probe")
	must(t, err)
	defer r.Close()
	if _, err := r.Conn.ExecContext(store.Background(), "INSERT INTO records VALUES('x','b','2')"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("read-only connection wrote: %v", err)
	}
	if _, err := r.Conn.ExecContext(store.Background(), "PRAGMA user_version=6"); err == nil {
		t.Fatal("read-only connection changed the schema version")
	}
}
