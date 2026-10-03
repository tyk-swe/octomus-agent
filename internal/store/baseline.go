package store

import (
	"database/sql"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// BeginBaseline publishes the new check and its latest-check pointer together.
// A failed admission must not leave a running record without a worker.
func (s *Store) BeginBaseline(check model.BaselineCheck) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(false, func(c *sql.Conn) error {
		if err := txPut(c, "baseline", check.ID, check); err != nil {
			return err
		}
		return txPut(c, "settings", "baseline_latest", check.ID)
	})
}
