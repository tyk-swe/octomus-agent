// The migration runner, the pre-upgrade backup and the golden v0.1.0 database.

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

// Facts recorded in internal/store/testdata/state-v0.1.0.db, generated from
// v0.1.0's `chain` scenario by scripts/golden-state.py.
const (
	goldenCompletedCycle = "4306e9ae-0833-47e5-b569-14b7b1d7d4f8"
	goldenFailedCycle    = "0346c423-2759-4071-bd68-a87ed546ebcb"
	goldenFirstTask      = "e166cfa0-816b-4173-9d30-e22addfa1c6a"
	goldenSecondTask     = "b0ed2705-4eb3-4d41-bc9f-d3a55e9e9556"
	goldenThirdTask      = "d1351dc0-f45b-45f9-9069-5f6a9866c3dd"
)

func golden(t *testing.T) string {
	t.Helper()
	return testutil.GoldenState(t, "0.1.0", filepath.Join(t.TempDir(), "state.db"))
}

func backups(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".v*-backup-*")
	must(t, err)
	return matches
}

var (
	sqlWhitespace = regexp.MustCompile(`\s+`)
	sqlSpacing    = regexp.MustCompile(` *([(),]) *`)
)

// normalizeSQL collapses formatting differences: an ALTER TABLE in a migration
// rewrites the stored CREATE TABLE text differently than fresh DDL does.
func normalizeSQL(sql string) string {
	return sqlSpacing.ReplaceAllString(sqlWhitespace.ReplaceAllString(sql, " "), "$1")
}

func master(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT type,name,tbl_name,COALESCE(sql,'') FROM sqlite_master ORDER BY type,name,tbl_name")
	must(t, err)
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var typ, name, tbl, ddl string
		must(t, rows.Scan(&typ, &name, &tbl, &ddl))
		out = append(out, typ+"|"+name+"|"+tbl+"|"+normalizeSQL(ddl))
	}
	must(t, rows.Err())
	return out
}

func recordDump(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("SELECT kind,id,data FROM records ORDER BY kind,id")
	must(t, err)
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var kind, id, data string
		must(t, rows.Scan(&kind, &id, &data))
		out = append(out, kind+"|"+id+"|"+data)
	}
	must(t, rows.Err())
	return out
}

func equalStrings(a, b []string) (string, bool) {
	if len(a) != len(b) {
		return "length", false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] + " != " + b[i], false
		}
	}
	return "", true
}

// probeSteps is the test-only v7->v8 migration: a new table and index, an added
// column and a record rewrite that exercises the projection triggers.
func probeSteps(ctx context.Context, c *sql.Conn) error {
	if _, err := c.ExecContext(ctx, "CREATE TABLE migration_probe(task_id TEXT PRIMARY KEY, status TEXT NOT NULL)"); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "INSERT INTO migration_probe(task_id,status) SELECT id,json_extract(data,'$.status') FROM records WHERE kind='task'"); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "CREATE INDEX migration_probe_status ON migration_probe(status)"); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "ALTER TABLE usage ADD COLUMN probe INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := c.ExecContext(ctx, "UPDATE records SET data=json_set(data,'$.probe_note','v8') WHERE kind='task'")
	return err
}

func v7GoldenDDL(t *testing.T) string {
	t.Helper()
	// Keep this synthetic v7->v8 plan independent of main's real migrations.
	// The historical golden is the authority for its complete baseline DDL.
	rows, err := raw(t, golden(t)).Query("SELECT sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 ELSE 2 END,name")
	must(t, err)
	defer rows.Close()
	var baseline strings.Builder
	for rows.Next() {
		var statement string
		must(t, rows.Scan(&statement))
		baseline.WriteString(statement + ";\n")
	}
	must(t, rows.Err())
	return baseline.String()
}

func v8Plan(t *testing.T) (string, []store.Migration) {
	t.Helper()
	const usage = `CREATE TABLE usage (
                day TEXT PRIMARY KEY, sessions INTEGER NOT NULL
            );`
	ddl := v7GoldenDDL(t)
	if strings.Count(ddl, usage) != 1 {
		t.Fatal("golden usage DDL changed; update the v8 test plan")
	}
	v8 := strings.Replace(ddl, usage, `CREATE TABLE usage (
                day TEXT PRIMARY KEY, sessions INTEGER NOT NULL, probe INTEGER NOT NULL DEFAULT 0
            );`, 1)
	v8 += "\nCREATE TABLE migration_probe(task_id TEXT PRIMARY KEY, status TEXT NOT NULL);\n"
	v8 += "CREATE INDEX migration_probe_status ON migration_probe(status);\n"
	return v8, []store.Migration{store.NewMigration("v8-probe", probeSteps)}
}

