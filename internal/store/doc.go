// Package store is the SQLite state: typed records, transactions and the
// bounded read models the dashboard and exports query. It owns the schema
// and its forward-only migrations; every other package reaches saved state
// through it.
package store
