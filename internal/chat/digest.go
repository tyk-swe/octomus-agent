package chat

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Digest builds the fleet-wide 7-day summary. On a schedule it doubles as
// the heartbeat: a missing digest means the service is down.
func (s *Service) Digest(_ context.Context) string {
	apps := s.fleet.Apps()
	if len(apps) == 0 {
		return "Octomus digest: no repositories registered."
	}
	var b strings.Builder
	b.WriteString("Octomus digest (7 days)")
	for _, app := range apps {
		b.WriteString("\n\n")
		view, err := app.StateView()
		if err != nil {
			fmt.Fprintf(&b, "%s — status unavailable: %s", app.Repository(), operatorError(err))
			continue
		}
		mode := view.Control.Mode.String()
		if view.Control.Paused {
			mode = "paused"
		}
		fmt.Fprintf(&b, "%s — %s, cycle #%d, %d active, %d need attention, %d pending requests",
			view.Repository, mode, view.Control.CycleNumber, view.ActiveTasks, attentionCount(view), view.PendingRequests)
		ins, err := app.Insights(7)
		if err == nil {
			fmt.Fprintf(&b, "\n  Tasks: %s · Cycles: %s · PRs: %s",
				countLine(ins.Tasks), countLine(ins.Cycles), countLine(ins.PRs))
		}
	}
	return boundText(b.String())
}

func countLine(counts map[string]uint64) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// nextDigest returns the next local HH:MM boundary strictly after now.
func nextDigest(now time.Time, loc *time.Location, hhmm string) time.Time {
	t, _ := time.Parse("15:04", hhmm)
	local := now.In(loc)
	year, month, day := local.Date()
	next := time.Date(year, month, day, t.Hour(), t.Minute(), 0, 0, loc)
	if !next.After(local) {
		// Advance the calendar date and reconstruct the wall time: adding
		// a day to a spring-forward-normalized nonexistent time (02:30
		// becoming 01:30) would schedule tomorrow at the wrong hour and
		// then fire twice.
		year, month, day = time.Date(year, month, day, 0, 0, 0, 0, loc).AddDate(0, 0, 1).Date()
		next = time.Date(year, month, day, t.Hour(), t.Minute(), 0, 0, loc)
	}
	return next
}

// runDigest posts the digest to every alert channel on the configured
// daily boundary until ctx ends.
func (s *Service) runDigest(ctx context.Context, post func(context.Context, Provider, string, Response) error) {
	if s.cfg.DigestTime == "" {
		return
	}
	for {
		next := nextDigest(s.now(), s.cfg.Timezone, s.cfg.DigestTime)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		text := s.Digest(ctx)
		for _, pc := range []*ProviderConfig{s.cfg.Slack, s.cfg.Discord} {
			if ctx.Err() != nil {
				return
			}
			if pc == nil || pc.AlertChannel == "" {
				continue
			}
			if err := post(ctx, pc.Provider, pc.AlertChannel, Response{Text: text}); err != nil {
				s.warn(err)
			}
		}
	}
}
