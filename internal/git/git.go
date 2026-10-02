package git

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

func blocked(reason model.BlockedReason, message string) error {
	return fmt.Errorf("%s: %w", message, reason)
}

func reasoned(reason model.BlockedReason, message string, err error) error {
	return fmt.Errorf("%s: %w: %w", message, reason, err)
}

func Git(ctx context.Context, c config.Config, cwd string, args []string) (string, error) {
	return git(ctx, c, cwd, args, isolatedConfig)
}

// remoteGit is Git for the few commands that may need an inherited credential helper: fetches, remote listing and
// pushes. They never apply work-tree attributes, so the ambient configuration cannot arm a repository-named driver.
func remoteGit(ctx context.Context, c config.Config, cwd string, args []string) (string, error) {
	return git(ctx, c, cwd, args, nil)
}

func git(ctx context.Context, c config.Config, cwd string, args []string, env []string) (string, error) {
	out, err := process.RunMachineEnv(ctx, "git", args, cwd, c.CommandTimeoutSeconds, env)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// isolatedConfig cuts every inherited configuration scope out of a git child: an owned work tree's .gitattributes
// may name any filter or diff driver the ambient configuration defines, and git would run it as the control plane.
// The trusted repo.git and the -c pins above still apply; remoteGit keeps ambient configuration only where a
// credential helper may live.
var isolatedConfig = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_GLOBAL=" + os.DevNull,
	"GIT_CONFIG_COUNT=0",
	"GIT_CONFIG_PARAMETERS=",
}

// hardened pins behaviour that configuration or submodules could otherwise turn into command execution. The trusted
// metadata is orchestrator-owned; these flags also cover legacy clones whose metadata sits inside the work tree.
var hardened = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.untrackedCache=false",
	"-c", "submodule.recurse=false",
	"-c", "diff.ignoreSubmodules=all",
	"-c", "status.submoduleSummary=false",
}

func treeArgs(workTree string, args []string) ([]string, error) {
	gitDir, err := workspace.GitDir(workTree)
	if err != nil {
		return nil, reasoned(model.BlockedReasonWorkspaceInvalid, "Workspace git metadata is unavailable", err)
	}
	full := append([]string{"--git-dir=" + gitDir, "--work-tree=" + workTree}, hardened...)
	return append(full, args...), nil
}

// WorkGit runs git on an owned work tree against its trusted metadata, never the work tree's own .git entry.
func WorkGit(ctx context.Context, c config.Config, workTree string, args []string) (string, error) {
	full, err := treeArgs(workTree, args)
	if err != nil {
		return "", err
	}
	return Git(ctx, c, workTree, full)
}

