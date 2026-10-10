package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/redact"
)

// ErrMergeUnconfirmed identifies failures after the mutation returned successfully.
// An HTTP error from a later read is not a refusal of the already-completed PUT.
var ErrMergeUnconfirmed = errors.New("Merge request outcome is unconfirmed")

type MergeStatus struct {
	Number         uint64
	URL            string
	State          string
	HeadBranch     string
	Head           string
	HeadRepository string
	BaseBranch     string
	BaseOID        string
	Repository     string
	Draft          bool
	MergeQueue     bool
	SquashAllowed  bool
	ReviewDecision *string
	Mergeable      string
	MergeState     string
	Additions      *uint64
	Deletions      *uint64
	ChangedFiles   *uint64
	MergedAt       *string
	MergeCommit    *string
	CheckState     *string
	CheckContexts  uint64
	StatsMalformed bool
}

func MaintenanceMergeStatus(ctx context.Context, c config.Config, number uint64) (MergeStatus, error) {
	owner, name, _ := strings.Cut(c.GitHubRepo, "/")
	out, err := gh(ctx, c, []string{"api", "--hostname", "github.com", "graphql", "-f", "query=" + maintenanceMergeStatusQuery,
		"-f", "owner=" + owner, "-f", "name=" + name, "-F", fmt.Sprintf("number=%d", number)})
	if err != nil {
		return MergeStatus{}, err
	}
	return parseMergeStatus(out, c.GitHubRepo, number)
}