func TestGoldenOpensOnMain(t *testing.T) {
	t.Parallel()
	path := golden(t)
	s, err := store.Open(path)
	must(t, err)
	defer s.Close()
	if version := queryInt(t, raw(t, path), "PRAGMA user_version"); version != store.ReleaseVersion() {
		t.Fatalf("user_version = %d, want %d", version, store.ReleaseVersion())
	}
	if store.ReleaseVersion() == store.BaseVersion {
		if s.Upgraded() != nil || len(backups(t, path)) != 0 {
			t.Fatalf("release has no migrations, but an upgrade ran: %+v %v", s.Upgraded(), backups(t, path))
		}
	} else {
		upgraded := s.Upgraded()
		if upgraded == nil || upgraded.From != store.BaseVersion || upgraded.To != store.ReleaseVersion() {
			t.Fatalf("upgrade = %+v", upgraded)
		}
		if len(backups(t, path)) != 1 {
			t.Fatalf("backups = %v", backups(t, path))
		}
	}
	tasks, err := store.List[model.Task](s, "task")
	must(t, err)
	if len(tasks) != 3 {
		t.Fatalf("%d tasks", len(tasks))
	}
	byID := map[string]model.Task{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	first, ok := byID[goldenFirstTask]
	if !ok || first.Status != model.StatusPublished || first.Lifecycle.ArchivedAt == nil || len(first.Proposal.Dependencies) != 0 || first.PRNumber == nil || *first.PRNumber != 42 {
		t.Fatalf("first task: %+v", first)
	}
	second, ok := byID[goldenSecondTask]
	if !ok || second.Status != model.StatusPublished || second.Lifecycle.ArchivedAt != nil || len(second.Proposal.Dependencies) != 1 || second.Proposal.Dependencies[0] != goldenFirstTask {
		t.Fatalf("second task: %+v", second)
	}
	third, ok := byID[goldenThirdTask]
	if !ok || third.Status != model.StatusPublished || len(third.Proposal.Dependencies) != 1 || third.Proposal.Dependencies[0] != goldenSecondTask {
		t.Fatalf("third task: %+v", third)
	}
	cycles, err := store.List[model.Cycle](s, "cycle")
	must(t, err)
	if len(cycles) != 2 {
		t.Fatalf("%d cycles", len(cycles))
	}
	for _, cycle := range cycles {
		if cycle.Mode != model.CycleModeExecution {
			t.Fatalf("cycle %s mode %s", cycle.ID, cycle.Mode)
		}
	}
	byCycle := map[string]model.Cycle{}
	for _, cycle := range cycles {
		byCycle[cycle.ID] = cycle
	}
	if byCycle[goldenCompletedCycle].Status != model.CycleCompleted || byCycle[goldenCompletedCycle].Number != 1 {
		t.Fatalf("completed cycle: %+v", byCycle[goldenCompletedCycle])
	}
	if byCycle[goldenFailedCycle].Status != model.CycleFailed || byCycle[goldenFailedCycle].Number != 2 || byCycle[goldenFailedCycle].Error == nil {
		t.Fatalf("failed cycle: %+v", byCycle[goldenFailedCycle])
	}
	prs, err := store.List[model.PRObservation](s, "pr")
	must(t, err)
	if len(prs) != 1 || prs[0].PR.Number != 42 || prs[0].PR.State != "open" || !prs[0].PR.Owned {
		t.Fatalf("prs: %+v", prs)
	}
	decisions, err := store.List[model.DecisionRecord](s, "decision")
	must(t, err)
	if len(decisions) != 3 {
		t.Fatalf("%d decisions", len(decisions))
	}
	rdb := raw(t, path)
	if n := queryInt(t, rdb, "SELECT count FROM record_counts WHERE kind='task' AND status='published' AND archived=0"); n != 2 {
		t.Fatalf("unarchived published count %d", n)
	}
	if n := queryInt(t, rdb, "SELECT count FROM record_counts WHERE kind='task' AND status='published' AND archived=1"); n != 1 {
		t.Fatalf("archived published count %d", n)
	}
	if n := queryInt(t, rdb, "SELECT COALESCE(sum(count),0) FROM record_counts WHERE kind='cycle'"); n != 2 {
		t.Fatalf("cycle count %d", n)
	}
	r, err := store.OpenReadOnly(path, "golden test")
	must(t, err)
	must(t, r.Close())
}

func TestReleasedGoldensOpenOnMain(t *testing.T) {
	paths, err := filepath.Glob("testdata/state-v*.db")
	must(t, err)
	if len(paths) == 0 {
		t.Fatal("no released golden databases")
	}
	for _, source := range paths {
		t.Run(filepath.Base(source), func(t *testing.T) {
			t.Parallel()
			version := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(source), "state-v"), ".db")
			path := filepath.Join(t.TempDir(), "state.db")
			testutil.GoldenState(t, version, path)
			s, err := store.Open(path)
			must(t, err)
			defer s.Close()
			if got := queryInt(t, raw(t, path), "PRAGMA user_version"); got != store.ReleaseVersion() {
				t.Fatalf("schema version %d, want %d", got, store.ReleaseVersion())
			}
			_, err = store.List[model.Task](s, "task")
			must(t, err)
			_, err = store.List[model.Cycle](s, "cycle")
			must(t, err)
			_, err = store.List[model.PRObservation](s, "pr")
			must(t, err)
			_, err = store.List[model.DecisionRecord](s, "decision")
			must(t, err)
			_, err = store.Get[config.Config](s, "settings", "config")
			must(t, err)
			_, err = store.Get[model.Control](s, "settings", "control")
			must(t, err)
		})
	}
}

