package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotPRLookupUsesCompleteIndexedIdentity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.snapshot(func(c *sql.Conn) error {
		rows, err := c.QueryContext(background, "EXPLAIN QUERY PLAN "+snapshotPRObservationsQuery, "fixture/project", 5)
		if err != nil {
			return err
		}
		defer rows.Close()
		indexed := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				return err
			}
			indexed = indexed || (strings.Contains(detail, "meta_pr_identity") && strings.Contains(detail, "<expr>="))
		}
		if !indexed {
			t.Fatal("PR evidence lookup must use both repository and PR number in the identity index")
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}
