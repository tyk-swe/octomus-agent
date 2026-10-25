package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// githubTokenEnv is where the Docker deployment's GitHub token lives inside the control plane, for gh and the git
// credential helper. Sandboxes never see the control plane's environment.
const githubTokenEnv = "GH_TOKEN"

const maxSecretFileBytes = 64 << 10

// secretFiles lets each secret the service reads from its environment arrive as a file instead, the way Docker
// secrets do, which keeps it out of `docker inspect`.
var secretFiles = []struct{ variable, file string }{
	{config.TokenEnv, "OCTOMUS_TOKEN_FILE"},
	{config.WebhookEnv, "OCTOMUS_NOTIFICATION_WEBHOOK_URL_FILE"},
	{githubTokenEnv, "OCTOMUS_GITHUB_TOKEN_FILE"},
	{config.MergeTokenEnv, "OCTOMUS_MERGE_TOKEN_FILE"},
	{config.SlackAppTokenEnv, "OCTOMUS_SLACK_APP_TOKEN_FILE"},
	{config.SlackBotTokenEnv, "OCTOMUS_SLACK_BOT_TOKEN_FILE"},
	{config.DiscordBotTokenEnv, "OCTOMUS_DISCORD_BOT_TOKEN_FILE"},
}

// loadSecretFiles moves file-held secrets into the process environment before anything reads it, so redaction and
// the orchestrator's own git and gh children see them exactly as if they had been set directly.
func loadSecretFiles(env func(string) (string, bool), setenv func(string, string) error) (func(string) (string, bool), error) {
	loaded := map[string]string{}
	for _, secret := range secretFiles {
		path, ok := env(secret.file)
		if !ok || path == "" {
			continue
		}
		if _, set := env(secret.variable); set {
			return nil, fmt.Errorf("Set %s or %s, not both", secret.variable, secret.file)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("Reading %s: %w", secret.file, err)
		}
		data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("Reading %s: %w", secret.file, err)
		}
		if len(data) > maxSecretFileBytes {
			return nil, fmt.Errorf("%s exceeds the 64 KiB size limit", secret.file)
		}
		value := strings.TrimRight(string(data), "\r\n")
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("%s must hold one non-empty line", secret.file)
		}
		if err := setenv(secret.variable, value); err != nil {
			return nil, err
		}
		loaded[secret.variable] = value
	}
	if err := loadRepoTokensDir(env, setenv, loaded); err != nil {
		return nil, err
	}
	return func(key string) (string, bool) {
		if value, ok := loaded[key]; ok {
			return value, true
		}
		return env(key)
	}, nil
}

// loadRepoTokensDir loads per-repository GitHub tokens from the directory in
// OCTOMUS_GITHUB_TOKENS_DIR: one file per repository, named by its slug
// ("owner%2Fname"), holding one token line. Each becomes the repository's
// OCTOMUS_GITHUB_TOKEN__OWNER_2FNAME variable. A variable already set directly
// wins and is left alone; the directory is optional.
func loadRepoTokensDir(env func(string) (string, bool), setenv func(string, string) error, loaded map[string]string) error {
	dir, ok := env(config.GitHubTokensDirEnv)
	if !ok || dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("Reading %s: %w", config.GitHubTokensDirEnv, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		repo, err := url.PathUnescape(entry.Name())
		if err != nil || !config.ValidGitHubRepo(repo) || store.RepositorySlug(repo) != entry.Name() {
			return fmt.Errorf("Token filename %q must be a lowercase URL-escaped repository identity (owner%%2Fname)", entry.Name())
		}
		variable := config.RepoGitHubTokenEnv(repo)
		if _, set := env(variable); set {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("Reading %s: %w", path, err)
		}
		if len(data) > maxSecretFileBytes {
			return fmt.Errorf("%s exceeds the 64 KiB size limit", path)
		}
		value := strings.TrimRight(string(data), "\r\n")
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s must hold one non-empty line", path)
		}
		if err := setenv(variable, value); err != nil {
			return err
		}
		loaded[variable] = value
	}
	return nil
}

// gitCredential answers git's credential protocol for https://github.com only, from the deployment's token. It is
// configured as the control plane's credential helper; sandboxes have neither the helper nor the token.
func gitCredential(args []string, stdin io.Reader, stdout io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	if args[0] != "get" {
		_, _ = io.Copy(io.Discard, stdin)
		return 0
	}
	fields := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(stdin, 64<<10))
	for scanner.Scan() {
		if key, value, ok := strings.Cut(scanner.Text(), "="); ok {
			fields[key] = value
		}
	}
	// Orchestrator git children already carry their repository's token as
	// GH_TOKEN. When git also names the repository path (credential.useHttpPath),
	// prefer that repository's dedicated token over the ambient one.
	token := os.Getenv(githubTokenEnv)
	if path := strings.TrimSuffix(strings.TrimPrefix(fields["path"], "/"), ".git"); path != "" {
		if dedicated, ok := config.GitHubTokenFor(path); ok {
			token = dedicated
		}
	}
	if fields["protocol"] != "https" || fields["host"] != "github.com" || token == "" {
		return 0
	}
	fmt.Fprintf(stdout, "username=x-access-token\npassword=%s\n", token)
	return 0
}
