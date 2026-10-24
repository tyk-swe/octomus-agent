package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/engine"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

const helpText = `Octomus commands (add --repo OWNER/REPO anywhere to pick a repository):
status — mode, cycle, active tasks, today's sessions
digest — 7-day summary for every repository
tasks [N] — latest tasks · task <id> — one task's detail
cycles [N] — latest cycles · prs [N] — observed pull requests
requests — pending work requests
request <title> :: <description> — file work for the next planning cycle
pause · resume · cycle · audit — operating controls (ask for confirmation)
retry <task-id> · cancel <task-id> · withdraw <request-id> (ask for confirmation)
repos — repositories this instance serves`

// parseCommand splits invocation text into verb, positional fields and the
// optional --repo flag.
func parseCommand(text string) (verb string, fields []string, rest string, repo string, err error) {
	var kept []string
	raw := strings.Fields(text)
	for i := 0; i < len(raw); i++ {
		switch {
		case raw[i] == "--repo" && i+1 < len(raw):
			repo = raw[i+1]
			i++
		case strings.HasPrefix(raw[i], "--repo="):
			repo = strings.TrimPrefix(raw[i], "--repo=")
		case strings.HasPrefix(raw[i], "--"):
			return "", nil, "", "", fmt.Errorf("Unknown flag %s; only --repo is supported", raw[i])
		default:
			kept = append(kept, raw[i])
		}
	}
	if len(kept) == 0 {
		return "help", nil, "", repo, nil
	}
	verb = strings.ToLower(kept[0])
	fields = kept[1:]
	// rest preserves free text after the verb (used by request, where
	// spacing and newlines matter) minus any --repo flag.
	rest = text
	if idx := strings.Index(rest, kept[0]); idx >= 0 {
		rest = rest[idx+len(kept[0]):]
	}
	rest = stripRepoFlag(rest)
	return verb, fields, strings.TrimSpace(rest), repo, nil
}

func stripRepoFlag(text string) string {
	fields := strings.Fields(text)
	var kept []string
	for i := 0; i < len(fields); i++ {
		if fields[i] == "--repo" && i+1 < len(fields) {
			i++
			continue
		}
		if strings.HasPrefix(fields[i], "--repo=") {
			continue
		}
		kept = append(kept, fields[i])
	}
	return strings.Join(kept, " ")
}

// resolveApp picks the repository's App: explicit --repo, then the thread's
// remembered repository, then the channel binding, then the primary.
func (s *Service) resolveApp(inv Invocation, repoFlag string) (*engine.App, error) {
	repo := repoFlag
	if repo == "" && s.store != nil {
		if ctx, err := s.store.ChatContext(string(inv.Provider), inv.ChannelID, inv.ThreadID); err == nil {
			repo = ctx.Repository
		}
	}
	if repo == "" {
		if pc := s.cfg.provider(inv.Provider); pc != nil {
			repo = pc.Bindings[inv.ChannelID]
		}
	}
	app := s.fleet.For(repo)
	if app == nil {
		return nil, fmt.Errorf("Repository %s is not served by this instance; try `repos`", repo)
	}
	return app, nil
}

func (s *Service) remember(inv Invocation, app *engine.App, taskID string) {
	if s.store == nil || app == nil {
		return
	}
	current, _ := s.store.ChatContext(string(inv.Provider), inv.ChannelID, inv.ThreadID)
	if taskID == "" {
		taskID = current.TaskID
	}
	_ = s.store.SetChatContext(string(inv.Provider), inv.ChannelID, inv.ThreadID,
		store.ChatContext{Repository: app.Repository(), TaskID: taskID}, s.now())
}

func (s *Service) handleCommand(ctx context.Context, inv Invocation) (string, []Button, error) {
	verb, fields, rest, repoFlag, err := parseCommand(inv.Text)
	if err != nil {
		return "", nil, err
	}
	switch verb {
	case "help":
		return helpText, nil, nil
	case "repos":
		return s.reposText(), nil, nil
	case "digest":
		return s.Digest(ctx), nil, nil
	}
	app, err := s.resolveApp(inv, repoFlag)
	if err != nil {
		return "", nil, err
	}
	text, buttons, taskID, err := s.dispatch(ctx, inv, app, verb, fields, rest)
	if err == nil {
		s.remember(inv, app, taskID)
	}
	return text, buttons, err
}

func (s *Service) dispatch(ctx context.Context, inv Invocation, app *engine.App, verb string, fields []string, rest string) (string, []Button, string, error) {
	switch verb {
	case "status":
		text, err := statusText(app)
		return text, nil, "", err
	case "tasks":
		text, err := tasksText(app, listLimit(fields))
		return text, nil, "", err
	case "task":
		return s.taskDetail(ctx, inv, app, fields)
	case "cycles":
		text, err := cyclesText(app, listLimit(fields))
		return text, nil, "", err
	case "prs":
		text, err := prsText(app, listLimit(fields))
		return text, nil, "", err
	case "requests":
		text, err := requestsText(app)
		return text, nil, "", err
	case "request":
		text, err := s.createRequest(app, rest)
		return text, nil, "", err
	case "pause", "resume", "cycle", "audit":
		return s.controlConfirm(ctx, inv, app, verb)
	case "retry", "cancel":
		return s.taskConfirm(ctx, inv, app, verb, fields)
	case "withdraw":
		return s.withdrawConfirm(ctx, inv, app, fields)
	}
	return "", nil, "", fmt.Errorf("Unknown command %q; try `help`", verb)
}

func listLimit(fields []string) int {
	if len(fields) > 0 {
		if n, err := strconv.Atoi(fields[0]); err == nil && n > 0 {
			if n > 20 {
				return 20
			}
			return n
		}
	}
	return defaultList
}

var controlActions = map[string]model.ControlAction{
	"pause": model.ControlPause, "resume": model.ControlResume,
	"cycle": model.ControlCycle, "audit": model.ControlAudit,
}