func TestFreshSchemaMatchesOpenedGolden(t *testing.T) {
	t.Parallel()
	path := golden(t)
	s, err := store.Open(path)
	must(t, err)
	must(t, s.Close())
	fresh := statePath(t)
	f, err := store.Open(fresh)
	must(t, err)
	must(t, f.Close())
	if diff, ok := equalStrings(master(t, raw(t, path)), master(t, raw(t, fresh))); !ok {
		t.Fatalf("golden's schema after Open differs from a fresh database: %s", diff)
	}
	if v := queryInt(t, raw(t, path), "PRAGMA user_version"); v != store.ReleaseVersion() {
		t.Fatalf("golden at %d, release at %d", v, store.ReleaseVersion())
	}
}

func TestMigrationUpgradesGolden(t *testing.T) {
	t.Parallel()
	path := golden(t)
	pristine := golden(t)
	ddl, migrations := v8Plan(t)
	s, err := store.OpenPlan(path, ddl, migrations...)
	must(t, err)
	defer s.Close()
	upgraded := s.Upgraded()
	if upgraded == nil || upgraded.From != 7 || upgraded.To != 8 {
		t.Fatalf("upgrade = %+v", upgraded)
	}
	backup := upgraded.Backup
	if filepath.Dir(backup) != filepath.Dir(path) {
		t.Fatalf("backup not beside the database: %s", backup)
	}
	if !regexp.MustCompile(`^state\.db\.v7-backup-\d{8}T\d{6}Z$`).MatchString(filepath.Base(backup)) {
		t.Fatalf("backup name: %s", backup)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode: %v %v", info.Mode(), err)
	}
	bdb := raw(t, backup)
	if v := queryInt(t, bdb, "PRAGMA user_version"); v != 7 {
		t.Fatalf("backup at version %d", v)
	}
	var journalMode string
	must(t, bdb.QueryRow("PRAGMA journal_mode").Scan(&journalMode))
	if journalMode != "delete" {
		t.Fatalf("backup journal mode %s", journalMode)
	}
	rows, err := bdb.Query("PRAGMA integrity_check")
	must(t, err)
	found := 0
	for rows.Next() {
		var line string
		must(t, rows.Scan(&line))
		if line != "ok" {
			t.Fatalf("integrity: %s", line)
		}
		found++
	}
	must(t, rows.Err())
	rows.Close()
	if found != 1 {
		t.Fatalf("%d integrity rows", found)
	}
	if diff, ok := equalStrings(recordDump(t, bdb), recordDump(t, raw(t, pristine))); !ok {
		t.Fatalf("backup records differ from the golden: %s", diff)
	}

	if v := queryInt(t, raw(t, path), "PRAGMA user_version"); v != 8 {
		t.Fatalf("migrated database at version %d", v)
	}
	if n := queryInt(t, raw(t, path), "SELECT count(*) FROM migration_probe"); n != 3 {
		t.Fatalf("%d probe rows", n)
	}
	if n := queryInt(t, raw(t, path), "SELECT count(*) FROM records WHERE kind='task' AND json_extract(data,'$.probe_note')='v8'"); n != 3 {
		t.Fatalf("%d rewritten task records", n)
	}
	tasks, err := store.List[model.Task](s, "task")
	must(t, err)
	if len(tasks) != 3 {
		t.Fatalf("%d tasks after the record rewrite", len(tasks))
	}
	entity := "system"
	events, err := s.Events(&entity)
	must(t, err)
	upgrade := 0
	for _, event := range events {
		if event.Kind == "upgrade" {
			upgrade++
			if !strings.Contains(event.Message, "version 7 to 8") {
				t.Fatalf("upgrade event: %+v", event)
			}
		}
	}
	if upgrade != 1 {
		t.Fatalf("events: %+v", events)
	}
	fresh := statePath(t)
	fs, err := store.OpenPlan(fresh, ddl, migrations...)
	must(t, err)
	must(t, fs.Close())
	if diff, ok := equalStrings(master(t, raw(t, path)), master(t, raw(t, fresh))); !ok {
		t.Fatalf("migrated schema differs from fresh v8 DDL: %s", diff)
	}

	must(t, s.Close())
	reopened, err := store.OpenPlan(path, ddl, migrations...)
	must(t, err)
	if reopened.Upgraded() != nil || len(backups(t, path)) != 1 {
		t.Fatalf("reopen ran an upgrade: %+v %v", reopened.Upgraded(), backups(t, path))
	}
	must(t, reopened.Close())

	before, err := os.ReadFile(path)
	must(t, err)
	if _, err := store.OpenPlan(path, ddl); err == nil || !strings.Contains(err.Error(), "is newer than this release's version 7") {
		t.Fatalf("v7 plan open of v8: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || string(before) != string(after) {
		t.Fatal("refused v8 database changed")
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	t.Parallel()
	path := golden(t)
	ddl, good := v8Plan(t)
	failing := store.NewMigration("v8-probe-fails", func(ctx context.Context, c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, "CREATE TABLE migration_probe(task_id TEXT PRIMARY KEY, status TEXT NOT NULL)"); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "INSERT INTO migration_probe(task_id,status) SELECT id,json_extract(data,'$.status') FROM records WHERE kind='task'"); err != nil {
			return err
		}
		if _, err := c.ExecContext(ctx, "CREATE INDEX migration_probe_status ON migration_probe(status)"); err != nil {
			return err
		}
		return errors.New("injected migration failure")
	})
	beforeMaster := master(t, raw(t, path))
	beforeRecords := recordDump(t, raw(t, path))
	_, err := store.OpenPlan(path, ddl, failing)
	if err == nil || !strings.Contains(err.Error(), "remains at version 7") {
		t.Fatalf("upgrade error: %v", err)
	}
	saved := backups(t, path)
	if len(saved) != 1 || !strings.Contains(err.Error(), saved[0]) {
		t.Fatalf("backup %v not named in %v", saved, err)
	}
	if v := queryInt(t, raw(t, path), "PRAGMA user_version"); v != 7 {
		t.Fatalf("database at version %d", v)
	}
	if diff, ok := equalStrings(master(t, raw(t, path)), beforeMaster); !ok {
		t.Fatalf("rolled-back schema changed: %s", diff)
	}
	if diff, ok := equalStrings(recordDump(t, raw(t, path)), beforeRecords); !ok {
		t.Fatalf("rolled-back records changed: %s", diff)
	}
	s, err := store.OpenPlan(path, v7GoldenDDL(t))
	must(t, err)
	if tasks, err := store.List[model.Task](s, "task"); err != nil || len(tasks) != 3 {
		t.Fatalf("%v %v", tasks, err)
	}
	must(t, s.Close())
	retry, err := store.OpenPlan(path, ddl, good...)
	must(t, err)
	if up := retry.Upgraded(); up == nil || up.From != 7 || up.To != 8 {
		t.Fatalf("retry upgrade = %+v", up)
	}
	must(t, retry.Close())
	if after := backups(t, path); len(after) != 2 || after[0] == after[1] {
		t.Fatalf("backups after retry: %v", after)
	}
}

