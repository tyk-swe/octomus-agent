package chat

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
)

// DigestMessages builds the fleet-wide 7-day summary as provider-sized
// messages split at repository boundaries, so a large fleet never loses
// later repositories to one aggregate truncation limit. On a schedule it
// doubles as the heartbeat: a missing digest means the service is down.
func (s *Service) DigestMessages(_ context.Context) []string {
	apps := s.fleet.Apps()
	if len(apps) == 0 {
		return []string{"Octomus digest: no repositories registered."}
	}
	sections := make([]string, 0, len(apps))
	for _, app := range apps {
		sections = append(sections, digestAppSection(app))
	}
	return packDigestSections(sections)
}

func digestAppSection(app *engine.App) string {
	view, err := app.StateView()
	if err != nil {
		return fmt.Sprintf("%s — status unavailable: %s", app.Repository(), operatorError(err))
	}
	mode := view.Control.Mode.String()
	if view.Control.Paused {
		mode = "paused"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s, cycle #%d, %d active, %d need attention, %d pending requests",
		view.Repository, mode, view.Control.CycleNumber, view.ActiveTasks, attentionCount(view), view.PendingRequests)
	if ins, err := app.Insights(7); err == nil {
		fmt.Fprintf(&b, "\n  Tasks: %s · Cycles: %s · PRs: %s",
			countLine(ins.Tasks), countLine(ins.Cycles), countLine(ins.PRs))
	}
	return b.String()
}

// packDigestSections packs whole repository sections into messages no
// longer than maxTextLen. A single oversized section is bounded on its
// own rather than crowding out every following repository.
func packDigestSections(sections []string) []string {
	const (
		header    = "Octomus digest (7 days)"
		continued = header + " (continued)"
		// Enough for "\n\nPage 999 of 999" and then some.
		pageReserve = 32
	)
	limit := maxTextLen - pageReserve
	messages := []string{}
	current := header
	currentSections := 0
	flush := func() {
		if currentSections == 0 {
			return
		}
		messages = append(messages, boundTextLimit(current, limit))
		current = continued
		currentSections = 0
	}
	for _, section := range sections {
		section = cleanText(section)
		if section == "" {
			continue
		}
		candidate := current + "\n\n" + section
		if len(cleanText(candidate)) > limit && currentSections > 0 {
			flush()
			candidate = current + "\n\n" + section
		}
		if len(cleanText(candidate)) <= limit {
			current = candidate
			currentSections++
			continue
		}
		messages = append(messages, boundTextLimit(candidate, limit))
		current = continued
		currentSections = 0
	}
	flush()
	if len(messages) == 0 {
		messages = append(messages, header)
	}
	if len(messages) > 1 {
		for i := range messages {
			messages[i] = fmt.Sprintf("%s\n\nPage %d of %d", messages[i], i+1, len(messages))
		}
	}
	return messages
}

func (s *Service) digestCommand(ctx context.Context, fields []string) (string, []Button, error) {
	page := 1
	if len(fields) > 0 {
		parsed, err := strconv.Atoi(fields[0])
		if err != nil || parsed < 1 {
			return "", nil, fmt.Errorf("Name a digest page: `digest [page]`")
		}
		page = parsed
	}
	return s.digestPage(ctx, page)
}

func (s *Service) digestPage(ctx context.Context, page int) (string, []Button, error) {
	messages := s.DigestMessages(ctx)
	if page < 1 || page > len(messages) {
		return "", nil, fmt.Errorf("Digest page %d does not exist; it has %d page(s)", page, len(messages))
	}
	var buttons []Button
	if page > 1 {
		buttons = append(buttons, Button{Label: "Previous page", Value: fmt.Sprintf("digest:%d", page-1)})
	}
	if page < len(messages) {
		buttons = append(buttons, Button{Label: "Next page", Value: fmt.Sprintf("digest:%d", page+1)})
	}
	return messages[page-1], buttons, nil
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
		s.sendDigest(ctx, post)
	}
}

// sendDigest posts every digest page, in order, to every alert channel.
func (s *Service) sendDigest(ctx context.Context, post func(context.Context, Provider, string, Response) error) {
	messages := s.DigestMessages(ctx)
	for _, pc := range []*ProviderConfig{s.cfg.Slack, s.cfg.Discord} {
		if pc == nil || pc.AlertChannel == "" {
			continue
		}
		for _, text := range messages {
			if ctx.Err() != nil {
				return
			}
			if err := post(ctx, pc.Provider, pc.AlertChannel, Response{Text: text}); err != nil {
				s.warn(err)
			}
		}
	}
}
