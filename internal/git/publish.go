package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	whatwg "github.com/nlnwa/whatwg-url/url"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
)

// Publish pushes the task's reviewed commit to its owned branch on the trusted remote and creates or updates its PR.
// Any failure without a known blocked reason is a publication_uncertain one: the remote may have changed.
func Publish(ctx context.Context, task model.Task) (pr model.PullRequest, err error) {
	defer func() {
		if err != nil && model.BlockedReasonFromError(err) == model.BlockedUnknown {
			err = fmt.Errorf("%w: %w", model.BlockedPublicationUncertain, err)
		}
	}()
	c := task.ExecutionConfig()
	trustedRemote, err := validatedOrigin(ctx, c)
	if err != nil {
		return model.PullRequest{}, err
	}
	path := task.Workspace
	if task.OutputCommit == nil {
		return model.PullRequest{}, blocked(model.BlockedWorkspaceInvalid, "No reviewed commit")
	}
	commit := *task.OutputCommit
	if last := len(task.Reviews) - 1; last < 0 ||
		task.Reviews[last].Revision != commit || !task.Reviews[last].Result.Clean() {
		return model.PullRequest{}, blocked(model.BlockedWorkspaceInvalid,
			"Publication requires a clean review at the output revision")
	}
	if !task.ReviewAuthorizes(commit) {
		return model.PullRequest{}, blocked(model.BlockedWorkspaceInvalid,
			"Publication requires a clean maintenance-qualified review at the output revision")
	}
	for _, command := range c.VerificationCommands {
		if v := latestVerification(task, command); v == nil || !v.Success || v.Revision != commit {
			return model.PullRequest{}, blocked(model.BlockedWorkspaceInvalid,
				"Publication requires successful verification at the reviewed revision")
		}
	}
	if !strings.HasPrefix(task.Branch, c.BranchPrefix) || task.Branch == c.DefaultBranch {
		return model.PullRequest{}, blocked(model.BlockedWorkspaceInvalid,
			"Cannot publish outside the owned branch namespace")
	}
	if isClean, err := clean(ctx, c, path); err != nil {
		return model.PullRequest{}, err
	} else if !isClean {
		return model.PullRequest{}, blocked(model.BlockedWorkspaceInvalid,
			"Workspace changed after review")
	}
	if actual, err := head(ctx, c, path); err != nil {
		return model.PullRequest{}, err
	} else if actual != commit {
		return model.PullRequest{}, blocked(model.BlockedWorkspaceInvalid,
			"Workspace HEAD changed after review")
	}
	var existing *model.PullRequest
	if task.PRNumber != nil {
		p, err := PR(ctx, c, *task.PRNumber)
		if err != nil {
			return model.PullRequest{}, err
		}
		existing = &p
	} else {
		p, err := PublicationPR(ctx, c, task.Branch)
		if err != nil {
			return model.PullRequest{}, err
		}
		existing = p
	}
	if existing != nil {
		marker, err := TaskMarker(ctx, c, task.ID, *existing)
		if err != nil {
			return model.PullRequest{}, err
		}
		if err := validatePublication(task, *existing, marker, true); err == nil {
			return *existing, nil
		}
		// A delivery marker records a completed write. Even a reset to the
		// original source must not authorize replaying that task's push.
		if marker && existing.Head != commit {
			return model.PullRequest{}, blocked(model.BlockedRemoteConflict,
				"Delivered PR head changed; reconcile before retrying")
		}
		if !existing.OwnedOpen() || existing.Branch != task.Branch || existing.Base != c.DefaultBranch {
			return model.PullRequest{}, blocked(model.BlockedRemoteConflict,
				"PR ownership, base, or open state changed; reconcile before retrying")
		}
		if task.PRNumber == nil && !marker {
			return model.PullRequest{}, blocked(model.BlockedRemoteConflict,
				"Branch is already associated with another task")
		}
	}
	meta, err := preparePublication(task, existing, commit)
	if err != nil {
		return model.PullRequest{}, err
	}
	remote, err := RemoteRevision(ctx, c, task.Branch)
	if err != nil {
		return model.PullRequest{}, err
	}
	defaultRevision, err := RemoteRevision(ctx, c, c.DefaultBranch)
	if err != nil {
		return model.PullRequest{}, err
	}
	if defaultRevision == nil || *defaultRevision != task.DefaultRevision {
		return model.PullRequest{}, model.BlockedStaleBase
	}
	if remote == nil || *remote != commit {
		if task.PRNumber != nil {
			if remote == nil || *remote != task.SourceRevision {
				return model.PullRequest{}, model.BlockedRemoteConflict
			}
		} else if remote != nil {
			return model.PullRequest{}, model.BlockedRemoteConflict
		}
		ancestor, err := workIsAncestor(ctx, c, path, task.SourceRevision, commit)
		if err != nil {
			return model.PullRequest{}, err
		}
		if !ancestor {
			return model.PullRequest{}, blocked(model.BlockedRemoteConflict,
				"Reviewed output does not contain the recorded source; reconcile the branch")
		}
		expected := ""
		if remote != nil {
			expected = *remote
		}
		// Hooks, tags and submodule recursion are pinned off so ambient configuration can never push anything but the owned branch.
		if _, err := pushGit(ctx, c, path, []string{
			"-c", "push.followTags=false",
			"push",
			"--recurse-submodules=no",
			fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", task.Branch, expected),
			trustedRemote,
			fmt.Sprintf("%s:refs/heads/%s", commit, task.Branch),
		}); err != nil {
			return model.PullRequest{}, err
		}
	}
	bodyPath := filepath.Join(filepath.Dir(path), "pr-body.md")
	if err := os.WriteFile(bodyPath, []byte(meta.body), 0o666); err != nil {
		return model.PullRequest{}, err
	}
	if existing != nil {
		return updatePR(ctx, c, task, *existing, commit, bodyPath)
	}
	return createPR(ctx, c, task, meta.title, bodyPath)
}

