package chat

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// attentionCount totals every blocked/failed task from the dashboard
// status counts. AttentionTasks itself is only a five-item preview, so
// its length must never be reported as the backlog.
func attentionCount(view *engine.StateView) int64 {
	var total int64
	for _, status := range model.AttentionStatuses() {
		total += view.Counts[status.String()]
	}
	return total
}

func statusText(app *engine.App) (string, error) {
	view, err := app.StateView()
	if err != nil {
		return "", err
	}
	repo := view.Repository
	if repo == "" {
		repo = "(no repository configured)"
	}
	mode := view.Control.Mode.String()
	if view.Control.Paused {
		mode = "paused"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s, cycle #%d\n", repo, mode, view.Control.CycleNumber)
	fmt.Fprintf(&b, "Active tasks: %d · Needs attention: %d · Pending requests: %d\n",
		view.ActiveTasks, attentionCount(view), view.PendingRequests)
	fmt.Fprintf(&b, "Sessions today: %d/%d · Open owned PRs use the dashboard for detail", view.SessionsToday, view.SessionLimit)
	if view.Control.Error != nil && *view.Control.Error != "" {
		fmt.Fprintf(&b, "\nError: %s", *view.Control.Error)
	}
	if view.RecoveryError != nil && *view.RecoveryError != "" {
		fmt.Fprintf(&b, "\nRecovery: %s", *view.RecoveryError)
	}
	if len(view.AttentionTasks) > 0 {
		b.WriteString("\nNeeds attention:")
		for _, t := range view.AttentionTasks {
			fmt.Fprintf(&b, "\n- %s [%s] %s", shortID(t.ID), t.Status, t.Title)
		}
	}
	return b.String(), nil
}

func shortID(id string) string { return id }

func tasksText(app *engine.App, limit int) (string, error) {
	page, err := app.TaskPage(store.HistoryQuery{Limit: &limit})
	if err != nil {
		return "", err
	}
	if len(page.Items) == 0 {
		return "No tasks yet for " + app.Repository() + ".", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Latest tasks for %s:", app.Repository())
	for _, t := range page.Items {
		fmt.Fprintf(&b, "\n- %s [%s] %s", t.ID, t.Status, t.Title)
		if t.PRURL != nil && *t.PRURL != "" {
			fmt.Fprintf(&b, " — %s", *t.PRURL)
		} else if t.Error != nil && *t.Error != "" {
			fmt.Fprintf(&b, " — %s", *t.Error)
		}
	}
	return b.String(), nil
}

func cyclesText(app *engine.App, limit int) (string, error) {
	page, err := app.CyclePage(store.HistoryQuery{Limit: &limit})
	if err != nil {
		return "", err
	}
	if len(page.Items) == 0 {
		return "No cycles yet for " + app.Repository() + ".", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Latest cycles for %s:", app.Repository())
	for _, c := range page.Items {
		fmt.Fprintf(&b, "\n- #%d %s [%s] accepted %d, rejected %d, deferred %d (%s)",
			c.Number, c.ID, c.Status, c.Decisions.Accepted, c.Decisions.Rejected, c.Decisions.Deferred, c.StartedAt)
	}
	return b.String(), nil
}

func prsText(app *engine.App, limit int) (string, error) {
	page, err := app.PRPage(store.HistoryQuery{Limit: &limit})
	if err != nil {
		return "", err
	}
	if len(page.Items) == 0 {
		return "No observed pull requests for " + app.Repository() + ".", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Observed pull requests for %s:", app.Repository())
	for _, p := range page.Items {
		fmt.Fprintf(&b, "\n- #%d [%s] %s — %s", p.PR.Number, p.PR.State, p.PR.Title, p.PR.URL)
	}
	return b.String(), nil
}

func requestsText(app *engine.App) (string, error) {
	view, err := app.Requests()
	if err != nil {
		return "", err
	}
	if len(view.Pending) == 0 && len(view.Recent) == 0 {
		return "No work requests for " + app.Repository() + ".", nil
	}
	var b strings.Builder
	if len(view.Pending) > 0 {
		b.WriteString("Pending requests:")
		for _, r := range view.Pending {
			fmt.Fprintf(&b, "\n- %s %s (target %s)", r.ID, r.Title, r.Target)
		}
	}
	if len(view.Recent) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Recent:")
		for i, r := range view.Recent {
			if i >= defaultList {
				break
			}
			fmt.Fprintf(&b, "\n- %s [%s] %s", r.ID, r.Status, r.Title)
		}
	}
	return b.String(), nil
}

func (s *Service) reposText() string {
	entries := s.fleet.Entries()
	if len(entries) == 0 {
		return "No repositories registered yet."
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.GitHubRepo)
	}
	sort.Strings(names)
	return "Repositories this instance serves:\n- " + strings.Join(names, "\n- ")
}
