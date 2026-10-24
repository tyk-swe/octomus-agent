package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/store"
)

// notifyWorker drains one chat destination's slice of the durable
// attention outbox with the same pacing and retry rules as the webhook.
type notifyWorker struct {
	svc      *Service
	st       *store.Store
	dest     store.ChatDestination
	post     func(context.Context, Provider, string, Response) error
	provider Provider
}

func (w *notifyWorker) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		w.deliverNext(ctx)
		timer.Reset(time.Second)
	}
}

func (w *notifyWorker) deliverNext(ctx context.Context) {
	delivery, err := w.st.ClaimDelivery(w.dest.ID, w.svc.now())
	if err != nil {
		w.svc.warn(err)
		return
	}
	if delivery == nil {
		return
	}
	resp := w.svc.alertResponse(delivery)
	err = w.post(ctx, w.provider, w.dest.Target, resp)
	if err == nil {
		w.svc.warn(w.st.MarkDelivered(delivery.Seq, w.svc.now()))
		return
	}
	// The delivery error itself is the operator's only diagnostic for a
	// broken chat destination (webhook health excludes chat rows), so
	// log it instead of only warning about the bookkeeping write.
	w.svc.warn(err)
	if ctx.Err() != nil {
		// Uncertain send during shutdown: leave the claim to the store's
		// attempt-limit recovery, like the webhook worker does.
		return
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) && (apiErr.Status > 0 || apiErr.Code != "") {
		// Slack reports refusals with HTTP 200 plus an error code; only a
		// real HTTP failure status belongs in the health record.
		var status *uint16
		if apiErr.Code == "" && apiErr.Status > 0 {
			s := uint16(apiErr.Status)
			status = &s
		}
		w.svc.warn(w.st.MarkFailed(delivery.Seq, "http_status", status, apiErr.retryable()))
		return
	}
	w.svc.warn(w.st.MarkFailed(delivery.Seq, "transport_error", nil, true))
}

// alertResponse renders one attention event, enriched best-effort from
// engine reads; the stored outbox row itself stays identifier-only.
func (s *Service) alertResponse(d *store.NotificationDelivery) Response {
	repo := d.Repository
	if repo == "" {
		repo = "Octomus"
	}
	var text string
	var buttons []Button
	switch d.Category {
	case "task_published":
		text = fmt.Sprintf("✅ %s: task published", repo)
	case "cycle_failed":
		text = fmt.Sprintf("⚠️ %s: planning cycle failed", repo)
	case "audit_completed":
		text = fmt.Sprintf("🔎 %s: audit completed", repo)
	case "service_error_paused":
		text = fmt.Sprintf("⏸️ %s: service paused on error — inspect the dashboard", repo)
	default:
		text = fmt.Sprintf("⚠️ %s: task needs attention (%s)", repo, d.Category)
	}
	if d.TaskID != nil && *d.TaskID != "" {
		id := *d.TaskID
		if app := s.fleet.For(d.Repository); app != nil {
			if detail, err := app.TaskDetail(id); err == nil {
				text += "\n" + detail.Proposal.Title
				if detail.PRURL != nil && *detail.PRURL != "" {
					text += "\n" + *detail.PRURL
				}
			}
		}
		buttons = append(buttons, Button{Label: "View task", Value: taskActionValue("view", id)})
		if d.Category != "task_published" {
			buttons = append(buttons, Button{Label: "Retry task", Value: taskActionValue("retry", id)})
		}
	}
	return Response{Text: boundText(text), Buttons: buttons}
}