func validatePublication(task model.Task, p model.PullRequest, marker bool, reconcile bool) error {
	c := task.Config
	if !(config.EqualASCII(p.HeadRepository, c.GitHubRepo) &&
		config.EqualASCII(p.BaseRepository, c.GitHubRepo) &&
		p.Owned &&
		p.Branch == task.Branch &&
		p.Base == c.DefaultBranch &&
		task.OutputCommit != nil && p.Head == *task.OutputCommit &&
		marker &&
		(p.State == "open" ||
			(reconcile && (p.State == "closed" || p.State == "merged")))) {
		return errors.New("PR publication result does not match repository, ownership, branch, base, reviewed head, task marker or state")
	}
	return nil
}

func latestVerification(task model.Task, command string) *model.Verification {
	for i := len(task.Verification) - 1; i >= 0; i-- {
		if task.Verification[i].Command == command {
			return &task.Verification[i]
		}
	}
	return nil
}

func prBody(task model.Task, existing *model.PullRequest, commit string) string {
	var verification []string
	commands := task.ExecutionConfig().VerificationCommands
	for i, command := range commands {
		if slices.Contains(commands[:i], command) {
			continue
		}
		v := latestVerification(task, command)
		if v == nil || v.Revision != commit {
			continue
		}
		result := "failed"
		if v.Success {
			result = "passed"
		}
		verification = append(verification, fmt.Sprintf("- `%s`: %s", v.Command, result))
	}
	marker := taskMarkerFor(task.ID)
	summary := ""
	for i := len(task.Sessions) - 1; i >= 0; i-- {
		if role := task.Sessions[i].Role; role == "executor" || role == "repair" {
			summary = task.Sessions[i].Summary
			break
		}
	}
	update := fmt.Sprintf(
		"%s\n\n%s\n\nScope: %s\n\nVerification\n%s\n\n%s\n\nReviewed commit: `%s`. %d review round(s).\n\n%s",
		task.Proposal.Problem,
		task.Proposal.Benefit,
		task.Proposal.Scope,
		strings.Join(verification, "\n"),
		summary,
		commit,
		len(task.Reviews),
		marker)
	if existing != nil {
		return fmt.Sprintf("Octomus follow-up: %s\n\n%s", task.Proposal.Title, update)
	}
	return update
}

const (
	maxTitle = 256
	maxBody  = 65536
)

type publicationMetadata struct {
	title string
	body  string
}

