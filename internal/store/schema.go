package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
)

// baseVersion is the schema v0.1.0 shipped, the oldest one this binary upgrades.
const baseVersion = 7

// A migration upgrades the schema by exactly one version inside the runner's BEGIN IMMEDIATE
// transaction, which also sets user_version; it must not begin, commit or change the journal mode.
type migration struct {
	name  string
	apply func(ctx context.Context, c *sql.Conn) error
}

// releaseMigrations[i] upgrades baseVersion+i to baseVersion+i+1. Append only; schema.sql is the result.
var releaseMigrations = []migration{
	{name: "v8-notification-events", apply: func(ctx context.Context, c *sql.Conn) error {
		_, err := c.ExecContext(ctx, notificationEventsV8)
		return err
	}},
	{name: "v9-decision-identity", apply: func(ctx context.Context, c *sql.Conn) error {
		_, err := c.ExecContext(ctx, decisionIdentityV9)
		return err
	}},
}

//go:embed migrations/008-notifications.sql
var notificationEventsV8 string

//go:embed migrations/009-decision-identity.sql
var decisionIdentityV9 string

type schemaPlan struct {
	ddl        string
	migrations []migration
}

func (p schemaPlan) version() int64 { return baseVersion + int64(len(p.migrations)) }

var release = schemaPlan{ddl: schemaSQL, migrations: releaseMigrations}

// schema.sql is the complete fresh DDL, triggers included; it is applied once to an empty database.
//
//go:embed schema.sql
var schemaSQL string

// inspect reads only: it reports the saved version, whether the database is empty enough to
// create, or why neither opening nor upgrading it is this release's job.
func inspect(ctx context.Context, c *sql.Conn, plan schemaPlan) (version int64, fresh bool, err error) {
	version, err = userVersion(ctx, c)
	if err != nil {
		return 0, false, err
	}
	switch {
	case version == plan.version():
		return version, false, nil
	case version > plan.version():
		return 0, false, newerSchema(version, plan.version())
	case version >= baseVersion:
		return version, false, nil
	case version == 0:
		var objects int
		err = c.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&objects)
		if err != nil {
			return 0, false, err
		}
		if objects == 0 {
			return 0, true, nil
		}
	}
	return 0, false, unsupportedSchema(version)
}

// requireSchema is the read-only check: it accepts only the release's own version and never
// takes the upgrade path, so a read-only open leaves every byte untouched.
func requireSchema(ctx context.Context, c *sql.Conn, plan schemaPlan, what string) error {
	version, err := userVersion(ctx, c)
	if err != nil {
		return err
	}
	switch {
	case version == plan.version():
		return nil
	case version > plan.version():
		return newerSchema(version, plan.version())
	case version >= baseVersion:
		return fmt.Errorf("State database schema version %d must be upgraded to version %d before a read-only %s; start the service once to back it up and upgrade it, or use the release that wrote it", version, plan.version(), what)
	default:
		return unsupportedSchema(version)
	}
}

func userVersion(ctx context.Context, c *sql.Conn) (int64, error) {
	var version int64
	err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	return version, err
}

func unsupportedSchema(version int64) error {
	return fmt.Errorf("State database schema version %d is unsupported; this release upgrades version-%d and later state and otherwise requires a fresh data directory. Back up existing state before changing data directories", version, baseVersion)
}

func newerSchema(version, latest int64) error {
	return fmt.Errorf("State database schema version %d is newer than this release's version %d; run the release that wrote it, or restore the backup taken before its upgrade", version, latest)
}

func createSchema(ctx context.Context, c *sql.Conn, plan schemaPlan) error {
	return runTx(c, "BEGIN IMMEDIATE", func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, plan.ddl); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", plan.version()))
		return err
	})
}

// Upgrade records the migrations Open applied.
type Upgrade struct {
	From, To int64
	Backup   string
}