func parseMergeStatus(out, repository string, number uint64) (MergeStatus, error) {
	var response struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			Repository *struct {
				NameWithOwner      string `json:"nameWithOwner"`
				SquashMergeAllowed *bool  `json:"squashMergeAllowed"`
				PullRequest        *struct {
					Number   uint64 `json:"number"`
					URL      string `json:"url"`
					State    string `json:"state"`
					HeadRef  string `json:"headRefName"`
					HeadOID  string `json:"headRefOid"`
					HeadRepo *struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"headRepository"`
					BaseRef  string `json:"baseRefName"`
					BaseOID  string `json:"baseRefOid"`
					BaseRepo *struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
					Draft          *bool           `json:"isDraft"`
					MergeQueue     *bool           `json:"isMergeQueueEnabled"`
					ReviewDecision json.RawMessage `json:"reviewDecision"`
					Mergeable      string          `json:"mergeable"`
					MergeState     string          `json:"mergeStateStatus"`
					Additions      json.RawMessage `json:"additions"`
					Deletions      json.RawMessage `json:"deletions"`
					ChangedFiles   json.RawMessage `json:"changedFiles"`
					MergedAt       *string         `json:"mergedAt"`
					MergeCommit    *struct {
						OID string `json:"oid"`
					} `json:"mergeCommit"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								OID    string `json:"oid"`
								Rollup *struct {
									State    string `json:"state"`
									Contexts *struct {
										TotalCount uint64 `json:"totalCount"`
									} `json:"contexts"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return MergeStatus{}, errors.New("Invalid GitHub merge status response")
	}
	repo := response.Data.Repository
	if len(response.Errors) != 0 || repo == nil || repo.PullRequest == nil {
		return MergeStatus{}, errors.New("GitHub merge status query did not return a pull request")
	}
	if !config.EqualASCII(repo.NameWithOwner, repository) || repo.SquashMergeAllowed == nil {
		return MergeStatus{}, errors.New("GitHub merge status reports a different or incomplete repository")
	}
	p := repo.PullRequest
	if p.Number != number || p.URL == "" || p.HeadRef == "" || p.BaseRef == "" ||
		!footprintRevision(p.HeadOID) || !footprintRevision(p.BaseOID) ||
		p.HeadRepo == nil || p.BaseRepo == nil || p.Draft == nil || p.MergeQueue == nil ||
		!config.EqualASCII(p.HeadRepo.NameWithOwner, repository) ||
		!config.EqualASCII(p.BaseRepo.NameWithOwner, repository) {
		return MergeStatus{}, errors.New("GitHub merge status is missing required pull request identity")
	}
	status := MergeStatus{
		Number: p.Number, URL: p.URL, HeadBranch: p.HeadRef, Head: p.HeadOID,
		HeadRepository: p.HeadRepo.NameWithOwner, BaseBranch: p.BaseRef, BaseOID: p.BaseOID,
		Repository: repo.NameWithOwner, Draft: *p.Draft, MergeQueue: *p.MergeQueue,
		SquashAllowed: *repo.SquashMergeAllowed, Mergeable: p.Mergeable, MergeState: p.MergeState,
		MergedAt: p.MergedAt,
	}
	countField := func(raw json.RawMessage, out **uint64) {
		if len(raw) == 0 || string(raw) == "null" {
			return
		}
		var count uint64
		if err := json.Unmarshal(raw, &count); err != nil {
			status.StatsMalformed = true
			return
		}
		*out = &count
	}
	countField(p.Additions, &status.Additions)
	countField(p.Deletions, &status.Deletions)
	countField(p.ChangedFiles, &status.ChangedFiles)
	switch p.State {
	case "OPEN":
		status.State = "open"
	case "CLOSED":
		status.State = "closed"
	case "MERGED":
		status.State = "merged"
	default:
		return MergeStatus{}, errors.New("Unrecognized GitHub pull request state")
	}
	if p.MergeCommit != nil {
		if !footprintRevision(p.MergeCommit.OID) {
			return MergeStatus{}, errors.New("GitHub merge commit is missing its revision")
		}
		status.MergeCommit = new(p.MergeCommit.OID)
	}
	switch p.Mergeable {
	case "CONFLICTING", "MERGEABLE", "UNKNOWN":
	default:
		return MergeStatus{}, errors.New("Unrecognized GitHub PR mergeability")
	}
	switch p.MergeState {
	case "BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE":
	default:
		return MergeStatus{}, errors.New("Unrecognized GitHub merge state")
	}
	if p.ReviewDecision == nil {
		return MergeStatus{}, errors.New("GitHub merge status is missing the review decision field")
	}
	if raw := strings.TrimSpace(string(p.ReviewDecision)); raw != "null" {
		var decision string
		if err := json.Unmarshal(p.ReviewDecision, &decision); err != nil {
			return MergeStatus{}, errors.New("Unrecognized GitHub PR review decision")
		}
		switch decision {
		case "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED":
			status.ReviewDecision = &decision
		default:
			return MergeStatus{}, errors.New("Unrecognized GitHub PR review decision")
		}
	}
	if len(p.Commits.Nodes) != 1 || !footprintRevision(p.Commits.Nodes[0].Commit.OID) || p.Commits.Nodes[0].Commit.OID != p.HeadOID {
		return MergeStatus{}, errors.New("GitHub merge status head does not match its check commit")
	}
	if rollup := p.Commits.Nodes[0].Commit.Rollup; rollup != nil {
		if rollup.Contexts == nil {
			return MergeStatus{}, errors.New("GitHub merge status check rollup is missing its context count")
		}
		switch rollup.State {
		case "SUCCESS", "ERROR", "FAILURE", "EXPECTED", "PENDING":
			state := rollup.State
			status.CheckState = &state
			status.CheckContexts = rollup.Contexts.TotalCount
		default:
			return MergeStatus{}, errors.New("Unrecognized GitHub PR check status")
		}
	}
	return status, nil
}

func RemoteFootprintWithin(additions, deletions, files uint64, saved, live config.Config) bool {
	return maintenanceRemoteFootprintWithin(additions, deletions, files, saved, live)
}

func MergeBase(ctx context.Context, c config.Config, base, head string) (string, error) {
	return Git(ctx, c, c.Repository, []string{"merge-base", base, head})
}

func SquashMerge(ctx context.Context, c config.Config, number uint64, head, headBranch string) (string, error) {
	if number == 0 {
		return "", errors.New("Merge requires a pull request number")
	}
	if c.DeliveryMode != config.DeliveryModeMaintenance {
		return "", errors.New("Merge is only permitted under maintenance delivery")
	}
	if !footprintRevision(head) {
		return "", errors.New("Merge requires the exact hexadecimal reviewed head")
	}
	if !config.ValidBranch(headBranch) || headBranch == c.DefaultBranch {
		return "", errors.New("Merge requires the reviewed source branch")
	}
	token, err := mergeDestinationToken(ctx, c)
	if err != nil {
		return "", err
	}
	out, err := mergeGH(ctx, c, token, []string{
		"api", "--hostname", "github.com", "--method", "PUT",
		fmt.Sprintf("repos/%s/pulls/%d/merge", c.GitHubRepo, number),
		"-f", "sha=" + head,
		"-f", "merge_method=squash",
	})
	if err != nil {
		return "", err
	}
	var response struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return "", fmt.Errorf("%w: acknowledgement is not readable: %w", ErrMergeUnconfirmed, err)
	}
	if !response.Merged || !footprintRevision(response.SHA) {
		return "", fmt.Errorf("%w: acknowledgement did not confirm a merge commit: %s", ErrMergeUnconfirmed, redact.Text(response.Message))
	}
	status, err := MaintenanceMergeStatus(ctx, c, number)
	if err != nil {
		return "", fmt.Errorf("%w: acknowledgement received but its destination is unconfirmed: %w", ErrMergeUnconfirmed, err)
	}
	if status.State != "merged" || status.Head != head || status.HeadBranch != headBranch ||
		status.BaseBranch != c.DefaultBranch || status.MergeCommit == nil || *status.MergeCommit != response.SHA {
		return "", fmt.Errorf("%w: acknowledgement does not match the reviewed pull request and destination", ErrMergeUnconfirmed)
	}
	return response.SHA, nil
}
