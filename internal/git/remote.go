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

// Fetch updates the trusted checkout from its origin. It is a shared-checkout writer: see checkoutGate.
func Fetch(ctx context.Context, c config.Config) error {
	return withCheckout(ctx, c, true, func() error {
		_, err := remoteGit(ctx, c, c.Repository, fetchArgs("fetch", "--prune", "origin"))
		return err
	})
}

// fetchArgs keeps automatic maintenance inside the checkout gate. Git otherwise detaches maintenance after
// fetch exits, allowing a clone to race the object cleanup after the writer has released the gate.
func fetchArgs(args ...string) []string {
	return append([]string{"-c", "maintenance.autoDetach=false", "-c", "gc.autoDetach=false"}, args...)
}

const forkHeadNamespace = "refs/octomus/pr/"

// FetchForkHeads copies fork PR heads into the trusted checkout, whose objects every clone inherits, so planning
// sandboxes can inspect them by SHA without any route to GitHub. Heads the remote no longer serves are reported, not
// fatal: grounding records them as unavailable context rather than failing the cycle. It holds the trusted checkout
// for writing for the whole replacement: deleting the stale refs and fetching the current ones are separate commands,
// and another shared-checkout operation must not interleave with that sequence (see checkoutGate).
func FetchForkHeads(ctx context.Context, c config.Config, numbers []uint64) ([]uint64, error) {
	var missing []uint64
	if err := withCheckout(ctx, c, true, func() error {
		var err error
		missing, err = replaceForkHeads(ctx, c, numbers)
		return err
	}); err != nil {
		return nil, err
	}
	return missing, nil
}

func replaceForkHeads(ctx context.Context, c config.Config, numbers []uint64) ([]uint64, error) {
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
	// Both ls-remote's patterns and fetch's named refspecs can match a branch with the same suffix. Bind each
	// request to its exact advertised PR ref, then fetch that object ID so a branch alias or a moving ref cannot
	// silently supply a different head.
	wanted := make(map[string]uint64, len(numbers))
	lookup := []string{"ls-remote", "--refs", "origin"}
	for _, n := range numbers {
		ref := fmt.Sprintf("refs/pull/%d/head", n)
		wanted[ref] = n
		lookup = append(lookup, ref)
	}
	out, err := remoteGit(ctx, c, c.Repository, lookup)
	if err != nil {
		return nil, err
	}
	heads := make(map[uint64]string, len(numbers))
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		n, requested := wanted[ref]
		if !ok || !requested {
			continue
		}
		if !validObjectID(sha) {
			return nil, fmt.Errorf("Remote fork head %d returned an invalid object ID", n)
		}
		heads[n] = sha
	}
	var missing []uint64
	all := fetchArgs("fetch", "--no-tags", "--no-write-fetch-head", "origin")
	for _, n := range numbers {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		sha, advertised := heads[n]
		if !advertised {
			missing = append(missing, n)
			continue
		}
		all = append(all, fmt.Sprintf("+%s:%s%d", sha, forkHeadNamespace, n))
	}
	if len(heads) > 0 {
		if _, err := remoteGit(ctx, c, c.Repository, all); err != nil {
			return nil, err
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

// reportedPathCut marks a path this diagnostic shortened.
const reportedPathCut = "…"

// reportedPath prepares a work-tree path for a diagnostic message. Secret recognition runs on the complete path first:
// a limit applied beforehand would cut a credential in half and leave a fragment no pattern or configured value can
// match. Only then is the display bounded, counting characters so a cut never lands inside one. Bytes that are not
// valid UTF-8 are left alone, so the quoting caller can still escape them explicitly.
func reportedPath(name string) string {
	redacted := redact.Text(name)
	count := 0
	for i := range redacted {
		if count == maxReportedPath {
			return redacted[:i] + reportedPathCut
		}
		count++
	}
	return redacted
}

// refuseGitlinks blocks staged submodule entries that are new or point elsewhere: a nested repository in the work tree
// would otherwise publish as a gitlink whose content was never reviewed. Removing a submodule stays allowed.
func refuseGitlinks(ctx context.Context, c config.Config, path string) error {
	out, err := WorkGit(ctx, c, path, []string{
		"diff", "--cached", "--raw", "-z", "--no-renames", "--ignore-submodules=none", "HEAD",
	})
	if err != nil {
		return err
	}
	return refuseGitlinkEntries(out)
}

// RefuseCommittedGitlinks applies the same refusal to the commits a work tree carries rather than to its staged
// changes. A runner that commits its own work — Codex and OpenCode both do — leaves nothing staged, so the snapshot
// never sees the nested repository it committed, and the gitlink would reach review, verification and publication
// with content that was never reviewed. base is the revision the reviewed change set is measured from, so a submodule
// the base already has stays allowed while the range leaves it untouched, exactly as a staged entry does.
func RefuseCommittedGitlinks(ctx context.Context, c config.Config, path, base, revision string) error {
	if base == "" || revision == "" || base == revision {
		return nil
	}
	out, err := WorkGit(ctx, c, path, []string{
		"diff", "--raw", "-z", "--no-renames", "--ignore-submodules=none", base, revision,
	})
	if err != nil {
		return err
	}
	return refuseGitlinkEntries(out)
}

// refuseGitlinkEntries refuses the first raw-diff entry that adds or retargets a gitlink. Both diff forms report
// <old mode> <new mode> per entry, so a removal — whose new mode is 000000 — stays allowed.
func refuseGitlinkEntries(out string) error {
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		modes := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(modes) < 2 || modes[1] != "160000" {
			continue
		}
		return blocked(model.BlockedWorkspaceInvalid, fmt.Sprintf(
			"Nested repository or changed submodule at %s; Octomus publishes only reviewed file content", strconv.Quote(reportedPath(fields[i+1]))))
	}
	return nil
}
