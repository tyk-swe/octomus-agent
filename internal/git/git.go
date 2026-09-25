// Package git runs Git and GitHub operations as argument vectors,
// plus the publication safeguards that consume them. Every command goes through
// process.RunMachine, so output bounds, UTF-8 validation, timeouts and
// cancellation are the shared contract; no operation ever builds a shell
// command line.
package git

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	whatwg "github.com/nlnwa/whatwg-url/url"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// reasonContext displays its message before the typed reason and inner cause.
// Rendering joins the chain with ": ".
type reasonContext struct {
	msg    string
	reason model.BlockedReason
	err    error
	inner  bool // whether reason sits between msg and err rather than being err
}

func (e *reasonContext) Error() string {
	if e.inner {
		return e.msg + ": " + e.reason.Error() + ": " + e.err.Error()
	}
	return e.msg + ": " + e.err.Error()
}

func (e *reasonContext) Unwrap() []error {
	if e.inner {
		return []error{e.reason, e.err}
	}
	return []error{e.err}
}

// blocked attaches a typed reason as the innermost cause while keeping the
// detailed message outermost, so model.BlockedReasonFromError picks the reason.
func blocked(reason model.BlockedReason, message string) error {
	return &reasonContext{msg: message, err: reason}
}

// reasoned mirrors err.context(reason).context(message): the reason is a chain
// node above the original error.
func reasoned(reason model.BlockedReason, message string, err error) error {
	return &reasonContext{msg: message, reason: reason, err: err, inner: true}
}