func preparePublication(task model.Task, existing *model.PullRequest, commit string) (publicationMetadata, error) {
	refuse := func(message string) (publicationMetadata, error) {
		return publicationMetadata{}, blocked(model.BlockedWorkspaceInvalid, message)
	}
	title := redact.Secrets(task.Proposal.Title)
	body := redact.Secrets(prBody(task, existing, commit))
	if strings.ContainsRune(title, '\x00') || strings.ContainsRune(body, '\x00') {
		return refuse("Publication metadata contains an unsupported character")
	}
	if existing == nil {
		if strings.TrimSpace(title) == "" {
			return refuse("Publication title is empty after public-safe preparation")
		}
		if utf8.RuneCountInString(title) > maxTitle {
			return refuse("Publication title exceeds the remote title limit")
		}
	}
	if utf8.RuneCountInString(body) > maxBody {
		return refuse("Publication body exceeds the remote size limit")
	}
	marker := taskMarkerFor(task.ID)
	if !strings.Contains(body, marker) {
		return refuse("Publication metadata cannot carry the task's delivery marker")
	}
	if !strings.Contains(body, "Reviewed commit: `"+commit+"`") {
		return refuse("Publication metadata cannot carry the reviewed commit")
	}
	return publicationMetadata{title: title, body: body}, nil
}

func updatePR(ctx context.Context, c config.Config, task model.Task, p model.PullRequest, commit string, bodyPath string) (model.PullRequest, error) {
	latest, err := PR(ctx, c, p.Number)
	if err != nil {
		return model.PullRequest{}, err
	}
	if !latest.OwnedOpen() || latest.Base != c.DefaultBranch || latest.Head != commit {
		return model.PullRequest{}, blocked(model.BlockedRemoteConflict,
			"PR changed around publication; retry will reconcile the current remote state")
	}
	marker, err := TaskMarker(ctx, c, task.ID, latest)
	if err != nil {
		return model.PullRequest{}, err
	}
	if !marker {
		if _, err := gh(ctx, c, []string{
			"pr", "comment",
			strconv.FormatUint(p.Number, 10),
			"--repo", c.GitHubRepo,
			"--body-file", bodyPath,
		}); err != nil {
			return model.PullRequest{}, err
		}
	}
	published, err := PR(ctx, c, p.Number)
	if err != nil {
		return model.PullRequest{}, err
	}
	marker, err = TaskMarker(ctx, c, task.ID, published)
	if err != nil {
		return model.PullRequest{}, err
	}
	if err := validatePublication(task, published, marker, false); err != nil {
		return model.PullRequest{}, err
	}
	return published, nil
}

func createPR(ctx context.Context, c config.Config, task model.Task, title string, bodyPath string) (model.PullRequest, error) {
	created, err := gh(ctx, c, []string{
		"pr", "create",
		"--repo", c.GitHubRepo,
		"--head", task.Branch,
		"--base", c.DefaultBranch,
		"--title", title,
		"--body-file", bodyPath,
	})
	if err != nil {
		return model.PullRequest{}, err
	}
	number, err := prNumber(created, c.GitHubRepo)
	if err != nil {
		return model.PullRequest{}, err
	}
	published, err := PR(ctx, c, number)
	if err != nil {
		return model.PullRequest{}, err
	}
	marker := taskMarkerFor(task.ID)
	if err := validatePublication(task, published,
		strings.Contains(published.Body, marker), false); err != nil {
		return model.PullRequest{}, err
	}
	return published, nil
}

func prNumber(created string, repo string) (uint64, error) {
	trimmed := strings.TrimSpace(created)
	url, err := whatwg.Parse(trimmed)
	if err != nil {
		return 0, reasoned(model.BlockedRemoteConflict,
			"PR creation returned no unambiguous URL; reconcile before retrying", err)
	}
	if url.Scheme() != "https" || url.Hostname() != "github.com" ||
		strings.ContainsAny(trimmed, "?#") {
		return 0, blocked(model.BlockedRemoteConflict,
			"Invalid PR creation URL")
	}
	parts := strings.Split(strings.TrimPrefix(url.Pathname(), "/"), "/")
	if !(len(parts) == 4 && parts[2] == "pull" &&
		config.EqualASCII(parts[0]+"/"+parts[1], repo)) {
		return 0, blocked(model.BlockedRemoteConflict,
			"Created PR belongs to a different repository")
	}
	number, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return 0, reasoned(model.BlockedRemoteConflict,
			"Missing created PR number", err)
	}
	return number, nil
}
