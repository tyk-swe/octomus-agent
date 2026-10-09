package main

import (
	"bufio"
	"fmt"
	"github.com/tyk-swe/octomus-agent/internal/config"
	"io"
	"os"
	"strings"
)

// githubTokenEnv is where the Docker deployment's GitHub token lives inside the control plane, for gh and the git
// credential helper. Sandboxes never see the control plane's environment.
const githubTokenEnv = "GH_TOKEN"

// secretFiles lets each secret the service reads from its environment arrive as a file instead, the way Docker
// secrets do, which keeps it out of `docker inspect`.
var secretFiles = []struct{ variable, file string }{
	{config.TokenEnv, "OCTOMUS_TOKEN_FILE"},
	{config.WebhookEnv, "OCTOMUS_NOTIFICATION_WEBHOOK_URL_FILE"},
	{githubTokenEnv, "OCTOMUS_GITHUB_TOKEN_FILE"},
	{config.MergeTokenEnv, "OCTOMUS_MERGE_TOKEN_FILE"},
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
