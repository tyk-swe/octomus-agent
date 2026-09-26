package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// A step error on a later row closes the rows inside Next, after which Close
// reports nothing. The dashboard counts must surface such an error rather than
// return the groups read before it. sum() overflows only in the second group.
func TestDashboardReportsMidIterationErrors(t *testing.T) {
	s := fullOpen(t)
	execStore(t, s, "INSERT INTO record_counts VALUES ('task','aaa',0,1),('task','zzz',0,9223372036854775807),('task','zzz',1,1)")
	result, err := s.Dashboard()
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("Dashboard() = %v, %v; want the integer overflow", result.Counts, err)
	}
	// The failed read left no open rows on the pinned connection.
	execStore(t, s, "DELETE FROM record_counts WHERE status='zzz'")
	result, err = s.Dashboard()
	if err != nil || len(result.Counts) != 1 || result.Counts["aaa"] != 1 {
		t.Fatalf("Dashboard() = %v, %v; want only aaa", result.Counts, err)
	}
}

// Read-only exports decode their rows through QueryRecords and RecordAt, so
// the rows arrive in query order, an empty read is an empty list rather than
// null, and an error on a later row fails the read instead of cutting it short.
func TestQueryRecordsAndRecordAtReadCallerOwnedConnections(t *testing.T) {
	type record struct {
		N int `json:"n"`
	}
	s := fullOpen(t)
	read := func(fn func(c *sql.Conn) error) {
		t.Helper()
		if err := s.Snapshot(fn); err != nil {
			t.Fatal(err)
		}
	}
	read(func(c *sql.Conn) error {
		values, err := QueryRecords[record](c, "SELECT data FROM records WHERE kind='x' ORDER BY id")
		if err != nil || values == nil || len(values) != 0 {
			t.Fatalf("empty QueryRecords = %#v, %v; want a non-nil empty list", values, err)
		}
		missing, err := RecordAt[record](c, "x", "a")
		if err != nil || missing != nil {
			t.Fatalf("absent RecordAt = %#v, %v; want nil, nil", missing, err)
		}
		return nil
	})
	execStore(t, s, `INSERT INTO records VALUES ('x','b','{"n":2}'),('x','a','{"n":1}'),('x','c','{"n":3}')`)
	read(func(c *sql.Conn) error {
		values, err := QueryRecords[record](c, "SELECT data FROM records WHERE kind=?1 ORDER BY id", "x")
		if err != nil || !reflect.DeepEqual(values, []record{{1}, {2}, {3}}) {
			t.Fatalf("QueryRecords = %#v, %v", values, err)
		}
		found, err := RecordAt[record](c, "x", "b")
		if err != nil || found == nil || found.N != 2 {
			t.Fatalf("RecordAt = %#v, %v", found, err)
		}
		// json() fails while SQLite steps to row b, after row a was read.
		values, err = QueryRecords[record](c, "SELECT CASE id WHEN 'b' THEN json('not json') ELSE data END FROM records WHERE kind='x' ORDER BY id")
		if err == nil || values != nil || !strings.Contains(err.Error(), "malformed JSON") {
			t.Fatalf("QueryRecords with a failing row = %#v, %v; want the step error", values, err)
		}
		values, err = QueryRecords[record](c, "SELECT data FROM no_such_table")
		if err == nil || values != nil {
			t.Fatalf("QueryRecords on a missing table = %#v, %v; want an error", values, err)
		}
		return nil
	})
}

// A saved value followed by anything but white space is refused, as
// json.Unmarshal refuses it, also when the extra text is a stray bracket.
func TestDecodeJSONRefusesTrailingData(t *testing.T) {
	for _, raw := range []string{`{"n":1}}`, `{"n":1}]`, `{"n":1} {}`, `{"n":1} 2`, `{"n":1}x`, `[1]]`} {
		var value any
		if err := decodeJSON([]byte(raw), &value); err == nil || err.Error() != "trailing JSON data" {
			t.Errorf("decodeJSON(%s) = %v; want trailing JSON data", raw, err)
		}
	}
	for _, raw := range []string{`{"n":1}`, "{\"n\":1}\n", ` {"n":1} `, `12345678901234567890`} {
		var value any
		if err := decodeJSON([]byte(raw), &value); err != nil {
			t.Errorf("decodeJSON(%s) = %v", raw, err)
		}
	}
	var value any
	if err := decodeJSON([]byte(`12345678901234567890`), &value); err != nil || value != json.Number("12345678901234567890") {
		t.Fatalf("decodeJSON(number) = %#v, %v; want the exact json.Number", value, err)
	}
}

// The scheduler reads its view twice per tick under the store mutex, so the
// plan must reach tasks only through keyed index searches: walking every task
// entry of an index keyed by kind alone, or building an automatic index on
// each call, would make every tick cost grow with the whole task history.
// Only the bounded candidate list and its bounded windows may be scanned.
func TestSchedulingPlanNeverWalksTaskHistory(t *testing.T) {
	s := fullOpen(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, runID := range []any{nil, "run"} {
		rows, err := s.conn.QueryContext(background, "EXPLAIN QUERY PLAN "+schedulingTasksSQL(), runID)
		if err != nil {
			t.Fatal(err)
		}
		var steps []string
		for rows.Next() {
			var id, parent, unused int64
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, detail)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, step := range steps {
			scan, bounded := strings.CutPrefix(step, "SCAN ")
			bounded = !bounded || scan == "candidates" || strings.HasPrefix(scan, "(subquery-")
			if strings.HasSuffix(step, "(kind=?)") || strings.Contains(step, "AUTOMATIC") || !bounded {
				t.Fatalf("scheduling plan step %q walks task history; plan:\n%s", step, strings.Join(steps, "\n"))
			}
		}
	}
}

// A saved record that no longer decodes names itself, so an operator whose
// service paused on it can find the row, and the wrapped error keeps its
// class for the API's status mapping.
func TestUnreadableRecordsNameTheirIdentity(t *testing.T) {
	s := fullOpen(t)
	execStore(t, s, `INSERT INTO records VALUES ('task','broken','{"id":"broken","status":"executing"}')`)
	_, err := s.SchedulingTasks(nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Saved record broken is unreadable: ") || !errors.As(err, new(*wirejson.Error)) {
		t.Fatalf("SchedulingTasks = %v; want the unreadable record named, as a wirejson error", err)
	}
	var task model.Task
	found, err := s.Get("task", "broken", &task)
	if !found || err == nil || !strings.HasPrefix(err.Error(), "Saved task broken is unreadable: ") || !errors.As(err, new(*wirejson.Error)) {
		t.Fatalf("Get = %v, %v; want the unreadable record named, as a wirejson error", found, err)
	}

	type record struct {
		N int `json:"n"`
	}
	for raw, want := range map[string]string{
		`{"id":"wrong-type","n":"x"}`: "Saved record wrong-type is unreadable: ",
		`{"id":"trailing","n":1}}`:    "Saved record trailing is unreadable: trailing JSON data",
		`{"n":"x"}`:                   "Saved record (unknown id) is unreadable: ",
		`{"id":7,"n":"x"}`:            "Saved record (unknown id) is unreadable: ",
		`{"id":"cut`:                  "Saved record (unknown id) is unreadable: ",
	} {
		values, err := decodeAll[record]([][]byte{[]byte(`{"id":"fine","n":1}`), []byte(raw)})
		if values != nil || err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("decodeAll(%s) = %v, %v; want %q", raw, values, err, want)
		}
	}
}
