package store

import (
	"database/sql"
	"errors"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

var ErrRequestLimit = errors.New("request limit reached")

var ErrRequestChanged = errors.New("Operator request changed; reload requests and try again")

const requestRepository = "json_extract(data,'$.repository')=?1 COLLATE NOCASE"
const requestPending = "json_extract(data,'$.status')='pending'"

// CreateRequest admits one pending request under the per-repository bound in the same transaction that counts.
func (s *Store) CreateRequest(r model.OperatorRequest, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		var count int64
		if err := c.QueryRowContext(background, "SELECT count(*) FROM records WHERE kind='request' AND "+requestRepository+" AND "+requestPending, r.Repository).Scan(&count); err != nil {
			return err
		}
		if count >= int64(limit) {
			return ErrRequestLimit
		}
		return txPut(c, Requests.Key(r.ID), r)
	})
}

// PendingRequests lists a repository's pending requests oldest first; rowid is stable because txPut updates in place.
func (s *Store) PendingRequests(repository string, limit int) ([]model.OperatorRequest, error) {
	return listRecords[model.OperatorRequest](s, "SELECT data FROM records WHERE kind='request' AND "+requestRepository+" AND "+requestPending+" ORDER BY rowid ASC LIMIT ?2", repository, limit)
}

// RecentRequests lists decided requests newest first.
func (s *Store) RecentRequests(repository string, limit int) ([]model.OperatorRequest, error) {
	return listRecords[model.OperatorRequest](s, "SELECT data FROM records WHERE kind='request' AND "+requestRepository+" AND json_extract(data,'$.status')!='pending' ORDER BY json_extract(data,'$.updated_at') DESC, rowid DESC LIMIT ?2", repository, limit)
}

func (s *Store) CountPendingRequests(repository string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	err := s.conn.QueryRowContext(background, "SELECT count(*) FROM records WHERE kind='request' AND "+requestRepository+" AND "+requestPending, repository).Scan(&count)
	return count, err
}

// RequestUpdate replaces a request record only when the saved record still equals Expected.
type RequestUpdate struct {
	Expected, Next model.OperatorRequest
}

func txUpdateRequest(c *sql.Conn, u RequestUpdate) error {
	var current model.OperatorRequest
	found, err := txGet(c, Requests.Key(u.Expected.ID), &current)
	if err != nil {
		return err
	}
	if !found || !wirejson.Equal(current, u.Expected) {
		return ErrRequestChanged
	}
	return txPut(c, Requests.Key(u.Next.ID), u.Next)
}

func (s *Store) UpdateRequest(u RequestUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(true, func(c *sql.Conn) error {
		return txUpdateRequest(c, u)
	})
}
