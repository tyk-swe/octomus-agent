package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
)

const schemaVersion = 7

// schema.sql is the complete fresh DDL, triggers included; it is applied once to an empty database.
//
//go:embed schema.sql
var schemaSQL string

func schemaStatus(ctx context.Context, c *sql.Conn) (bool, error) {
	version, err := userVersion(ctx, c)
	if err != nil {
		return false, err
	}
	if version == schemaVersion {
		return false, nil
	}
	if version == 0 {
		var objects int
		err = c.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&objects)
		if err != nil {
			return false, err
		}
		if objects == 0 {
			return true, nil
		}
	}
	return false, unsupportedSchema(version)
}

func requireSchema(ctx context.Context, c *sql.Conn) error {
	version, err := userVersion(ctx, c)
	if err != nil {
		return err
	}
	if version != schemaVersion {
		return unsupportedSchema(version)
	}
	return nil
}

func userVersion(ctx context.Context, c *sql.Conn) (int64, error) {
	var version int64
	err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	return version, err
}

func unsupportedSchema(version int64) error {
	return fmt.Errorf("State database schema version %d is unsupported; this release requires a fresh version-%d data directory. Back up existing state before changing data directories", version, schemaVersion)
}

func createSchema(ctx context.Context, c *sql.Conn) error {
	return runTx(c, "BEGIN IMMEDIATE", func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx, schemaSQL); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", schemaVersion))
		return err
	})
}
