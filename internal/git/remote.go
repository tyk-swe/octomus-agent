package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
)

func ValidateRemote(ctx context.Context, c config.Config) error {
	_, err := validatedOrigin(ctx, c)
	return err
}

// AuthError is a remote validation failure after the origin matched: gh is not signed in to GitHub or cannot reach it.
// It reads as gh's own error; the checkout itself is not at fault.
type AuthError struct{ Err error }

func (e AuthError) Error() string { return e.Err.Error() }
func (e AuthError) Unwrap() error { return e.Err }

// validatedOrigin checks that the trusted checkout's origin is the configured GitHub repository and that gh is
// signed in, and returns that origin URL: publication pushes to it by name, never to a work tree's own remote.
func validatedOrigin(ctx context.Context, c config.Config) (string, error) {
	remote, err := originURL(ctx, c, c.Repository)
	if err != nil {
		return "", err
	}
	repo, ok := strings.CutPrefix(remote, "git@github.com:")
	if !ok {
		repo, ok = strings.CutPrefix(remote, "https://github.com/")
	}
	if !ok {
		repo, ok = strings.CutPrefix(remote, "ssh://git@github.com/")
	}
	if !ok {
		return "", errors.New("Origin must use github.com via SSH or credential-free HTTPS")
	}
	for strings.HasSuffix(repo, ".git") {
		repo = strings.TrimSuffix(repo, ".git")
	}
	if !config.EqualASCII(repo, c.GitHubRepo) {
		return "", errors.New("Origin does not match configured GitHub repository")
	}
	if _, err := gh(ctx, c, []string{"auth", "status", "--hostname", "github.com"}); err != nil {
		return "", AuthError{err}
	}
	return remote, nil
}

func originURL(ctx context.Context, c config.Config, repo string) (string, error) {
	return Git(ctx, c, repo, []string{"remote", "get-url", "origin"})
}

func Fetch(ctx context.Context, c config.Config) error {
	_, err := remoteGit(ctx, c, c.Repository, []string{"fetch", "--prune", "origin"})
	return err
}

const forkHeadNamespace = "refs/octomus/pr/"

// FetchForkHeads copies fork PR heads into the trusted checkout, whose objects every clone inherits, so planning
// sandboxes can inspect them by SHA without any route to GitHub. Heads the remote no longer serves are reported, not
// fatal: grounding records them as unavailable context rather than failing the cycle.
func FetchForkHeads(ctx context.Context, c config.Config, numbers []uint64) ([]uint64, error) {
	stale, err := Git(ctx, c, c.Repository, []string{"for-each-ref", "--format=%(refname)", forkHeadNamespace})
	if err != nil {
		return nil, err
	}
	for _, ref := range strings.Fields(stale) {
		if _, err := Git(ctx, c, c.Repository, []string{"update-ref", "-d", ref}); err != nil {
			return nil, err
		}
	}
	if len(numbers) == 0 {
		return nil, nil
	}
	refspec := func(n uint64) string {
		return fmt.Sprintf("+refs/pull/%d/head:%s%d", n, forkHeadNamespace, n)
	}
	all := []string{"fetch", "--no-tags", "--no-write-fetch-head", "origin"}
	for _, n := range numbers {
		all = append(all, refspec(n))
	}
	if _, err := remoteGit(ctx, c, c.Repository, all); err == nil {
		return nil, nil
	}
	var missing []uint64
	for _, n := range numbers {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, err := remoteGit(ctx, c, c.Repository, []string{"fetch", "--no-tags", "--no-write-fetch-head", "origin", refspec(n)}); err != nil {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

func RemoteRevision(ctx context.Context, c config.Config, branch string) (*string, error) {
	if !config.ValidBranch(branch) {
		return nil, errors.New("Invalid branch")
	}
	want := "refs/heads/" + branch
	out, err := remoteGit(ctx, c, c.Repository, []string{
		"ls-remote", "--heads", "origin", want,
	})
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(line, "\t"); ok && ref == want {
			return &sha, nil
		}
	}
	return nil, nil
}

func Snapshot(ctx context.Context, c config.Config, path string, message string) (string, error) {
	if _, err := WorkGit(ctx, c, path, []string{"add", "--all"}); err != nil {
		return "", err
	}
	if err := refuseGitlinks(ctx, c, path); err != nil {
		return "", err
	}
	changed, err := WorkGit(ctx, c, path, []string{"diff", "--cached", "--name-only", "-z", "--ignore-submodules=none"})
	if err != nil {
		return "", err
	}
	if changed != "" {
		if _, err := WorkGit(ctx, c, path, []string{"commit", "-m", redact.Secrets(message)}); err != nil {
			return "", err
		}
	}
	return head(ctx, c, path)
}

const maxReportedPath = 200

// refuseGitlinks blocks staged submodule entries that are new or point elsewhere: a nested repository in the work tree
// would otherwise publish as a gitlink whose content was never reviewed. Removing a submodule stays allowed.
func refuseGitlinks(ctx context.Context, c config.Config, path string) error {
	out, err := WorkGit(ctx, c, path, []string{
		"diff", "--cached", "--raw", "-z", "--no-renames", "--ignore-submodules=none", "HEAD",
	})
	if err != nil {
		return err
	}
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		modes := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(modes) < 2 || modes[1] != "160000" {
			continue
		}
		name := fields[i+1]
		if len(name) > maxReportedPath {
			name = name[:maxReportedPath]
		}
		return blocked(model.BlockedWorkspaceInvalid, fmt.Sprintf(
			"Nested repository or changed submodule at %s; Octomus publishes only reviewed file content", strconv.Quote(redact.Text(name))))
	}
	return nil
}
