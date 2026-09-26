package store

import (
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
