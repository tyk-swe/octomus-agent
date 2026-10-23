package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

func Put[T any](s *Store, key Key[T], value T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return txPut(s.conn, key, value)
}

// Get returns the saved record, or nil when none exists.
func Get[T any](s *Store, key Key[T]) (*T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return recordAt(s.conn, key)
}

func List[T any](s *Store, kind Kind[T]) ([]T, error) {
	return listRecords[T](s, "SELECT data FROM records WHERE kind=?1 ORDER BY rowid DESC", kind.name)
}

func recordAt[T any](c *sql.Conn, key Key[T]) (*T, error) {
	var value T
	found, err := txGet(c, key, &value)
	if err != nil || !found {
		return nil, err
	}
	return &value, nil
}

func queryRecords[T any](c *sql.Conn, query string, args ...any) ([]T, error) {
	rows, err := c.QueryContext(background, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []T{}
	var data sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		value, err := decodeRecord[T](data)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// scanAll reads every row into a T through the scan destinations fields returns, then closes rows. The result is
// never nil, so an empty one encodes as [].
func scanAll[T any](rows *sql.Rows, fields func(*T) []any) ([]T, error) {
	defer rows.Close()
	values := []T{}
	for rows.Next() {
		var value T
		if err := rows.Scan(fields(&value)...); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func listRecords[T any](s *Store, query string, args ...any) ([]T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return queryRecords[T](s.conn, query, args...)
}
func txGet[T any](c *sql.Conn, key Key[T], dst *T) (bool, error) {
	var data string
	err := c.QueryRowContext(background, "SELECT data FROM records WHERE kind=?1 AND id=?2", key.kind, key.id).Scan(&data)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := decodeJSON([]byte(data), dst); err != nil {
		return true, fmt.Errorf("Saved %s %s is unreadable: %w", key.kind, key.id, err)
	}
	return true, nil
}

func txPut[T any](c *sql.Conn, key Key[T], value T) error {
	data, err := wirejson.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.ExecContext(background, `INSERT INTO records VALUES (?1,?2,?3)
         ON CONFLICT(kind,id) DO UPDATE SET data=excluded.data`, key.kind, key.id, string(data))
	return err
}
func queryStrings(c *sql.Conn, query string, args ...any) ([]string, error) {
	rows, err := c.QueryContext(background, query, args...)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(value *string) []any { return []any{value} })
}
func decodeJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func decodeRecord[T any](data []byte) (T, error) {
	var value T
	if err := decodeJSON(data, &value); err != nil {
		return value, fmt.Errorf("Saved record %s is unreadable: %w", savedRecordID(data), err)
	}
	return value, nil
}

func savedRecordID(data []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(bytes.NewReader(data)).Decode(&probe) != nil || probe.ID == "" {
		return "(unknown id)"
	}
	return probe.ID
}