// migrate runs each pending migration in its own transaction after one consistent backup.
// A failed step rolls back alone: the database stays at the version the step started from.
// The last step's transaction also records the upgrade event, so a migrated database
// always carries it and Open can never fail after the final commit.
func (s *Store) migrate(plan schemaPlan, from int64) error {
	backup, err := backupState(s.conn, s.path, from)
	if err != nil {
		return fmt.Errorf("Cannot back up the version-%d state database before upgrading it; it was not changed: %w", from, err)
	}
	latest := plan.version()
	for v := from; v < latest; v++ {
		step := plan.migrations[v-baseVersion]
		err := runTx(s.conn, "BEGIN IMMEDIATE", func(c *sql.Conn) error {
			current, err := userVersion(background, c)
			if err != nil {
				return err
			}
			if current != v {
				return fmt.Errorf("schema version changed to %d during the upgrade", current)
			}
			if err := step.apply(background, c); err != nil {
				return err
			}
			if _, err := c.ExecContext(background, fmt.Sprintf("PRAGMA user_version=%d", v+1)); err != nil {
				return err
			}
			if v+1 == latest {
				return txEvent(c, "system", "upgrade", fmt.Sprintf("Upgraded state from schema version %d to %d; the pre-upgrade backup %s is kept until the operator removes it", from, latest, filepath.Base(backup)))
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("State database upgrade from version %d to %d (%s) failed and was rolled back; the database remains at version %d and the pre-upgrade backup %s is kept: %w", v, v+1, step.name, v, backup, err)
		}
	}
	s.upgraded = &Upgrade{From: from, To: latest, Backup: backup}
	return nil
}

// backupState writes a consistent copy of the database beside it through the SQLite backup
// API, verifies the copy, then publishes it under a name that never overwrites an older backup.
func backupState(conn *sql.Conn, path string, version int64) (string, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".backup-partial-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	fail := func(err error) (string, error) {
		tmp.Close()
		for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
			os.Remove(tmpPath + suffix)
		}
		return "", err
	}
	var backup *sqlite.Backup
	err = conn.Raw(func(driverConn any) error {
		src, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("the SQLite driver cannot produce an online backup")
		}
		var err error
		backup, err = src.NewBackup(dsn(tmpPath, "_pragma=busy_timeout(5000)"))
		return err
	})
	if err != nil {
		return fail(err)
	}
	for {
		more, err := backup.Step(-1)
		if err != nil {
			backup.Finish()
			return fail(err)
		}
		if !more {
			break
		}
	}
	if err := backup.Finish(); err != nil {
		return fail(err)
	}
	if err := verifyBackup(tmpPath, version); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	var final string
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 100 && final == ""; i++ {
		candidate := fmt.Sprintf("%s.v%d-backup-%s", path, version, stamp)
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", candidate, i+1)
		}
		if err := os.Link(tmpPath, candidate); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fail(err)
		}
		final = candidate
	}
	if final == "" {
		return fail(errors.New("no free backup filename"))
	}
	os.Remove(tmpPath)
	dirFile, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	err = dirFile.Sync()
	dirFile.Close()
	if err != nil {
		return "", err
	}
	return final, nil
}

// verifyBackup checks the copy: a self-contained delete-journal file that passes an
// integrity check and carries the schema version it was taken from.
func verifyBackup(path string, version int64) error {
	check, err := sql.Open("sqlite", dsn(path, "_pragma=busy_timeout(5000)"))
	if err != nil {
		return err
	}
	defer check.Close()
	var mode string
	// The copy carries the source's WAL header; switch it to a self-contained delete journal.
	if err := check.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		return err
	}
	if mode != "delete" {
		return fmt.Errorf("backup journal mode is %s", mode)
	}
	rows, err := check.Query("PRAGMA integrity_check")
	if err != nil {
		return err
	}
	integrity := []string{}
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			rows.Close()
			return err
		}
		integrity = append(integrity, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(integrity) != 1 || integrity[0] != "ok" {
		return fmt.Errorf("backup integrity check reported %v", integrity)
	}
	var saved int64
	if err := check.QueryRow("PRAGMA user_version").Scan(&saved); err != nil {
		return err
	}
	if saved != version {
		return fmt.Errorf("backup is at schema version %d, not %d", saved, version)
	}
	return nil
}
