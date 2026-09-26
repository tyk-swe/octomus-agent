package store

import (
	"database/sql"
	"time"
)

// The pending-queue cap (1000 rows) lives in the schema's notify_* triggers.
const (
	NotificationMaxAttempts int64 = 5
	NotificationExpiry      int64 = 24 * 60 * 60
)

// NotificationRetryDelays is indexed by the attempt number just made; its
// length is tied to the attempt limit.
var NotificationRetryDelays = [NotificationMaxAttempts]int64{30, 120, 600, 1800, 1800}

// NotificationDelivery is one claimed outbox row: attention evidence references,
// never task content.
type NotificationDelivery struct {
	Seq        int64   `json:"seq"`
	EventID    string  `json:"event_id"`
	CreatedAt  string  `json:"created_at"`
	Repository string  `json:"repository"`
	CycleID    *string `json:"cycle_id"`
	RunID      *string `json:"run_id"`
	TaskID     *string `json:"task_id"`
	Category   string  `json:"category"`
	Action     string  `json:"action"`
	Attempts   int64   `json:"attempts"`
}

// NotificationHealth is the operator-facing outbox summary.
type NotificationHealth struct {
	State           string  `json:"state"`
	Configured      bool    `json:"configured"`
	Pending         int64   `json:"pending"`
	Failed          int64   `json:"failed"`
	LastDeliveredAt *string `json:"last_delivered_at"`
	LastError       *string `json:"last_error"`
	LastHTTPStatus  *int64  `json:"last_http_status"`
}

// ConfigureNotifications saves the destination policy and cancels pending
// deliveries addressed elsewhere.
func (s *Store) ConfigureNotifications(destination *string, state string, errorText *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(false, func(c *sql.Conn) error {
		enabled := int64(0)
		if state == "enabled" && destination != nil {
			enabled = 1
		}
		if _, err := c.ExecContext(background, `UPDATE notification_outbox SET status='cancelled', last_error='destination_changed'
             WHERE status='pending' AND (?1=0 OR destination_id IS NOT ?2)`, enabled, destination); err != nil {
			return err
		}
		_, err := c.ExecContext(background, `INSERT INTO notification_policy VALUES (1,?2,?1,?3,?4)
             ON CONFLICT(id) DO UPDATE SET destination_id=excluded.destination_id, enabled=excluded.enabled, state=excluded.state, error=excluded.error`, enabled, destination, state, errorText)
		return err
	})
}

// ClaimNotification expires and fails stale rows, then claims the oldest due
// delivery for the enabled destination and schedules its next attempt.
func (s *Store) ClaimNotification(destination string, now time.Time) (*NotificationDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch := now.Unix()
	var claimed *NotificationDelivery
	err := s.transaction(false, func(c *sql.Conn) error {
		if _, err := c.ExecContext(background, "UPDATE notification_outbox SET status='failed', last_error='expired' WHERE status='pending' AND unixepoch(created_at) < ?1", epoch-NotificationExpiry); err != nil {
			return err
		}
		if _, err := c.ExecContext(background, "UPDATE notification_outbox SET status='failed', last_error='delivery_uncertain' WHERE status='pending' AND attempts >= ?1", NotificationMaxAttempts); err != nil {
			return err
		}
		var d NotificationDelivery
		err := c.QueryRowContext(background, `SELECT seq,event_id,created_at,repository,cycle_id,run_id,task_id,category,action,attempts
                 FROM notification_outbox
                 WHERE status='pending' AND next_attempt_at <= ?1 AND destination_id=?2
                     AND destination_id = (SELECT destination_id FROM notification_policy WHERE id=1 AND enabled=1)
                 ORDER BY seq LIMIT 1`, epoch, destination).Scan(&d.Seq, &d.EventID, &d.CreatedAt, &d.Repository, &d.CycleID, &d.RunID, &d.TaskID, &d.Category, &d.Action, &d.Attempts)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			attempt := d.Attempts + 1
			index := attempt - 1
			if index > NotificationMaxAttempts-1 {
				index = NotificationMaxAttempts - 1
			}
			delay := NotificationRetryDelays[index]
			if _, err := c.ExecContext(background, "UPDATE notification_outbox SET attempts=?1, last_attempt_at=?2, next_attempt_at=?3 WHERE seq=?4", attempt, rfc3339(now), epoch+delay, d.Seq); err != nil {
				return err
			}
			d.Attempts = attempt
			claimed = &d
		}
		_, err = c.ExecContext(background, `DELETE FROM notification_outbox WHERE status != 'pending' AND seq NOT IN (
                SELECT seq FROM notification_outbox WHERE status != 'pending' ORDER BY seq DESC
                LIMIT COALESCE(json_extract((SELECT data FROM records WHERE kind='settings' AND id='config'),'$.retain_events'),10000))`)
		return err
	})
	return claimed, err
}

