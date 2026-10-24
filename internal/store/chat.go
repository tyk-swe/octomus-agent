package store

import (
	"database/sql"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// ChatDestination is one proactive-notification sink beyond the generic
// webhook: a Slack or Discord channel the attention outbox fans out to.
type ChatDestination struct {
	ID     string
	Kind   string // "slack" or "discord"
	Target string // provider channel ID
}

// SetChatDestinations replaces the chat (non-webhook) notification
// destinations. Pending deliveries to destinations that disappear are
// cancelled, mirroring webhook rotation; webhook rows are untouched.
func (s *Store) SetChatDestinations(destinations []ChatDestination) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transaction(false, func(c *sql.Conn) error {
		rows, err := c.QueryContext(background, "SELECT destination_id FROM notification_destinations WHERE kind IN ('slack','discord')")
		if err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, d := range destinations {
			keep[d.ID] = true
		}
		var removed []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if !keep[id] {
				removed = append(removed, id)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for _, id := range removed {
			if _, err := c.ExecContext(background, "UPDATE notification_outbox SET status='cancelled', last_error='destination_changed' WHERE status='pending' AND destination_id=?1", id); err != nil {
				return err
			}
			if _, err := c.ExecContext(background, "DELETE FROM notification_destinations WHERE destination_id=?1", id); err != nil {
				return err
			}
		}
		for _, d := range destinations {
			if _, err := c.ExecContext(background, `INSERT INTO notification_destinations(destination_id,kind,target,enabled) VALUES (?1,?2,?3,1)
                 ON CONFLICT(destination_id) DO UPDATE SET kind=excluded.kind, target=excluded.target, enabled=1`, d.ID, d.Kind, d.Target); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkChatEventSeen records a provider event ID and reports whether it was
// already recorded. Chat adapters use it so a redelivered command (provider
// retry, gateway resume) never executes twice. Records older than the
// retention window are pruned opportunistically on insert.
func (s *Store) MarkChatEventSeen(provider, eventID string, now time.Time) (seen bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.transaction(false, func(c *sql.Conn) error {
		cutoff := model.Timestamp(now.Add(-7 * 24 * time.Hour))
		if _, err := c.ExecContext(background, "DELETE FROM chat_events WHERE received_at < ?1", cutoff); err != nil {
			return err
		}
		result, err := c.ExecContext(background, "INSERT OR IGNORE INTO chat_events(provider,event_id,received_at) VALUES (?1,?2,?3)", provider, eventID, model.Timestamp(now))
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		seen = inserted == 0
		return nil
	})
	return seen, err
}

// ChatContext is the durable conversational memory for one chat thread: the
// repository and task last under discussion there.
type ChatContext struct {
	Repository string
	TaskID     string
}

func (s *Store) ChatContext(provider, channelID, threadID string) (ChatContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out ChatContext
	err := s.conn.QueryRowContext(background, "SELECT repository,task_id FROM chat_context WHERE provider=?1 AND channel_id=?2 AND thread_id=?3", provider, channelID, threadID).Scan(&out.Repository, &out.TaskID)
	if err == sql.ErrNoRows {
		return ChatContext{}, nil
	}
	return out, err
}

func (s *Store) SetChatContext(provider, channelID, threadID string, value ChatContext, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.ExecContext(background, `INSERT INTO chat_context(provider,channel_id,thread_id,repository,task_id,updated_at) VALUES (?1,?2,?3,?4,?5,?6)
         ON CONFLICT(provider,channel_id,thread_id) DO UPDATE SET repository=excluded.repository, task_id=excluded.task_id, updated_at=excluded.updated_at`,
		provider, channelID, threadID, value.Repository, value.TaskID, model.Timestamp(now))
	return err
}
