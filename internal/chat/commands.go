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
// optional --repo flag. Token end offsets are tracked so rest starts
// exactly after the verb token, even when the verb also appears inside a
// preceding --repo value.
func parseCommand(text string) (verb string, fields []string, rest string, repo string, err error) {
	type token struct {
		value string
		end   int // byte offset just past the token in text
	}
	var tokens []token
	cursor := 0
	for _, field := range strings.Fields(text) {
		idx := strings.Index(text[cursor:], field)
		if idx < 0 {
			break
		}
		end := cursor + idx + len(field)
		tokens = append(tokens, token{value: field, end: end})
		cursor = end
	}
	var kept []token
	var unknown []string
	for i := 0; i < len(tokens); i++ {
		switch {
		case tokens[i].value == "--repo" && i+1 < len(tokens):
			repo = tokens[i+1].value
			i++
		case strings.HasPrefix(tokens[i].value, "--repo="):
			repo = strings.TrimPrefix(tokens[i].value, "--repo=")
		case strings.HasPrefix(tokens[i].value, "--"):
			unknown = append(unknown, tokens[i].value)
		default:
			kept = append(kept, tokens[i])
		}
	}
	if len(kept) == 0 {
		if len(unknown) > 0 {
			return "", nil, "", "", fmt.Errorf("Unknown flag %s; only --repo is supported", unknown[0])
		}
		return "help", nil, "", repo, nil
	}
	verb = strings.ToLower(kept[0].value)
	// A work request's description is free text: CLI flags mentioned in it
	// (--help, --dry-run, …) are content, not chat flags to reject.
	if verb != "request" && len(unknown) > 0 {
		return "", nil, "", "", fmt.Errorf("Unknown flag %s; only --repo is supported", unknown[0])
	}
	for _, tok := range kept[1:] {
		fields = append(fields, tok.value)
	}
	// rest preserves free text after the verb (used by request, where
	// spacing and newlines matter) minus any --repo flag.
	rest = stripRepoFlag(text[kept[0].end:])
	return verb, fields, strings.TrimSpace(rest), repo, nil
}

// stripRepoFlag removes only the --repo flag's own text span. Free-form
// request text keeps its newlines, tabs and indentation byte for byte.
func stripRepoFlag(text string) string {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	var b strings.Builder
	for i := 0; i < len(text); {
		boundary := i == 0 || isSpace(text[i-1])
		if boundary && strings.HasPrefix(text[i:], "--repo") {
			rest := text[i+len("--repo"):]
			if strings.HasPrefix(rest, "=") {
				j := i + len("--repo=")
				for j < len(text) && !isSpace(text[j]) {
					j++
				}
				i = j
				continue
			}
			if rest == "" || isSpace(rest[0]) {
				j := i + len("--repo")
				for j < len(text) && isSpace(text[j]) {
					j++
				}
				for j < len(text) && !isSpace(text[j]) {
					j++
				}
				i = j
				continue
			}
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
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

// resolveCommandApp routes ID-addressed verbs through the record's owning
// repository, exactly like the HTTP API: an explicit --repo naming another
// owner is not-found, and implicit thread/channel defaults never let one
// repository's App act on another's task or request.
func (s *Service) resolveCommandApp(inv Invocation, verb string, fields []string, repoFlag string) (*engine.App, error) {
	kind := ""
	switch verb {
	case "task", "retry", "cancel":
		kind = "task"
	case "withdraw":
		kind = "request"
	default:
		return s.resolveApp(inv, repoFlag)
	}
	id := ""
	if len(fields) > 0 {
		id = fields[0]
	} else if verb == "task" && s.store != nil {
		if ctx, err := s.store.ChatContext(string(inv.Provider), inv.ChannelID, inv.ThreadID); err == nil {
			id = ctx.TaskID
		}
	}
	if id == "" {
		// Dispatch produces the "Name a …" guidance on the default App.
		return s.resolveApp(inv, repoFlag)
	}
	return s.fleet.ForRecord(kind, id, repoFlag)
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
	app, err := s.resolveCommandApp(inv, verb, fields, repoFlag)
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
