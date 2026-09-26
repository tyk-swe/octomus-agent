package store

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
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