// FinishNotificationDelivered marks a claimed row delivered.
func (s *Store) FinishNotificationDelivered(seq int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.ExecContext(background, "UPDATE notification_outbox SET status='delivered', delivered_at=?1, last_error=NULL, http_status=NULL WHERE seq=?2 AND status='pending'", rfc3339(now), seq)
	return err
}

// FinishNotificationFailure records a failed attempt, keeping the row pending
// only while it is retryable and under the attempt limit.
func (s *Store) FinishNotificationFailure(seq int64, category string, httpStatus *uint16, retryable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	retry := int64(0)
	if retryable {
		retry = 1
	}
	var status *int64
	if httpStatus != nil {
		value := int64(*httpStatus)
		status = &value
	}
	_, err := s.conn.ExecContext(background, "UPDATE notification_outbox SET status=CASE WHEN ?1=1 AND attempts<?2 THEN 'pending' ELSE 'failed' END, last_error=?3, http_status=?4 WHERE seq=?5 AND status='pending'", retry, NotificationMaxAttempts, category, status, seq)
	return err
}

// NotificationHealth summarizes the policy and outbox for the dashboard.
func (s *Store) NotificationHealth() (NotificationHealth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	health := NotificationHealth{State: "disabled"}
	var policyError *string
	err := s.conn.QueryRowContext(background, "SELECT state, state!='disabled', error FROM notification_policy WHERE id=1").Scan(&health.State, &health.Configured, &policyError)
	if err != nil && err != sql.ErrNoRows {
		return health, err
	}
	if err := s.conn.QueryRowContext(background, "SELECT COUNT(*) FROM notification_outbox WHERE status='pending'").Scan(&health.Pending); err != nil {
		return health, err
	}
	if err := s.conn.QueryRowContext(background, "SELECT COUNT(*) FROM notification_outbox WHERE status='failed'").Scan(&health.Failed); err != nil {
		return health, err
	}
	if err := s.conn.QueryRowContext(background, "SELECT MAX(delivered_at) FROM notification_outbox WHERE status='delivered'").Scan(&health.LastDeliveredAt); err != nil {
		return health, err
	}
	var lastError *string
	err = s.conn.QueryRowContext(background, "SELECT last_error, http_status FROM notification_outbox WHERE last_error IS NOT NULL OR http_status IS NOT NULL ORDER BY seq DESC LIMIT 1").Scan(&lastError, &health.LastHTTPStatus)
	if err != nil && err != sql.ErrNoRows {
		return health, err
	}
	health.LastError = policyError
	if health.LastError == nil {
		health.LastError = lastError
	}
	return health, nil
}

// rfc3339 matches chrono's `to_rfc3339` for a UTC instant: seconds precision
// only when the instant has no sub-second part, otherwise nanoseconds.
func rfc3339(at time.Time) string {
	at = at.UTC()
	if at.Nanosecond() == 0 {
		return at.Format("2006-01-02T15:04:05+00:00")
	}
	return at.Format("2006-01-02T15:04:05.000000000+00:00")
}
