package chat

import (
	"context"
	"fmt"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
)

func (s *Service) taskDetail(_ context.Context, inv Invocation, app *engine.App, fields []string) (string, []Button, string, error) {
	id := ""
	if len(fields) > 0 {
		id = fields[0]
	} else if s.store != nil {
		if ctx, err := s.store.ChatContext(string(inv.Provider), inv.ChannelID, inv.ThreadID); err == nil {
			id = ctx.TaskID
		}
	}
	if id == "" {
		return "", nil, "", fmt.Errorf("Name a task: `task <id>`")
	}
	detail, err := app.TaskDetail(id)
	if err != nil {
		return "", nil, "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s [%s]\n%s", detail.ID, detail.Status, detail.Proposal.Title)
	if detail.PRURL != nil && *detail.PRURL != "" {
		fmt.Fprintf(&b, "\nPR: %s", *detail.PRURL)
	}
	if detail.Error != nil && *detail.Error != "" {
		fmt.Fprintf(&b, "\nError: %s", *detail.Error)
	}
	fmt.Fprintf(&b, "\nTarget: %s · Branch: %s · Updated: %s", detail.Proposal.Target, detail.Branch, detail.UpdatedAt)
	var buttons []Button
	for _, action := range detail.AllowedActions {
		switch action {
		case model.TaskActionRetry:
			buttons = append(buttons, Button{Label: "Retry task", Value: taskActionValue("retry", id)})
		case model.TaskActionCancel:
			buttons = append(buttons, Button{Label: "Cancel task", Value: taskActionValue("cancel", id)})
		}
	}
	return b.String(), buttons, id, nil
}

func (s *Service) createRequest(app *engine.App, rest string) (string, error) {
	title, description, found := strings.Cut(rest, "::")
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	if title == "" {
		return "", fmt.Errorf("Describe the work: `request <title> :: <description>`")
	}
	if !found || description == "" {
		description = title
	}
	cfg, err := app.Config()
	if err != nil {
		return "", err
	}
	rec, err := app.CreateRequest(engine.RequestInput{Title: title, Description: description, Target: cfg.DefaultBranch})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Request %s filed for %s: %q. It enters the next planning cycle as a candidate; planning still reviews it like any discovered work.", rec.ID, rec.Repository, rec.Title), nil
}

func (s *Service) controlConfirm(_ context.Context, inv Invocation, app *engine.App, verb string) (string, []Button, string, error) {
	action := controlActions[verb]
	repo := app.Repository()
	labels := map[string]string{
		"pause": "Pause", "resume": "Resume continuous operation for",
		"cycle": "Run one cycle for", "audit": "Run an audit for",
	}
	return s.confirm(inv, fmt.Sprintf("%s %s?", labels[verb], repo), func(ctx context.Context) (string, error) {
		_, err := app.ControlAction(action)
		if err != nil {
			return "", err
		}
		return statusText(app)
	})
}

func (s *Service) taskConfirm(_ context.Context, inv Invocation, app *engine.App, verb string, fields []string) (string, []Button, string, error) {
	if len(fields) == 0 {
		return "", nil, "", fmt.Errorf("Name a task: `%s <id>`", verb)
	}
	id := fields[0]
	action := model.TaskActionRetry
	if verb == "cancel" {
		action = model.TaskActionCancel
	}
	text, buttons, _, err := s.confirm(inv, fmt.Sprintf("%s task %s on %s?", capitalize(verb), id, app.Repository()), func(ctx context.Context) (string, error) {
		if err := app.TaskAction(id, action); err != nil {
			return "", err
		}
		return fmt.Sprintf("Task %s: %s requested.", id, verb), nil
	})
	return text, buttons, id, err
}

func (s *Service) withdrawConfirm(_ context.Context, inv Invocation, app *engine.App, fields []string) (string, []Button, string, error) {
	if len(fields) == 0 {
		return "", nil, "", fmt.Errorf("Name a request: `withdraw <id>`")
	}
	id := fields[0]
	return s.confirm(inv, fmt.Sprintf("Withdraw request %s on %s?", id, app.Repository()), func(ctx context.Context) (string, error) {
		if err := app.RequestAction(id, model.RequestActionWithdraw); err != nil {
			return "", err
		}
		return "Request " + id + " withdrawn.", nil
	})
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
