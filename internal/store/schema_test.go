// The fresh Go-owned schema and the refusal of databases from other versions.

package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestFreshGoSchemaAndReopen(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	if version := queryInt(t, raw(t, path), "PRAGMA user_version"); version != 7 {
		t.Fatalf("schema version = %d", version)
	}
	for _, table := range []string{"records", "record_meta", "proposal_records", "admissions", "notification_outbox"} {
		if n := queryInt(t, raw(t, path), "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table); n != 1 {
			t.Fatalf("missing %s", table)
		}
	}
	for _, trigger := range []string{"project_record_insert", "project_record_update"} {
		var sql string
		must(t, raw(t, path).QueryRow("SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?", trigger).Scan(&sql))
		for _, decision := range model.Decisions() {
			if !strings.Contains(sql, "'"+decision+"'") {
				t.Fatalf("%s does not count %s decisions", trigger, decision)
			}
		}
	}
	must(t, s.Close())
	s = open(t, path)
	must(t, s.Close())
	r, err := store.OpenReadOnly(path, "schema test")
	must(t, err)
	must(t, r.Close())
}

func TestUnsupportedStateRefused(t *testing.T) {
	t.Parallel()
	create := func(t *testing.T, version int) string {
		path := filepath.Join(t.TempDir(), "state.db")
		db := raw(t, path)
		exec(t, db, "CREATE TABLE records(kind TEXT,id TEXT,data TEXT)")
		exec(t, db, "PRAGMA user_version="+strconv.Itoa(version))
		must(t, db.Close())
		return path
	}
	// A refusal must leave the file untouched and create nothing beside it:
	// no journal or WAL side file and no backup.
	refused := func(t *testing.T, path string, open func() error, want string) {
		t.Helper()
		before, err := os.ReadFile(path)
		must(t, err)
		if err := open(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%v, want %q", err, want)
		}
		after, err := os.ReadFile(path)
		must(t, err)
		if string(before) != string(after) {
			t.Fatal("refused database changed")
		}
		matches, err := filepath.Glob(path + "*")
		must(t, err)
		if len(matches) != 1 {
			t.Fatalf("refusal left files: %v", matches)
		}
	}
	writable := func(path string) func() error {
		return func() error {
			s, err := store.Open(path)
			if s != nil {
				s.Close()
			}
			return err
		}
	}
	readOnly := func(path string) func() error {
		return func() error {
			r, err := store.OpenReadOnly(path, "schema test")
			if r != nil {
				r.Close()
			}
			return err
		}
	}
	for _, tc := range []struct {
		version int
		want    string
	}{
		{0, "requires a fresh data directory"},
		{6, "requires a fresh data directory"},
		{8, "is newer than this release's version 7"},
	} {
		t.Run(strconv.Itoa(tc.version), func(t *testing.T) {
			path := create(t, tc.version)
			refused(t, path, writable(path), tc.want)
			refused(t, path, readOnly(path), tc.want)
		})
	}
	// A plan with pending migrations still refuses what its last step cannot cover.
	steps := []store.Migration{
		store.NewMigration("test-v8", func(context.Context, *sql.Conn) error { return nil }),
		store.NewMigration("test-v9", func(context.Context, *sql.Conn) error { return nil }),
	}
	planWritable := func(path string) func() error {
		return func() error {
			s, err := store.OpenPlan(path, store.SchemaDDL(), steps...)
			if s != nil {
				s.Close()
			}
			return err
		}
	}
	planReadOnly := func(path string) func() error {
		return func() error {
			r, err := store.OpenReadOnlyPlan(path, "schema test", store.SchemaDDL(), steps...)
			if r != nil {
				r.Close()
			}
			return err
		}
	}
	t.Run("newer-than-last-migration", func(t *testing.T) {
		path := create(t, 10)
		refused(t, path, planWritable(path), "is newer than this release's version 9")
		refused(t, path, planReadOnly(path), "is newer than this release's version 9")
	})
	t.Run("read-only-needs-upgrade", func(t *testing.T) {
		path := create(t, 7)
		refused(t, path, planReadOnly(path), "must be upgraded to version 9 before a read-only schema test")
	})
}
