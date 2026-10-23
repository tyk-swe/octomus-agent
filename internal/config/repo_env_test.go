package config_test

import (
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func TestRepoTokenResolution(t *testing.T) {
	t.Setenv("GH_TOKEN", "shared-token")
	t.Setenv(config.RepoGitHubTokenEnv("acme/one"), "repo-one-token")

	if got, ok := config.GitHubTokenFor("acme/one"); !ok || got != "repo-one-token" {
		t.Fatalf("dedicated token not preferred: %q %v", got, ok)
	}
	if got, ok := config.GitHubTokenFor("acme/two"); !ok || got != "shared-token" {
		t.Fatalf("shared fallback not used: %q %v", got, ok)
	}
	if !config.HasOwnGitHubToken("ACME/One") {
		t.Fatal("dedicated token not detected case-insensitively")
	}
	if config.HasOwnGitHubToken("acme/two") {
		t.Fatal("shared token reported as dedicated")
	}
	if !config.IsPerRepoSecretEnv(config.RepoGitHubTokenEnv("acme/one")) || config.IsPerRepoSecretEnv("GH_TOKEN") {
		t.Fatal("per-repository secret classification differs")
	}
}

func TestRepositoryCredentialsDoNotCollide(t *testing.T) {
	repos := []string{"acme/one-two", "acme/one_two", "acme/one.two", "acme-one/two", "acme/one_2Dtwo"}
	seen := map[string]bool{}
	for _, repo := range repos {
		for _, name := range []string{config.RepoGitHubTokenEnv(repo), config.RepoMergeTokenEnv(repo), config.RepoMergeRulesetEnv(repo)} {
			if seen[name] {
				t.Fatalf("credential identifier collision: %s", name)
			}
			seen[name] = true
		}
		t.Setenv(config.RepoGitHubTokenEnv(repo), "token-for-"+repo)
		t.Setenv(config.RepoMergeTokenEnv(repo), "merger-for-"+repo)
		t.Setenv(config.RepoMergeRulesetEnv(repo), "ruleset-for-"+repo)
	}
	for _, repo := range repos {
		if token, ok := config.GitHubTokenFor(repo); !ok || token != "token-for-"+repo || !config.HasOwnGitHubToken(repo) {
			t.Fatalf("wrong GitHub token for %s", repo)
		}
		if token, _ := config.MergeTokenFor(repo); token != "merger-for-"+repo {
			t.Fatalf("wrong merger token for %s", repo)
		}
		if ruleset, _ := config.MergeRulesetFor(repo); ruleset != "ruleset-for-"+repo {
			t.Fatalf("wrong ruleset for %s", repo)
		}
	}
}