// Git runs one git invocation as a machine capture; output is trimmed.
func Git(ctx context.Context, c config.Config, cwd string, args []string) (string, error) {
	out, err := process.RunMachine(ctx, "git", args, cwd, c.CommandTimeoutSeconds)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gh runs one gh invocation in the configured repository checkout.
func gh(ctx context.Context, c config.Config, args []string) (string, error) {
	return process.RunMachine(ctx, "gh", args, c.Repository, c.CommandTimeoutSeconds)
}

// originURL returns the origin URL of a clone of the configured repository.
func originURL(ctx context.Context, c config.Config, repo string) (string, error) {
	return Git(ctx, c, repo, []string{"remote", "get-url", "origin"})
}

// head returns the commit a checkout currently has checked out.
func head(ctx context.Context, c config.Config, path string) (string, error) {
	return Git(ctx, c, path, []string{"rev-parse", "HEAD"})
}

// ghPages decodes a `gh api --paginate` response: one JSON document per page.
// UseNumber preserves integer literals; non-integer values cannot be u64.
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

// field walks nested JSON objects; a miss or non-object yields nil.
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

// jstr accepts only JSON strings.
func jstr(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func text(p map[string]any, keys ...string) string {
	s, _ := jstr(field(p, keys...))
	return s
}

// jnum accepts only non-negative integer literals.
func jnum(v any) (uint64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	u, err := strconv.ParseUint(n.String(), 10, 64)
	return u, err == nil
}

// ValidateRemote requires the configured checkout's origin to name the
// configured GitHub repository over SSH or credential-free HTTPS, and gh to be
// authenticated against github.com.
func ValidateRemote(ctx context.Context, c config.Config) error {
	remote, err := originURL(ctx, c, c.Repository)
	if err != nil {
		return err
	}
	repo, ok := strings.CutPrefix(remote, "git@github.com:")
	if !ok {
		repo, ok = strings.CutPrefix(remote, "https://github.com/")
	}
	if !ok {
		repo, ok = strings.CutPrefix(remote, "ssh://git@github.com/")
	}
	if !ok {
		return errors.New("Origin must use github.com via SSH or credential-free HTTPS")
	}
	for strings.HasSuffix(repo, ".git") {
		repo = strings.TrimSuffix(repo, ".git")
	}
	if !config.EqualASCII(repo, c.GitHubRepo) {
		return errors.New("Origin does not match configured GitHub repository")
	}
	_, err = gh(ctx, c, []string{"auth", "status", "--hostname", "github.com"})
	return err
}

// Fetch refreshes the configured checkout's view of origin.
func Fetch(ctx context.Context, c config.Config) error {
	_, err := Git(ctx, c, c.Repository, []string{"fetch", "--prune", "origin"})
	return err
}

// RemoteRevision returns origin's head for branch, or nil when it is absent.
func RemoteRevision(ctx context.Context, c config.Config, branch string) (*string, error) {
	if !config.ValidBranch(branch) {
		return nil, errors.New("Invalid branch")
	}
	out, err := Git(ctx, c, c.Repository, []string{
		"ls-remote", "--heads", "origin", "refs/heads/" + branch,
	})
	if err != nil {
		return nil, err
	}
	if fields := strings.Fields(out); len(fields) > 0 {
		return &fields[0], nil
	}
	return nil, nil
}

// CloneAt creates an independent checkout at revision: no hardlinks, detached
// HEAD, the trusted origin URL, a deterministic committer identity, and an
// exclude rule that keeps application state out of generated commits.
func CloneAt(ctx context.Context, c config.Config, path string, revision string) error {
	if _, err := os.Stat(path); err == nil {
		return errors.New("Workspace already exists; recovery must inspect it")
	}
	if parent := filepath.Dir(path); parent == path {
		return errors.New("Invalid workspace path")
	} else if err := os.MkdirAll(parent, 0o777); err != nil {
		return err
	}
	if _, err := Git(ctx, c, c.Repository, []string{
		"clone", "--no-hardlinks", "--no-checkout", "--", c.Repository, path,
	}); err != nil {
		return err
	}
	if _, err := Git(ctx, c, path, []string{"checkout", "--detach", revision}); err != nil {
		return err
	}
	remote, err := originURL(ctx, c, c.Repository)
	if err != nil {
		return err
	}
	if _, err := Git(ctx, c, path, []string{"remote", "set-url", "origin", remote}); err != nil {
		return err
	}
	if _, err := Git(ctx, c, path, []string{"config", "user.name", "Octomus Agent"}); err != nil {
		return err
	}
	if _, err := Git(ctx, c, path, []string{
		"config", "user.email", "octomus-agent@users.noreply.github.com",
	}); err != nil {
		return err
	}
	// Task clones must never include application state in generated commits.
	return os.WriteFile(filepath.Join(path, ".git/info/exclude"), []byte("/.octomus/\n"), 0o666)
}

// Snapshot stages every workspace change, commits when anything changed, and
// returns the resulting HEAD.
func Snapshot(ctx context.Context, c config.Config, path string, message string) (string, error) {
	if _, err := Git(ctx, c, path, []string{"add", "--all"}); err != nil {
		return "", err
	}
	changed, err := Git(ctx, c, path, []string{"diff", "--cached", "--name-only"})
	if err != nil {
		return "", err
	}
	if changed != "" {
		if _, err := Git(ctx, c, path, []string{
			"-c", "core.hooksPath=/dev/null", "commit", "-m", message,
		}); err != nil {
			return "", err
		}
	}
	return head(ctx, c, path)
}

// clean reports whether the worktree has no pending changes.
func clean(ctx context.Context, c config.Config, path string) (bool, error) {
	out, err := Git(ctx, c, path, []string{"status", "--porcelain"})
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// IsAncestor reports whether ancestor is an ancestor of descendant.
// `merge-base --is-ancestor` reports a false predicate as exit 1; every other
// nonzero status is a real command failure and propagates instead of reading
// as false.
func IsAncestor(ctx context.Context, c config.Config, cwd string, ancestor string, descendant string) (bool, error) {
	return process.RunPredicate(ctx, "git",
		[]string{"merge-base", "--is-ancestor", ancestor, descendant},
		cwd, c.CommandTimeoutSeconds, []int{1})
}

// At reports whether the worktree is clean and HEAD is exactly revision.
func At(ctx context.Context, c config.Config, path string, revision string) (bool, error) {
	isClean, err := clean(ctx, c, path)
	if err != nil || !isClean {
		return isClean, err
	}
	actual, err := head(ctx, c, path)
	if err != nil {
		return false, err
	}
	return actual == revision, nil
}

// OpenPrInventory reads every open pull request, paginating the API so older
// open work is never quietly omitted.
func OpenPrInventory(ctx context.Context, c config.Config) (model.OpenPrInventory, error) {
	observedAt := model.Now()
	out, err := gh(ctx, c, []string{
		"api", "--paginate",
		fmt.Sprintf("repos/%s/pulls?state=open&per_page=100", c.GitHubRepo),
	})
	if err != nil {
		return model.OpenPrInventory{}, err
	}
	inventory, err := ParseInventory(out, c)
	if err != nil {
		return model.OpenPrInventory{}, err
	}
	inventory.ObservedAt = observedAt
	return inventory, nil
}

// ParseInventory decodes a paginated open-PR listing into a sorted, deduplicated
// inventory, rejecting unknown states, missing identity, foreign base
// repositories and conflicting duplicates.
func ParseInventory(out string, c config.Config) (model.OpenPrInventory, error) {
	prs := map[uint64]model.PullRequest{}
	pages := 0
	err := ghPages(out, func(page []map[string]any) error {
		pages++
		for _, p := range page {
			if state, _ := jstr(field(p, "state")); state != "open" && state != "closed" {
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
		return model.OpenPrInventory{}, err
	}
	if pages < 1 {
		return model.OpenPrInventory{}, errors.New("Open PR inventory response is empty")
	}
	ordered := make([]model.PullRequest, 0, len(prs))
	for _, pr := range prs {
		ordered = append(ordered, pr)
	}
	slices.SortFunc(ordered, func(a, b model.PullRequest) int {
		return cmp.Compare(a.Number, b.Number)
	})
	return model.OpenPrInventory{
		Repository: c.GitHubRepo,
		ObservedAt: model.Now(),
		PRs:        ordered,
	}, nil
}

// OwnedPrDetails re-reads every owned inventory entry: an entry that changed
// while the open inventory was being read fails rather than proceeding on
// stale identity.
func OwnedPrDetails(ctx context.Context, c config.Config, inventory model.OpenPrInventory) ([]model.PullRequest, error) {
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

// PR reads one pull request by number.
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

// prComments returns the bodies of a pull request's comments, paginated like
// every other list read.
func prComments(ctx context.Context, c config.Config, number uint64) ([]string, error) {
	out, err := gh(ctx, c, []string{
		"api", "--paginate",
		fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", c.GitHubRepo, number),
	})
	if err != nil {
		return nil, err
	}
	bodies := []string{}
	err = ghPages(out, func(page []map[string]any) error {
		for _, value := range page {
			bodies = append(bodies, text(value, "body"))
		}
		return nil
	})
	return bodies, err
}

// taskMarker reports whether the task's publication marker is attached to the
// pull request: in the description when this task originated the request, or
// in an append-only comment when it delivered a follow-up.
func taskMarker(ctx context.Context, c config.Config, taskID string, p model.PullRequest) (bool, error) {
	marker := "<!-- octomus:task:" + taskID + " -->"
	if strings.Contains(p.Body, marker) {
		return true, nil
	}
	bodies, err := prComments(ctx, c, p.Number)
	if err != nil {
		return false, err
	}
	for _, body := range bodies {
		if strings.Contains(body, marker) {
			return true, nil
		}
	}
	return false, nil
}

// TaskMarker reports whether a task's durable publication marker is present in
// a pull request description or comment.
func TaskMarker(ctx context.Context, c config.Config, taskID string, p model.PullRequest) (bool, error) {
	return taskMarker(ctx, c, taskID, p)
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
	headRepo, headRepoOk := jstr(field(p, "head", "repo", "full_name"))
	baseRepo, baseRepoOk := jstr(field(p, "base", "repo", "full_name"))
	owned := strings.HasPrefix(branch, c.BranchPrefix) &&
		headRepoOk && config.EqualASCII(headRepo, c.GitHubRepo) &&
		baseRepoOk && config.EqualASCII(baseRepo, c.GitHubRepo) &&
		strings.Contains(body, "<!-- octomus:task:")
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

// publicationPR finds the pull request associated with a branch, if any.
// Multiple candidates are ambiguous and must be reconciled before publication.
func publicationPR(ctx context.Context, c config.Config, branch string) (*model.PullRequest, error) {
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
		return nil, blocked(model.BlockedReasonRemoteConflict,
			"Ambiguous PR association; reconcile before publication")
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return &matches[0], nil
}

// PublicationPR finds the unique pull request associated with an admitted
// branch, including closed and merged requests.
func PublicationPR(ctx context.Context, c config.Config, branch string) (*model.PullRequest, error) {
	return publicationPR(ctx, c, branch)
}

// ValidatePublication checks a delivered or reconciled pull request against the
// task's recorded expectations. `marker` carries the caller's check of
// taskMarker: the marker may live in the description or in a follow-up comment,
// which this synchronous check cannot fetch for itself.
func ValidatePublication(task model.Task, p model.PullRequest, marker bool, reconcile bool) error {
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

// Publish delivers a reviewed commit. Failures that already carry a typed
// reason — deterministic refusals whose remedy is supersede, not reconcile —
// surface with their own message. Untyped failures, where the remote state is
// genuinely unknown, are reported as PublicationUncertain so reconciliation
// preserves the output.
func Publish(ctx context.Context, task model.Task) (model.PullRequest, error) {
	pr, err := publishInner(ctx, task)
	if err != nil {
		if model.BlockedReasonFromError(err) != model.BlockedReasonUnknown {
			return pr, err
		}
		return pr, reasoned(model.BlockedReasonPublicationUncertain,
			model.BlockedReasonPublicationUncertain.Error(), err)
	}
	return pr, nil
}

// prBody builds the pull request text for a reviewed commit. A task that
// already owns a pull request posts a follow-up comment rather than rewriting
// the description, so earlier delivery notes and any maintainer conversation
// are never replaced. The task marker makes that append idempotent: a
// republication of the same task adds nothing.
func prBody(task model.Task, existing *model.PullRequest, commit string) string {
	var verification []string
	for _, v := range task.Verification {
		if v.Revision != commit {
			continue
		}
		result := "failed"
		if v.Success {
			result = "passed"
		}
		verification = append(verification, fmt.Sprintf("- `%s`: %s", v.Command, result))
	}
	marker := fmt.Sprintf("<!-- octomus:task:%s -->", task.ID)
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

// Publication text must fit the remote's pull request fields without any
// shortening: the task marker sits at the tail of the body, so a truncated
// body would silently drop the identity reconciliation depends on. These
// ceilings mirror GitHub's accepted title and body sizes.
const (
	maxPublicationTitleChars = 256
	maxPublicationBodyChars  = 65536
)

// publicationMetadata is the public representation prepared for one delivery:
// the title a new pull request is created with and the complete description
// or append-only follow-up comment. Canonical task fields are never altered;
// only the outgoing copies are scrubbed.
type publicationMetadata struct {
	title string
	body  string
}

// preparePublication assembles and validates the public text for a delivery
// that is about to write. Proposal text, command descriptions and session
// summaries pass through the non-truncating secret scrubber — never the
// bounded operator-message formatter, whose length cap could cut the trailing
// marker — and the result is checked as a whole. Metadata that cannot satisfy
// both the public-text policy and the task's delivery identity is refused
// before any outbound write; refusal messages stay generic so the
// operator-facing record never echoes the private text that was rejected.
func preparePublication(task model.Task, existing *model.PullRequest, commit string) (publicationMetadata, error) {
	refuse := func(message string) (publicationMetadata, error) {
		return publicationMetadata{}, blocked(model.BlockedReasonWorkspaceInvalid, message)
	}
	title := store.RedactSecrets(task.Proposal.Title)
	body := store.RedactSecrets(prBody(task, existing, commit))
	// New PR titles are passed as process arguments, which cannot contain NUL;
	// keep all public metadata free of it so refusal happens before branch push.
	if strings.ContainsRune(title, '\x00') || strings.ContainsRune(body, '\x00') {
		return refuse("Publication metadata contains an unsupported character")
	}
	// The title becomes a real title field only for a new pull request; on a
	// follow-up it lives inside the comment body and is covered by the body
	// checks instead.
	if existing == nil {
		if strings.TrimSpace(title) == "" {
			return refuse("Publication title is empty after public-safe preparation")
		}
		if utf8.RuneCountInString(title) > maxPublicationTitleChars {
			return refuse("Publication title exceeds the remote title limit")
		}
	}
	if utf8.RuneCountInString(body) > maxPublicationBodyChars {
		return refuse("Publication body exceeds the remote size limit")
	}
	marker := fmt.Sprintf("<!-- octomus:task:%s -->", task.ID)
	if !strings.Contains(body, marker) {
		return refuse("Publication metadata cannot carry the task's delivery marker")
	}
	if !strings.Contains(body, "Reviewed commit: `"+commit+"`") {
		return refuse("Publication metadata cannot carry the reviewed commit")
	}
	return publicationMetadata{title: title, body: body}, nil
}

// updatePR attaches follow-up evidence to a pull request this task already
// owns. The remote is re-read immediately before the comment: another writer
// moving the head or closing the request between reconciliation and here means
// the delivery no longer matches what was reviewed, so it is refused and the
// retry reconciles against whatever is now there. The evidence itself is posted
// as a comment rather than rewritten into the description — the body is shared,
// maintainer-editable text with no conditional-replace API, so a check-then-
// edit could silently drop a concurrent maintainer edit while a comment can
// only ever append.
func updatePR(ctx context.Context, c config.Config, task model.Task, p model.PullRequest, commit string, bodyPath string) (model.PullRequest, error) {
	latest, err := PR(ctx, c, p.Number)
	if err != nil {
		return model.PullRequest{}, err
	}
	if !latest.OwnedOpen() || latest.Base != c.DefaultBranch || latest.Head != commit {
		return model.PullRequest{}, blocked(model.BlockedReasonRemoteConflict,
			"PR changed around publication; retry will reconcile the current remote state")
	}
	marker, err := taskMarker(ctx, c, task.ID, latest)
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
	// The marker is attached by construction: it was either already present or
	// posted by the comment above.
	if err := ValidatePublication(task, published, true, false); err != nil {
		return model.PullRequest{}, err
	}
	return published, nil
}

// createPR opens a new pull request and confirms what was actually created.
// `gh` reports success as a URL, which is parsed rather than trusted: a URL on
// another host, or naming another repository, means the request was not created
// where this task believes it was.
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
	trimmed := strings.TrimSpace(created)
	url, err := whatwg.Parse(trimmed)
	if err != nil {
		return model.PullRequest{}, reasoned(model.BlockedReasonRemoteConflict,
			"PR creation returned no unambiguous URL; reconcile before retrying", err)
	}
	// Query and fragment components make a creation URL ambiguous.
	if url.Scheme() != "https" || url.Hostname() != "github.com" ||
		strings.ContainsAny(trimmed, "?#") {
		return model.PullRequest{}, blocked(model.BlockedReasonRemoteConflict,
			"Invalid PR creation URL")
	}
	parts := strings.Split(strings.TrimPrefix(url.Pathname(), "/"), "/")
	if !(len(parts) == 4 && parts[2] == "pull" &&
		config.EqualASCII(parts[0]+"/"+parts[1], c.GitHubRepo)) {
		return model.PullRequest{}, blocked(model.BlockedReasonRemoteConflict,
			"Created PR belongs to a different repository")
	}
	number, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return model.PullRequest{}, reasoned(model.BlockedReasonRemoteConflict,
			"Missing created PR number", err)
	}
	published, err := PR(ctx, c, number)
	if err != nil {
		return model.PullRequest{}, err
	}
	marker := fmt.Sprintf("<!-- octomus:task:%s -->", task.ID)
	if err := ValidatePublication(task, published,
		strings.Contains(published.Body, marker), false); err != nil {
		return model.PullRequest{}, err
	}
	return published, nil
}

func publishInner(ctx context.Context, task model.Task) (model.PullRequest, error) {
	c := task.ExecutionConfig()
	fail := func(err error) (model.PullRequest, error) {
		return model.PullRequest{}, err
	}
	if err := ValidateRemote(ctx, c); err != nil {
		return fail(err)
	}
	trustedRemote, err := originURL(ctx, c, c.Repository)
	if err != nil {
		return fail(err)
	}
	path := task.Workspace
	if task.OutputCommit == nil {
		return fail(blocked(model.BlockedReasonWorkspaceInvalid, "No reviewed commit"))
	}
	commit := *task.OutputCommit
	if last := len(task.Reviews) - 1; last < 0 ||
		task.Reviews[last].Revision != commit || !task.Reviews[last].Result.Clean() {
		return fail(blocked(model.BlockedReasonWorkspaceInvalid,
			"Publication requires a clean review at the output revision"))
	}
	verified := true
	for _, cmd := range c.VerificationCommands {
		found := false
		for i := len(task.Verification) - 1; i >= 0; i-- {
			if v := task.Verification[i]; v.Command == cmd {
				found = v.Success && v.Revision == commit
				break
			}
		}
		if !found {
			verified = false
			break
		}
	}
	if !verified {
		return fail(blocked(model.BlockedReasonWorkspaceInvalid,
			"Publication requires successful verification at the reviewed revision"))
	}
	if !strings.HasPrefix(task.Branch, c.BranchPrefix) || task.Branch == c.DefaultBranch {
		return fail(blocked(model.BlockedReasonWorkspaceInvalid,
			"Cannot publish outside the owned branch namespace"))
	}
	if isClean, err := clean(ctx, c, path); err != nil {
		return fail(err)
	} else if !isClean {
		return fail(blocked(model.BlockedReasonWorkspaceInvalid,
			"Workspace changed after review"))
	}
	if actual, err := head(ctx, c, path); err != nil {
		return fail(err)
	} else if actual != commit {
		return fail(blocked(model.BlockedReasonWorkspaceInvalid,
			"Workspace HEAD changed after review"))
	}
	var existing *model.PullRequest
	if task.PRNumber != nil {
		p, err := PR(ctx, c, *task.PRNumber)
		if err != nil {
			return fail(err)
		}
		existing = &p
	} else {
		p, err := publicationPR(ctx, c, task.Branch)
		if err != nil {
			return fail(err)
		}
		existing = p
	}
	if existing != nil {
		marker, err := taskMarker(ctx, c, task.ID, *existing)
		if err != nil {
			return fail(err)
		}
		if err := ValidatePublication(task, *existing, marker, true); err == nil {
			// Delivery already happened, even if a maintainer has since closed
			// or merged the PR.
			return *existing, nil
		}
		if !existing.OwnedOpen() || existing.Branch != task.Branch || existing.Base != c.DefaultBranch {
			return fail(blocked(model.BlockedReasonRemoteConflict,
				"PR ownership, base, or open state changed; reconcile before retrying"))
		}
		if task.PRNumber == nil && !marker {
			return fail(blocked(model.BlockedReasonRemoteConflict,
				"Branch is already associated with another task"))
		}
	}
	// The public representation is assembled and validated before the first
	// new outbound write: a refused title or body never reaches the push below
	// or the remote. Already-delivered reconciliation returned above, so this
	// never gates the read-only path.
	meta, err := preparePublication(task, existing, commit)
	if err != nil {
		return fail(err)
	}
	remote, err := RemoteRevision(ctx, c, task.Branch)
	if err != nil {
		return fail(err)
	}
	defaultRevision, err := RemoteRevision(ctx, c, c.DefaultBranch)
	if err != nil {
		return fail(err)
	}
	if defaultRevision == nil || *defaultRevision != task.DefaultRevision {
		return fail(model.BlockedReasonStaleBase)
	}
	if remote == nil || *remote != commit {
		if task.PRNumber != nil {
			if remote == nil || *remote != task.SourceRevision {
				return fail(model.BlockedReasonRemoteConflict)
			}
		} else if remote != nil {
			return fail(model.BlockedReasonRemoteConflict)
		}
		// An exact lease protects the check/push race. The local ancestry must
		// also be preserved.
		ancestor, err := IsAncestor(ctx, c, path, task.SourceRevision, commit)
		if err != nil {
			return fail(err)
		}
		if !ancestor {
			return fail(blocked(model.BlockedReasonRemoteConflict,
				"Reviewed output does not contain the recorded source; reconcile the branch"))
		}
		expected := ""
		if remote != nil {
			expected = *remote
		}
		if _, err := Git(ctx, c, path, []string{
			"-c", "core.hooksPath=/dev/null",
			"-c", "push.followTags=false",
			"push",
			fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", task.Branch, expected),
			trustedRemote,
			fmt.Sprintf("%s:refs/heads/%s", commit, task.Branch),
		}); err != nil {
			return fail(err)
		}
	}
	bodyPath := filepath.Join(filepath.Dir(path), "pr-body.md")
	if err := os.WriteFile(bodyPath, []byte(meta.body), 0o666); err != nil {
		return fail(err)
	}
	if existing != nil {
		return updatePR(ctx, c, task, *existing, commit, bodyPath)
	}
	return createPR(ctx, c, task, meta.title, bodyPath)
}
