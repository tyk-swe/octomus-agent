package engine

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// RequestInput is the operator's free-text request: what should change and where it should land.
type RequestInput struct {
	Title, Description, Target string
}

func (a *App) CreateRequest(input RequestInput) (*model.OperatorRequest, error) {
	a.gate.Lock()
	if err := a.ctx.Err(); err != nil {
		a.gate.Unlock()
		return nil, err
	}
	a.wg.Add(1)
	defer a.wg.Done()
	defer a.gate.Unlock()
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	if !config.ValidGitHubRepo(cfg.GitHubRepo) {
		return nil, conflictError("Save a GitHub repository before requesting work")
	}
	title := strings.TrimSpace(input.Title)
	if len(title) == 0 || len(title) > 200 || strings.ContainsFunc(title, unicode.IsControl) {
		return nil, errors.New("Request title must be 1–200 bytes without control characters")
	}
	description := strings.TrimSpace(input.Description)
	if len(description) == 0 || len(description) > 8000 || !utf8.ValidString(description) || strings.ContainsFunc(description, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\t'
	}) {
		return nil, errors.New("Request description must be 1–8000 bytes without control characters other than newlines and tabs")
	}
	if input.Target != cfg.DefaultBranch && !(strings.HasPrefix(input.Target, cfg.BranchPrefix) && config.ValidBranch(input.Target)) {
		return nil, errors.New("Request target must be the default branch or an owned pull request branch")
	}
	now := model.Now()
	record := model.OperatorRequest{
		ID: model.ID(), Repository: cfg.GitHubRepo, Title: title, Description: description,
		Target: input.Target, Status: model.RequestPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := a.store.CreateRequest(record, model.RequestPendingLimit); err != nil {
		if errors.Is(err, store.ErrRequestLimit) {
			return nil, conflictError("At most 20 requests can wait for planning; withdraw one or let a cycle assess them")
		}
		return nil, err
	}
	_ = a.store.Event(record.ID, model.EventRequest, "Requested: "+title)
	a.notify()
	return &record, nil
}

func (a *App) RequestAction(id string, action model.RequestAction) error {
	a.gate.Lock()
	if err := a.ctx.Err(); err != nil {
		a.gate.Unlock()
		return err
	}
	a.wg.Add(1)
	defer a.wg.Done()
	defer a.gate.Unlock()
	if action != model.RequestActionWithdraw {
		return NotFound("Unknown request action")
	}
	current, err := store.Get(a.store, store.Requests.Key(id))
	if err != nil {
		return err
	}
	if current == nil {
		return NotFound("Request not found")
	}
	if current.Status != model.RequestPending {
		return conflictError("Only pending requests can be withdrawn")
	}
	a.runtimeMu.Lock()
	planning := a.runtime.planning()
	a.runtimeMu.Unlock()
	if planning {
		return codedConflict(ActiveWorkConflict, "Wait for planning to finish before withdrawing a request")
	}
	next := current.Clone()
	next.Status = model.RequestWithdrawn
	next.UpdatedAt = model.Now()
	if err := a.store.UpdateRequest(store.RequestUpdate{Expected: *current, Next: next}); err != nil {
		if errors.Is(err, store.ErrRequestChanged) {
			return conflictError(err.Error())
		}
		return err
	}
	_ = a.store.Event(id, model.EventRequest, "Withdrawn")
	a.notify()
	return nil
}

type RequestTarget struct {
	Branch string `json:"branch"`
	Number uint64 `json:"number"`
	Title  string `json:"title"`
}

type RequestsView struct {
	Repository    string                  `json:"repository"`
	DefaultBranch string                  `json:"default_branch"`
	Pending       []model.OperatorRequest `json:"pending"`
	Recent        []model.OperatorRequest `json:"recent"`
	PendingLimit  int                     `json:"pending_limit"`
	SeedLimit     int                     `json:"seed_limit"`
	Targets       []RequestTarget         `json:"targets"`
}

func scrubRequests(requests []model.OperatorRequest) {
	for i := range requests {
		requests[i].Title = redact.Secrets(requests[i].Title)
		requests[i].Description = redact.Secrets(requests[i].Description)
		if requests[i].Assessment != nil {
			requests[i].Assessment.Reason = redact.Secrets(requests[i].Assessment.Reason)
		}
	}
}

func (a *App) Requests() (*RequestsView, error) {
	cfg, err := a.Config()
	if err != nil {
		return nil, err
	}
	pending, err := a.store.PendingRequests(cfg.GitHubRepo, model.RequestPendingLimit)
	if err != nil {
		return nil, err
	}
	recent, err := a.store.RecentRequests(cfg.GitHubRepo, 50)
	if err != nil {
		return nil, err
	}
	inventory, err := a.store.OpenPRInventory()
	if err != nil {
		return nil, err
	}
	targets := []RequestTarget{}
	if inventory != nil {
		for _, pr := range inventory.PRs {
			if !pr.OwnedOpen() || pr.Base != cfg.DefaultBranch || !config.EqualASCII(pr.BaseRepository, cfg.GitHubRepo) {
				continue
			}
			title, _ := truncateRunes(redact.Secrets(pr.Title), 200)
			targets = append(targets, RequestTarget{Branch: pr.Branch, Number: pr.Number, Title: title})
		}
		slices.SortFunc(targets, func(a, b RequestTarget) int { return cmp.Compare(a.Number, b.Number) })
	}
	scrubRequests(pending)
	scrubRequests(recent)
	return &RequestsView{
		Repository: cfg.GitHubRepo, DefaultBranch: cfg.DefaultBranch,
		Pending: pending, Recent: recent,
		PendingLimit: model.RequestPendingLimit, SeedLimit: model.RequestSeedLimit,
		Targets: targets,
	}, nil
}
