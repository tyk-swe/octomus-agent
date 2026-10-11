package store

import (
	"context"
	"database/sql"
)

// Test hooks into the migration runner; they exist only in test binaries.

type Migration = migration

func NewMigration(name string, apply func(context.Context, *sql.Conn) error) Migration {
	return migration{name: name, apply: apply}
}

func OpenPlan(path, ddl string, migrations ...Migration) (*Store, error) {
	return open(path, schemaPlan{ddl: ddl, migrations: migrations})
}

func OpenReadOnlyPlan(path, what, ddl string, migrations ...Migration) (*ReadOnly, error) {
	return openReadOnly(path, what, schemaPlan{ddl: ddl, migrations: migrations})
}

func SchemaDDL() string { return schemaSQL }

const BaseVersion = baseVersion

func ReleaseVersion() int64 { return release.version() }

// Raw connection hooks support storage failure injection and connection checks
// without making SQL or mutable connections part of the production read API.
func (s *Store) RawSnapshot(fn func(*sql.Conn) error) error    { return s.snapshot(fn) }
func (r *ReadOnly) RawSnapshot(fn func(*sql.Conn) error) error { return r.snapshot(fn) }
func (r *ReadOnly) RawConn() *sql.Conn                         { return r.conn }
