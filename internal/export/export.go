// Package export produces the read-only operator exports of saved records: the
// usage report and the run evidence document. Every export reads one snapshot
// of the state database and passes through the one redaction pass before it
// leaves the process.
package export

import (
	"database/sql"

	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

// export opens the state database read-only for one export, reads a value
// inside one snapshot and returns it redacted. The database is never created
// or changed on the caller's behalf.
func export[T any](stateDB, what string, read func(c *sql.Conn) (T, error)) (map[string]any, error) {
	r, err := store.OpenReadOnly(stateDB, what)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var value T
	err = r.Snapshot(func(c *sql.Conn) (err error) {
		value, err = read(c)
		return err
	})
	if err != nil {
		return nil, err
	}
	return redacted(value)
}

func records[T any](c *sql.Conn, kind string) ([]T, error) {
	return store.QueryRecords[T](c, "SELECT data FROM records WHERE kind=?1 ORDER BY id", kind)
}

// redacted converts an export to its generic JSON document and scrubs every
// string in it; both exports return only values that passed through here.
func redacted(value any) (map[string]any, error) {
	generic, err := wirejson.GenericMap(value)
	if err != nil {
		return nil, err
	}
	redact.JSON(generic)
	return generic, nil
}
