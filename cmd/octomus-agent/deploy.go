package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/egress"
	"github.com/tyk-swe/octomus-agent/internal/engine"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/process"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// githubTokenEnv is where the Docker deployment's GitHub token lives inside the control plane, for gh and the git
// credential helper. Sandboxes never see the control plane's environment.
const githubTokenEnv = "GH_TOKEN"

// secretFiles lets each secret the service reads from its environment arrive as a file instead, the way Docker
// secrets do, which keeps it out of `docker inspect`.
var secretFiles = []struct{ variable, file string }{
	{redact.TokenEnv, "OCTOMUS_TOKEN_FILE"},
	{redact.WebhookEnv, "OCTOMUS_NOTIFICATION_WEBHOOK_URL_FILE"},
	{githubTokenEnv, "OCTOMUS_GITHUB_TOKEN_FILE"},
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
		data, err := io.ReadAll(io.LimitReader(file, 64<<10))
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("Reading %s: %w", secret.file, err)
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
	return func(key string) (string, bool) {
		if value, ok := loaded[key]; ok {
			return value, true
		}
		return env(key)
	}, nil
}

// prepareDeployment reads the Docker deployment's fixed settings. A GitHub token or pinned repository only exists in
// Docker mode: without a sandbox, runners would inherit the one and could rewrite the other.
func prepareDeployment(ctx context.Context, mode sandbox.Mode, data string, env func(string) (string, bool), stderr io.Writer) (engine.Deployment, error) {
	repo, pinned := env("OCTOMUS_GITHUB_REPO")
	_, token := env(githubTokenEnv)
	fromFile, _ := env("OCTOMUS_GITHUB_TOKEN_FILE")
	if mode == sandbox.ModeOff {
		if pinned && repo != "" {
			return engine.Deployment{}, errors.New("OCTOMUS_GITHUB_REPO needs --sandbox docker; unsandboxed runners could rewrite a managed checkout")
		}
		if fromFile != "" {
			return engine.Deployment{}, errors.New("OCTOMUS_GITHUB_TOKEN_FILE needs --sandbox docker; unsandboxed runners would inherit the token")
		}
		return engine.Deployment{}, nil
	}
	policy, err := egress.PolicyFromEnv(getenv(env))
	if err != nil {
		return engine.Deployment{}, err
	}
	deployment := engine.Deployment{Egress: policy.Describe()}
	if !pinned || repo == "" {
		return deployment, nil
	}
	if !config.ValidGitHubRepo(repo) {
		return engine.Deployment{}, errors.New("OCTOMUS_GITHUB_REPO must be OWNER/REPOSITORY")
	}
	checkout := filepath.Join(data, "checkout")
	deployment.Repository, deployment.GitHubRepo = checkout, repo
	if _, err := os.Stat(filepath.Join(checkout, ".git")); err == nil {
		cfg := config.Default()
		cfg.Repository, cfg.GitHubRepo = checkout, repo
		if err := gitops.ValidateRemote(ctx, cfg); err != nil {
			return engine.Deployment{}, fmt.Errorf("The trusted checkout is not %s; remove %s and restart: %w", repo, checkout, err)
		}
		return deployment, nil
	}
	if !token {
		return engine.Deployment{}, errors.New("The trusted checkout needs a GitHub token; set OCTOMUS_GITHUB_TOKEN_FILE")
	}
	fmt.Fprintf(stderr, "Cloning %s into the trusted checkout; later starts reuse it.\n", repo)
	partial := checkout + ".partial"
	_ = os.RemoveAll(partial)
	if _, err := process.RunMachine(ctx, "git", []string{"clone", "--origin", "origin", "https://github.com/" + repo + ".git", partial}, data, 3600); err != nil {
		_ = os.RemoveAll(partial)
		return engine.Deployment{}, fmt.Errorf("Cloning %s: %w", repo, err)
	}
	if err := os.Rename(partial, checkout); err != nil {
		return engine.Deployment{}, err
	}
	cfg := config.Default()
	cfg.Repository, cfg.GitHubRepo = checkout, repo
	if err := gitops.ValidateRemote(ctx, cfg); err != nil {
		return engine.Deployment{}, fmt.Errorf("The new checkout does not verify: %w", err)
	}
	return deployment, nil
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
	token := os.Getenv(githubTokenEnv)
	if fields["protocol"] != "https" || fields["host"] != "github.com" || token == "" {
		return 0
	}
	fmt.Fprintf(stdout, "username=x-access-token\npassword=%s\n", token)
	return 0
}

// healthcheck asks this container's own service whether it is up, for Docker's HEALTHCHECK.
func healthcheck(listen string, stderr io.Writer) int {
	address, err := parseListen(listen)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", address.Port()))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
