package store_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestFreshGoSchemaAndReopen(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	if version := queryInt(t, raw(t, path), "PRAGMA user_version"); version != store.SupportedSchemaVersion {
		t.Fatalf("schema version = %d", version)
	}
	for _, table := range []string{"records", "record_meta", "proposal_records", "admissions", "notification_outbox"} {
		if n := queryInt(t, raw(t, path), "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table); n != 1 {
			t.Fatalf("missing %s", table)
		}
	}
	must(t, s.Close())
	s = open(t, path)
	must(t, s.Close())
	r, err := store.OpenReadOnly(path, "schema test")
	must(t, err)
	must(t, r.Close())
}

func TestUnsupportedStateIsRefusedWithoutChanges(t *testing.T) {
	for _, version := range []int{0, 6, 8} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db := raw(t, path)
			exec(t, db, "CREATE TABLE records(kind TEXT,id TEXT,data TEXT)")
			exec(t, db, "PRAGMA user_version="+strconv.Itoa(version))
			must(t, db.Close())
			before, err := os.ReadFile(path)
			must(t, err)
			if _, err := store.Open(path); err == nil || !strings.Contains(err.Error(), "requires a fresh version-7 data directory") {
				t.Fatalf("writable open: %v", err)
			}
			if _, err := store.OpenReadOnly(path, "schema test"); err == nil || !strings.Contains(err.Error(), "requires a fresh version-7 data directory") {
				t.Fatalf("read-only open: %v", err)
			}
			after, err := os.ReadFile(path)
			must(t, err)
			if string(before) != string(after) {
				t.Fatal("unsupported database changed")
			}
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
					t.Fatalf("unsupported database acquired %s", suffix)
				}
			}
		})
	}
}
