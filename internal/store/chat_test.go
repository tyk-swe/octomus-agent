// Chat destinations, event dedupe and thread context.

package store_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

func pendingDestinations(t *testing.T, path string) map[string]int {
	t.Helper()
	db := raw(t, path)
	rows, err := db.Query("SELECT destination_id, COUNT(*) FROM notification_outbox WHERE status='pending' GROUP BY destination_id")
	must(t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		must(t, rows.Scan(&id, &n))
		out[id] = n
	}
	must(t, rows.Err())
	return out
}

var slackDest = store.ChatDestination{ID: "slack-dest", Kind: "slack", Target: "C123"}
var discordDest = store.ChatDestination{ID: "discord-dest", Kind: "discord", Target: "456"}

func TestChatDestinationFanout(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	must(t, s.SetNotifyPolicy(str(notifyDest), "enabled", nil))
	must(t, s.SetChatDestinations([]store.ChatDestination{slackDest, discordDest}))
	putNotificationTask(t, s, "fanout-1", "blocked", "timeout")
	got := pendingDestinations(t, path)
	for _, want := range []string{notifyDest, slackDest.ID, discordDest.ID} {
		if got[want] != 1 {
			t.Fatalf("pending by destination = %v; want one row for %q", got, want)
		}
	}
	claimed, err := s.ClaimDelivery(slackDest.ID, time.Now())
	must(t, err)
	if claimed == nil || claimed.Category != "timeout" {
		t.Fatalf("slack claim = %+v; want the blocked-task notice", claimed)
	}
	// Rotating the webhook must not cancel chat destinations' rows.
	must(t, s.SetNotifyPolicy(str("destination-b"), "enabled", nil))
	got = pendingDestinations(t, path)
	if got[discordDest.ID] != 1 {
		t.Fatalf("after webhook rotation pending = %v; want the discord row kept", got)
	}
	if got[notifyDest] != 0 {
		t.Fatalf("after webhook rotation pending = %v; want the old webhook row cancelled", got)
	}
}

func TestClaimCleanupIsolatedPerDestination(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	must(t, s.SetChatDestinations([]store.ChatDestination{slackDest, discordDest}))
	putNotificationTask(t, s, "claim-iso-1", "blocked", "timeout")
	db := raw(t, path)
	// Simulate the slack worker's fifth attempt being in flight.
	var slackSeq int64
	must(t, db.QueryRow("SELECT seq FROM notification_outbox WHERE destination_id=?", slackDest.ID).Scan(&slackSeq))
	_, err := db.Exec("UPDATE notification_outbox SET attempts=5 WHERE seq=?", slackSeq)
	must(t, err)
	claimed, err := s.ClaimDelivery(discordDest.ID, time.Now())
	must(t, err)
	if claimed == nil {
		t.Fatal("discord claim found nothing; want its own pending row")
	}
	var status string
	must(t, db.QueryRow("SELECT status FROM notification_outbox WHERE seq=?", slackSeq).Scan(&status))
	if status != "pending" {
		t.Fatalf("slack row status = %q after another destination claimed; want pending", status)
	}
	must(t, s.MarkDelivered(slackSeq, time.Now()))
	must(t, db.QueryRow("SELECT status FROM notification_outbox WHERE seq=?", slackSeq).Scan(&status))
	if status != "delivered" {
		t.Fatalf("slack row status = %q after MarkDelivered; want delivered", status)
	}
}

func TestNotificationHealthScopesToWebhookDestination(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	must(t, s.SetNotifyPolicy(str(notifyDest), "enabled", nil))
	must(t, s.SetChatDestinations([]store.ChatDestination{slackDest}))
	putNotificationTask(t, s, "health-scope-1", "blocked", "timeout")
	db := raw(t, path)
	var slackSeq, webhookSeq int64
	must(t, db.QueryRow("SELECT seq FROM notification_outbox WHERE destination_id=?", slackDest.ID).Scan(&slackSeq))
	must(t, db.QueryRow("SELECT seq FROM notification_outbox WHERE destination_id=?", notifyDest).Scan(&webhookSeq))
	// A chat success and a webhook failure: the webhook health panel must
	// report only its own destination's outcome.
	must(t, s.MarkDelivered(slackSeq, time.Now()))
	status := uint16(403)
	must(t, s.MarkFailed(webhookSeq, "http_status", &status, false))
	health, err := s.NotificationHealth()
	must(t, err)
	if health.LastDeliveredAt != nil {
		t.Fatalf("webhook health claimed the chat delivery at %v", *health.LastDeliveredAt)
	}
	if health.Failed != 1 || health.Pending != 0 {
		t.Fatalf("health = %+v; want one failed webhook row and none pending", health)
	}
	if health.LastHTTPStatus == nil || *health.LastHTTPStatus != 403 {
		t.Fatalf("health last HTTP status = %v; want the webhook 403", health.LastHTTPStatus)
	}
}

func TestChatDestinationRemovalCancelsPending(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	must(t, s.SetChatDestinations([]store.ChatDestination{slackDest}))
	putNotificationTask(t, s, "removal-1", "failed", "")
	if got := pendingDestinations(t, path); got[slackDest.ID] != 1 {
		t.Fatalf("pending = %v; want one slack row", got)
	}
	must(t, s.SetChatDestinations(nil))
	if got := pendingDestinations(t, path); len(got) != 0 {
		t.Fatalf("pending = %v; want none after removing the destination", got)
	}
	db := raw(t, path)
	var status, lastError string
	must(t, db.QueryRow("SELECT status,last_error FROM notification_outbox").Scan(&status, &lastError))
	if status != "cancelled" || lastError != "destination_changed" {
		t.Fatalf("row = %s/%s; want cancelled/destination_changed", status, lastError)
	}
}

func TestChatEventDedupe(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	now := time.Now()
	seen, err := s.MarkChatEventSeen("slack", "env-1", now)
	must(t, err)
	if seen {
		t.Fatal("first delivery reported as seen")
	}
	seen, err = s.MarkChatEventSeen("slack", "env-1", now)
	must(t, err)
	if !seen {
		t.Fatal("redelivery not reported as seen")
	}
	seen, err = s.MarkChatEventSeen("discord", "env-1", now)
	must(t, err)
	if seen {
		t.Fatal("another provider's delivery reported as seen")
	}
}

func TestChatContextRoundTrip(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	got, err := s.ChatContext("slack", "C1", "T1")
	must(t, err)
	if got != (store.ChatContext{}) {
		t.Fatalf("missing context = %+v; want zero", got)
	}
	want := store.ChatContext{Repository: "acme/widgets", TaskID: "task-7"}
	must(t, s.SetChatContext("slack", "C1", "T1", want, time.Now()))
	got, err = s.ChatContext("slack", "C1", "T1")
	must(t, err)
	if got != want {
		t.Fatalf("context = %+v; want %+v", got, want)
	}
	other, err := s.ChatContext("slack", "C1", "T2")
	must(t, err)
	if other != (store.ChatContext{}) {
		t.Fatalf("other thread context = %+v; want zero", other)
	}
}
