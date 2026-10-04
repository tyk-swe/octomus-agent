package git

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
)

func gh(ctx context.Context, c config.Config, args []string) (string, error) {
	return process.RunMachine(ctx, "gh", args, c.Repository, c.CommandTimeoutSeconds, nil)
}

func ghPages(out string, each func(page []map[string]any) error) error {
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	for {
		var page []map[string]any
		err := dec.Decode(&page)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if page == nil {
			return errors.New("Expected a JSON array for pagination page, got null")
		}
		if err := each(page); err != nil {
			return err
		}
	}
}

func field(p map[string]any, keys ...string) any {
	var v any = p
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func text(p map[string]any, keys ...string) string {
	s, _ := field(p, keys...).(string)
	return s
}

func jnum(v any) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	u, err := strconv.ParseUint(n.String(), 10, 64)
	return u, err == nil
}

func OpenPRs(ctx context.Context, c config.Config) (model.OpenPRInventory, error) {
	observedAt := model.Now()
	out, err := gh(ctx, c, []string{
		"api", "--paginate",
		fmt.Sprintf("repos/%s/pulls?state=open&per_page=100", c.GitHubRepo),
	})
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	inventory, err := parseInventory(out, c)
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	inventory.ObservedAt = observedAt
	return inventory, nil
}

func parseInventory(out string, c config.Config) (model.OpenPRInventory, error) {
	prs := map[uint64]model.PullRequest{}
	pages := 0
	err := ghPages(out, func(page []map[string]any) error {
		pages++
		for _, p := range page {
			if state, _ := field(p, "state").(string); state != "open" && state != "closed" {
				return errors.New("Open PR entry has an unrecognized state")
			}
			pr, err := parsePR(p, c)
			if err != nil {
				return err
			}
			if pr.Branch == "" || pr.Head == "" || pr.Base == "" ||
				pr.BaseRepository == "" || pr.URL == "" {
				return errors.New("Open PR entry is missing required identity")
			}
			if !config.EqualASCII(pr.BaseRepository, c.GitHubRepo) {
				return errors.New("Open PR entry reports a different base repository")
			}
			if pr.State != "open" {
				continue
			}
			if existing, ok := prs[pr.Number]; ok {
				if existing != pr {
					return errors.New("Conflicting open PR inventory entries")
				}
			} else {
				prs[pr.Number] = pr
			}
		}
		return nil
	})
	if err != nil {
		return model.OpenPRInventory{}, err
	}
	if pages < 1 {
		return model.OpenPRInventory{}, errors.New("Open PR inventory response is empty")
	}
	ordered := make([]model.PullRequest, 0, len(prs))
	for _, pr := range prs {
		ordered = append(ordered, pr)
	}
	slices.SortFunc(ordered, func(a, b model.PullRequest) int {
		return cmp.Compare(a.Number, b.Number)
	})
	return model.OpenPRInventory{Repository: c.GitHubRepo, PRs: ordered}, nil
}

func OwnedPRs(ctx context.Context, c config.Config, inventory model.OpenPRInventory) ([]model.PullRequest, error) {
	prs := []model.PullRequest{}
	for _, observed := range inventory.PRs {
		if !observed.Owned {
			continue
		}
		detail, err := PR(ctx, c, observed.Number)
		if err != nil {
			return nil, err
		}
		if !detail.OwnedOpen() || detail.Branch != observed.Branch || detail.Base != observed.Base {
			return nil, errors.New("Owned PR changed while the open inventory was being read")
		}
		prs = append(prs, detail)
	}
	return prs, nil
}

func PR(ctx context.Context, c config.Config, number uint64) (model.PullRequest, error) {
	out, err := gh(ctx, c, []string{
		"api", fmt.Sprintf("repos/%s/pulls/%d", c.GitHubRepo, number),
	})
	if err != nil {
		return model.PullRequest{}, err
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.UseNumber()
	var p map[string]any
	if err := dec.Decode(&p); err != nil {
		return model.PullRequest{}, err
	}
	return parsePR(p, c)
}

const taskMarkerPrefix = "<!-- octomus:task:"

func taskMarkerFor(taskID string) string {
	return taskMarkerPrefix + taskID + " -->"
}

// TaskMarker reports whether the PR's body or any of its comments carries the task's delivery marker.
func TaskMarker(ctx context.Context, c config.Config, taskID string, p model.PullRequest) (bool, error) {
	marker := taskMarkerFor(taskID)
	if strings.Contains(p.Body, marker) {
		return true, nil
	}
	out, err := gh(ctx, c, []string{
		"api", "--paginate",
		fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", c.GitHubRepo, p.Number),
	})
	if err != nil {
		return false, err
	}
	found := false
	err = ghPages(out, func(page []map[string]any) error {
		for _, value := range page {
			if strings.Contains(text(value, "body"), marker) {
				found = true
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

func parsePR(p map[string]any, c config.Config) (model.PullRequest, error) {
	number, ok := jnum(field(p, "number"))
	if !ok {
		return model.PullRequest{}, errors.New("Missing PR number")
	}
	branch := text(p, "head", "ref")
	body := text(p, "body")
	state := text(p, "state")
	if _, merged := field(p, "merged_at").(string); merged {
		state = "merged"
	}
	additions, _ := jnum(field(p, "additions"))
	deletions, _ := jnum(field(p, "deletions"))
	headRepo, headRepoOk := field(p, "head", "repo", "full_name").(string)
	baseRepo, baseRepoOk := field(p, "base", "repo", "full_name").(string)
	owned := strings.HasPrefix(branch, c.BranchPrefix) &&
		headRepoOk && config.EqualASCII(headRepo, c.GitHubRepo) &&
		baseRepoOk && config.EqualASCII(baseRepo, c.GitHubRepo) &&
		strings.Contains(body, taskMarkerPrefix)
	return model.PullRequest{
		Number:         number,
		Title:          text(p, "title"),
		Branch:         branch,
		Head:           text(p, "head", "sha"),
		Base:           text(p, "base", "ref"),
		URL:            text(p, "html_url"),
		Body:           body,
		State:          state,
		ChangedLines:   additions + deletions,
		CreatedAt:      text(p, "created_at"),
		Owned:          owned,
		HeadRepository: headRepo,
		BaseRepository: baseRepo,
	}, nil
}

func PublicationPR(ctx context.Context, c config.Config, branch string) (*model.PullRequest, error) {
	owner, _, _ := strings.Cut(c.GitHubRepo, "/")
	out, err := gh(ctx, c, []string{
		"api", "--paginate",
		fmt.Sprintf("repos/%s/pulls?state=all&head=%s:%s&per_page=100", c.GitHubRepo, owner, branch),
	})
	if err != nil {
		return nil, err
	}
	var matches []model.PullRequest
	err = ghPages(out, func(page []map[string]any) error {
		for _, value := range page {
			candidate, err := parsePR(value, c)
			if err != nil {
				return err
			}
			if candidate.Branch == branch {
				matches = append(matches, candidate)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(matches) > 1 {
		return nil, blocked(model.BlockedRemoteConflict,
			"Ambiguous PR association; reconcile before publication")
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return &matches[0], nil
}
