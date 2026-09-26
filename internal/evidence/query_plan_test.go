package evidence

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Run evidence reads under the store mutex, so selecting a cycle's tasks must
// seek the cycle projection index rather than decode every saved task.
func TestCycleTasksQuerySeeksTheCycleIndex(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var details []string
	err = s.Snapshot(func(c *sql.Conn) error {
		rows, err := c.QueryContext(store.Background(), "EXPLAIN QUERY PLAN "+cycleTasksQuery, "cycle-a")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			details = append(details, detail)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "USING INDEX meta_cycle (kind=? AND cycle_id=?)") || strings.Contains(plan, "SCAN ") {
		t.Fatalf("cycle task plan:\n%s", plan)
	}
}
