package git

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/workspace"
)

// splitClone clones source into path with the git metadata beside the work tree in repo.git, outside anything a
// sandbox can write, while the work tree keeps a .git pointer so tools inside it still find the history, and checks
// out revision detached. exists is the refusal when path or that metadata is already there; quiet silences both the
// clone and the checkout, and flags are further clone options. It returns the metadata directory.
func splitClone(ctx context.Context, c config.Config, cwd, source, path, revision, exists string, quiet bool, flags ...string) (string, error) {
	root := filepath.Dir(path)
	gitDir := filepath.Join(root, workspace.GitDirName)
	for _, existing := range []string{path, gitDir} {
		if _, err := os.Lstat(existing); err == nil {
			return "", errors.New(exists)
		}
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	clone, checkout := []string{"clone"}, []string{"checkout"}
	if quiet {
		clone, checkout = append(clone, "--quiet"), append(checkout, "--quiet")
	}
	clone = append(append(clone, flags...), "--no-checkout", "--separate-git-dir="+gitDir, "--", source, path)
	if _, err := Git(ctx, c, cwd, clone); err != nil {
		return "", err
	}
	if _, err := WorkGit(ctx, c, path, append(checkout, "--detach", revision)); err != nil {
		return "", err
	}
	return gitDir, nil
}

// CloneAt makes an owned clone of the trusted checkout at revision, in the split layout.
func CloneAt(ctx context.Context, c config.Config, path string, revision string) error {
	if filepath.Dir(path) == path {
		return errors.New("Invalid workspace path")
	}
	gitDir, err := splitClone(ctx, c, c.Repository, c.Repository, path, revision,
		"Workspace already exists; recovery must inspect it", false, "--no-hardlinks")
	if err != nil {
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
		return reasoned(model.BlockedWorkspaceInvalid, "Workspace git metadata is unavailable", err)
	}
	gitDir, err := splitClone(ctx, c, filepath.Dir(path), sourceGitDir, path, revision,
		"Verification checkout already exists", true)
	if err != nil {
		return err
	}
	exclude, err := os.ReadFile(filepath.Join(sourceGitDir, "info", "exclude"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(filepath.Join(gitDir, "info", "exclude"), exclude, 0o666)
}

// CloneRemote clones url into path as an ordinary checkout with the ambient git configuration, where a credential
// helper may live: the deployment's own trusted checkout, which every later command pins its own configuration on.
func CloneRemote(ctx context.Context, url, path string, seconds uint64) error {
	_, err := process.RunMachine(ctx, "git", []string{"clone", "--origin", "origin", url, path}, filepath.Dir(path), seconds, nil)
	return err
}
