package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
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
	out, err := process.RunMachine(ctx, "git", args, cwd, c.CommandTimeoutSeconds, env)
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
	return metadataArgs(workTree, workTree, args)
}

// metadataArgs points git at workTree's trusted metadata, with tree as the work tree it reads.
func metadataArgs(workTree, tree string, args []string) ([]string, error) {
	gitDir, err := workspace.GitDir(workTree)
	if err != nil {
		return nil, reasoned(model.BlockedWorkspaceInvalid, "Workspace git metadata is unavailable", err)
	}
	full := append([]string{"--git-dir=" + gitDir, "--work-tree=" + tree}, hardened...)
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
// process.RunText returns it: at most limit bytes, and complete when that is all of it.
func DiffText(ctx context.Context, c config.Config, workTree string, args []string, limit int) (text string, complete bool, err error) {
	scratch, err := os.MkdirTemp("", "octomus-diff-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(scratch)
	empty := filepath.Join(scratch, "tree")
	if err := os.Mkdir(empty, 0o700); err != nil {
		return "", false, err
	}
	full, err := metadataArgs(workTree, empty, []string{"-c", "core.quotePath=true", "diff", "--no-color", "--no-ext-diff",
		"--no-textconv", "--text", "--ignore-submodules=none", "--submodule=short", "--no-renames"})
	if err != nil {
		return "", false, err
	}
	env := append(slices.Clone(isolatedConfig), "GIT_INDEX_FILE="+filepath.Join(scratch, "index"))
	return process.RunText(ctx, "git", append(full, args...), empty, c.CommandTimeoutSeconds, env, limit)
}

// pushGit is WorkGit for pushes: they may need an inherited credential helper, and they write no work tree
// and apply no attributes.
func pushGit(ctx context.Context, c config.Config, workTree string, args []string) (string, error) {
	full, err := treeArgs(workTree, args)
	if err != nil {
		return "", err
	}
	return remoteGit(ctx, c, workTree, full)
}

func head(ctx context.Context, c config.Config, path string) (string, error) {
	return WorkGit(ctx, c, path, []string{"rev-parse", "HEAD"})
}

func clean(ctx context.Context, c config.Config, path string) (bool, error) {
	out, err := WorkGit(ctx, c, path, []string{"status", "--porcelain", "--ignore-submodules=all"})
	if err != nil {
		return false, err
	}
	return out == "", nil
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

func IsAncestor(ctx context.Context, c config.Config, cwd string, ancestor string, descendant string) (bool, error) {
	return isAncestor(ctx, c, cwd, []string{"merge-base", "--is-ancestor", ancestor, descendant})
}

// workIsAncestor is IsAncestor on an owned work tree's trusted metadata, never the work tree's own .git entry.
func workIsAncestor(ctx context.Context, c config.Config, path string, ancestor string, descendant string) (bool, error) {
	args, err := treeArgs(path, []string{"merge-base", "--is-ancestor", ancestor, descendant})
	if err != nil {
		return false, err
	}
	return isAncestor(ctx, c, path, args)
}

func isAncestor(ctx context.Context, c config.Config, cwd string, args []string) (bool, error) {
	return process.RunPredicate(ctx, "git", args, cwd, c.CommandTimeoutSeconds, []int{1}, isolatedConfig)
}