func TestMigrationsRunInOrder(t *testing.T) {
	t.Parallel()
	ddl8, migrations8 := v8Plan(t)
	ddl9 := ddl8 + "\nCREATE TABLE migration_probe_v9(marker TEXT);\n"
	seen := []int64{}
	observer := func(name string) store.Migration {
		return store.NewMigration(name, func(ctx context.Context, c *sql.Conn) error {
			var v int64
			if err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
				return err
			}
			seen = append(seen, v)
			return nil
		})
	}
	migrations9 := []store.Migration{observer("v8"), observer("v9")}

	path := golden(t)
	s, err := store.OpenPlan(path, ddl9, migrations9...)
	must(t, err)
	must(t, s.Close())
	if len(seen) != 2 || seen[0] != 7 || seen[1] != 8 {
		t.Fatalf("observed versions %v", seen)
	}
	if v := queryInt(t, raw(t, path), "PRAGMA user_version"); v != 9 {
		t.Fatalf("database at version %d", v)
	}
	saved := backups(t, path)
	if len(saved) != 1 || !regexp.MustCompile(`^state\.db\.v7-backup-\d{8}T\d{6}Z$`).MatchString(filepath.Base(saved[0])) {
		t.Fatalf("backups: %v", saved)
	}

	seen = nil
	partial := golden(t)
	s, err = store.OpenPlan(partial, ddl8, migrations8...)
	must(t, err)
	must(t, s.Close())
	s, err = store.OpenPlan(partial, ddl9, migrations9...)
	must(t, err)
	must(t, s.Close())
	if len(seen) != 1 || seen[0] != 8 {
		t.Fatalf("observed versions from v8: %v", seen)
	}
	saved = backups(t, partial)
	if len(saved) != 2 {
		t.Fatalf("backups: %v", saved)
	}
	for _, pattern := range []string{`^state\.db\.v7-backup-\d{8}T\d{6}Z`, `^state\.db\.v8-backup-\d{8}T\d{6}Z`} {
		matched := false
		for _, name := range saved {
			if regexp.MustCompile(pattern).MatchString(filepath.Base(name)) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("no backup matching %s in %v", pattern, saved)
		}
	}

	stuck := golden(t)
	_, err = store.OpenPlan(stuck, ddl9, migrations8[0], store.NewMigration("v9-fails", func(context.Context, *sql.Conn) error {
		return errors.New("injected v9 failure")
	}))
	if err == nil || !strings.Contains(err.Error(), "remains at version 8") {
		t.Fatalf("v9 failure: %v", err)
	}
	if v := queryInt(t, raw(t, stuck), "PRAGMA user_version"); v != 8 {
		t.Fatalf("database at version %d", v)
	}
}

func TestBackupFailureSkipsMigration(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores a read-only directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	testutil.GoldenState(t, "0.1.0", path)
	// A WAL database needs its shm/wal side files even for reads; pre-create them
	// writable so the read-only directory fails only the backup's temp file.
	must(t, os.WriteFile(path+"-shm", nil, 0o600))
	must(t, os.WriteFile(path+"-wal", nil, 0o600))
	must(t, os.Chmod(dir, 0o500))
	called := false
	ddl, _ := v8Plan(t)
	_, err := store.OpenPlan(path, ddl, store.NewMigration("v8-probe", func(context.Context, *sql.Conn) error {
		called = true
		return nil
	}))
	must(t, os.Chmod(dir, 0o700))
	if err == nil || !strings.Contains(err.Error(), "it was not changed") {
		t.Fatalf("backup failure: %v", err)
	}
	if called {
		t.Fatal("migration ran without a backup")
	}
	if v := queryInt(t, raw(t, path), "PRAGMA user_version"); v != 7 {
		t.Fatalf("database at version %d", v)
	}
	if len(backups(t, path)) != 0 {
		t.Fatalf("backups: %v", backups(t, path))
	}
}