// DiffText runs git diff with args on an owned work tree's trusted metadata, for a reader rather than a parser. Git
// reads .gitattributes from its work tree and index, which a sandbox can write: an ignored file that ignores itself
// is never committed, yet `* -diff` in it shows every change as binary. So git gets an empty directory and an empty
// index instead, and attributes come only from the trusted metadata and the orchestrator's own configuration. --text
// shows even a file git takes for binary as text, and no external diff or textconv runs. Submodule entries show as
// the commits they point at: the command-line flag overrides both the hardened pin and a committed .gitmodules
// `ignore`. Renames show as a deletion and an addition, so each path stands for itself. The output is text as
// process.RunTextEnv returns it: at most limit bytes, and complete when that is all of it.
func DiffText(ctx context.Context, c config.Config, workTree string, args []string, limit int) (text string, complete bool, err error) {
	gitDir, err := workspace.GitDir(workTree)
	if err != nil {
		return "", false, reasoned(model.BlockedReasonWorkspaceInvalid, "Workspace git metadata is unavailable", err)
	}
	scratch, err := os.MkdirTemp("", "octomus-diff-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(scratch)
	empty := filepath.Join(scratch, "tree")
	if err := os.Mkdir(empty, 0o700); err != nil {
		return "", false, err
	}
	full := append([]string{"--git-dir=" + gitDir, "--work-tree=" + empty}, hardened...)
	full = append(full, "-c", "core.quotePath=true", "diff", "--no-color", "--no-ext-diff", "--no-textconv", "--text",
		"--ignore-submodules=none", "--submodule=short", "--no-renames")
	env := append(slices.Clone(isolatedConfig), "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
	return process.RunTextEnv(ctx, "git", append(full, args...), empty, c.CommandTimeoutSeconds, env, limit)
}

// remoteWorkGit is WorkGit for pushes: they may need an inherited credential helper, and they write no work tree
// and apply no attributes.
func remoteWorkGit(ctx context.Context, c config.Config, workTree string, args []string) (string, error) {
	full, err := treeArgs(workTree, args)
	if err != nil {
		return "", err
	}
	return remoteGit(ctx, c, workTree, full)
}

func gh(ctx context.Context, c config.Config, args []string) (string, error) {
	return process.RunMachine(ctx, "gh", args, c.Repository, c.CommandTimeoutSeconds)
}

func originURL(ctx context.Context, c config.Config, repo string) (string, error) {
	return Git(ctx, c, repo, []string{"remote", "get-url", "origin"})
}

func head(ctx context.Context, c config.Config, path string) (string, error) {
	return WorkGit(ctx, c, path, []string{"rev-parse", "HEAD"})
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

func ValidateRemote(ctx context.Context, c config.Config) error {
	_, err := validatedOrigin(ctx, c)
	return err
}

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
		return "", err
	}
	return remote, nil
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

// CloneAt makes an owned clone whose git metadata lives beside the work tree in repo.git, outside anything a sandbox
// can write, while the work tree keeps a .git pointer so tools inside it still find the history.
func CloneAt(ctx context.Context, c config.Config, path string, revision string) error {
	root := filepath.Dir(path)
	if root == path {
		return errors.New("Invalid workspace path")
	}
	gitDir := filepath.Join(root, workspace.GitDirName)
	for _, existing := range []string{path, gitDir} {
		if _, err := os.Lstat(existing); err == nil {
			return errors.New("Workspace already exists; recovery must inspect it")
		}
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return err
	}
	if _, err := Git(ctx, c, c.Repository, []string{
		"clone", "--no-hardlinks", "--no-checkout", "--separate-git-dir=" + gitDir, "--", c.Repository, path,
	}); err != nil {
		return err
	}
	if _, err := WorkGit(ctx, c, path, []string{"checkout", "--detach", revision}); err != nil {
		return err
	}
	remote, err := originURL(ctx, c, c.Repository)
	if err != nil {
		return err
	}
	if _, err := WorkGit(ctx, c, path, []string{"remote", "set-url", "origin", remote}); err != nil {
		return err
	}
	if _, err := WorkGit(ctx, c, path, []string{"config", "user.name", "Octomus Agent"}); err != nil {
		return err
	}
	if _, err := WorkGit(ctx, c, path, []string{
		"config", "user.email", "octomus-agent@users.noreply.github.com",
	}); err != nil {
		return err
	}
	// Task clones must never include application state in generated commits.
	return os.WriteFile(filepath.Join(gitDir, "info", "exclude"), []byte("/.octomus/\n"), 0o666)
}

// CloneReviewed makes a disposable clone of an owned clone at exactly one revision, in the same split layout, so
// verification sees the reviewed commit and nothing else a session left in the work tree: no ignored files, caches
// or build output. It keeps the source's local ignore rules and shares its immutable objects.
func CloneReviewed(ctx context.Context, c config.Config, source, path, revision string) error {
	sourceGitDir, err := workspace.GitDir(source)
	if err != nil {
		return reasoned(model.BlockedReasonWorkspaceInvalid, "Workspace git metadata is unavailable", err)
	}
	root := filepath.Dir(path)
	gitDir := filepath.Join(root, workspace.GitDirName)
	for _, existing := range []string{path, gitDir} {
		if _, err := os.Lstat(existing); err == nil {
			return errors.New("Verification checkout already exists")
		}
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return err
	}
	if _, err := Git(ctx, c, root, []string{
		"clone", "--quiet", "--no-checkout", "--separate-git-dir=" + gitDir, "--", sourceGitDir, path,
	}); err != nil {
		return err
	}
	if _, err := WorkGit(ctx, c, path, []string{"checkout", "--quiet", "--detach", revision}); err != nil {
		return err
	}
	exclude, err := os.ReadFile(filepath.Join(sourceGitDir, "info", "exclude"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(filepath.Join(gitDir, "info", "exclude"), exclude, 0o666)
}

func Snapshot(ctx context.Context, c config.Config, path string, message string) (string, error) {
	if _, err := WorkGit(ctx, c, path, []string{"add", "--all"}); err != nil {
		return "", err
	}
	if err := refuseGitlinks(ctx, c, path); err != nil {
		return "", err
	}
	changed, err := WorkGit(ctx, c, path, []string{"diff", "--cached", "--name-only", "--ignore-submodules=none"})
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
		return blocked(model.BlockedReasonWorkspaceInvalid, fmt.Sprintf(
			"Nested repository or changed submodule at %s; Octomus publishes only reviewed file content", strconv.Quote(redact.Text(name))))
	}
	return nil
}

func clean(ctx context.Context, c config.Config, path string) (bool, error) {
	out, err := WorkGit(ctx, c, path, []string{"status", "--porcelain", "--ignore-submodules=all"})
	if err != nil {
		return false, err
	}
	return out == "", nil
}

func IsAncestor(ctx context.Context, c config.Config, cwd string, ancestor string, descendant string) (bool, error) {
	return process.RunPredicateEnv(ctx, "git",
		[]string{"merge-base", "--is-ancestor", ancestor, descendant},
		cwd, c.CommandTimeoutSeconds, []int{1}, isolatedConfig)
}

func workIsAncestor(ctx context.Context, c config.Config, path string, ancestor string, descendant string) (bool, error) {
	args, err := treeArgs(path, []string{"merge-base", "--is-ancestor", ancestor, descendant})
	if err != nil {
		return false, err
	}
	return process.RunPredicateEnv(ctx, "git", args, path, c.CommandTimeoutSeconds, []int{1}, isolatedConfig)
}

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

func ParseInventory(out string, c config.Config) (model.OpenPrInventory, error) {
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

const taskMarkerPrefix = "<!-- octomus:task:"

func taskMarkerFor(taskID string) string {
	return taskMarkerPrefix + taskID + " -->"
}

func TaskMarker(ctx context.Context, c config.Config, taskID string, p model.PullRequest) (bool, error) {
	marker := taskMarkerFor(taskID)
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
		return nil, blocked(model.BlockedReasonRemoteConflict,
			"Ambiguous PR association; reconcile before publication")
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return &matches[0], nil
}

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

func Publish(ctx context.Context, task model.Task) (model.PullRequest, error) {
	pr, err := publishInner(ctx, task)
	if err != nil {
		if model.BlockedReasonFromError(err) != model.BlockedReasonUnknown {
			return pr, err
		}
		return pr, fmt.Errorf("%w: %w", model.BlockedReasonPublicationUncertain, err)
	}
	return pr, nil
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
	maxPublicationTitleChars = 256
	maxPublicationBodyChars  = 65536
)

type publicationMetadata struct {
	title string
	body  string
}

func preparePublication(task model.Task, existing *model.PullRequest, commit string) (publicationMetadata, error) {
	refuse := func(message string) (publicationMetadata, error) {
		return publicationMetadata{}, blocked(model.BlockedReasonWorkspaceInvalid, message)
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
		if utf8.RuneCountInString(title) > maxPublicationTitleChars {
			return refuse("Publication title exceeds the remote title limit")
		}
	}
	if utf8.RuneCountInString(body) > maxPublicationBodyChars {
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
		return model.PullRequest{}, blocked(model.BlockedReasonRemoteConflict,
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
	if err := ValidatePublication(task, published, marker, false); err != nil {
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
	number, err := parseCreatedPRURL(created, c.GitHubRepo)
	if err != nil {
		return model.PullRequest{}, err
	}
	published, err := PR(ctx, c, number)
	if err != nil {
		return model.PullRequest{}, err
	}
	marker := taskMarkerFor(task.ID)
	if err := ValidatePublication(task, published,
		strings.Contains(published.Body, marker), false); err != nil {
		return model.PullRequest{}, err
	}
	return published, nil
}

func parseCreatedPRURL(created string, repo string) (uint64, error) {
	trimmed := strings.TrimSpace(created)
	url, err := whatwg.Parse(trimmed)
	if err != nil {
		return 0, reasoned(model.BlockedReasonRemoteConflict,
			"PR creation returned no unambiguous URL; reconcile before retrying", err)
	}
	if url.Scheme() != "https" || url.Hostname() != "github.com" ||
		strings.ContainsAny(trimmed, "?#") {
		return 0, blocked(model.BlockedReasonRemoteConflict,
			"Invalid PR creation URL")
	}
	parts := strings.Split(strings.TrimPrefix(url.Pathname(), "/"), "/")
	if !(len(parts) == 4 && parts[2] == "pull" &&
		config.EqualASCII(parts[0]+"/"+parts[1], repo)) {
		return 0, blocked(model.BlockedReasonRemoteConflict,
			"Created PR belongs to a different repository")
	}
	number, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return 0, reasoned(model.BlockedReasonRemoteConflict,
			"Missing created PR number", err)
	}
	return number, nil
}

func publishInner(ctx context.Context, task model.Task) (model.PullRequest, error) {
	c := task.ExecutionConfig()
	fail := func(err error) (model.PullRequest, error) {
		return model.PullRequest{}, err
	}
	trustedRemote, err := validatedOrigin(ctx, c)
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
	for _, command := range c.VerificationCommands {
		if v := latestVerification(task, command); v == nil || !v.Success || v.Revision != commit {
			return fail(blocked(model.BlockedReasonWorkspaceInvalid,
				"Publication requires successful verification at the reviewed revision"))
		}
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
		p, err := PublicationPR(ctx, c, task.Branch)
		if err != nil {
			return fail(err)
		}
		existing = p
	}
	if existing != nil {
		marker, err := TaskMarker(ctx, c, task.ID, *existing)
		if err != nil {
			return fail(err)
		}
		if err := ValidatePublication(task, *existing, marker, true); err == nil {
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
		ancestor, err := workIsAncestor(ctx, c, path, task.SourceRevision, commit)
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
		// Hooks, tags and submodule recursion are pinned off so ambient configuration can never push anything but the owned branch.
		if _, err := remoteWorkGit(ctx, c, path, []string{
			"-c", "push.followTags=false",
			"push",
			"--recurse-submodules=no",
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
