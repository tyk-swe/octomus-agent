package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestRepoTokensDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, store.RepositorySlug("acme/one")), []byte("token-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, store.RepositorySlug("acme/two")), []byte("token-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{config.GitHubTokensDirEnv: dir, config.RepoGitHubTokenEnv("acme/two"): "direct-two"}
	env := func(k string) (string, bool) { v, ok := values[k]; return v, ok }
	loaded, err := loadSecretFiles(env, func(k, v string) error { values[k] = v; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := loaded(config.RepoGitHubTokenEnv("acme/one")); !ok || value != "token-one" {
		t.Fatalf("directory token not loaded: %q %v", value, ok)
	}
	if value, ok := loaded(config.RepoGitHubTokenEnv("acme/two")); !ok || value != "direct-two" {
		t.Fatalf("direct variable not preferred over directory file: %q %v", value, ok)
	}
}

func TestRepoTokenFilesKeepRepositoryBoundaries(t *testing.T) {
	dir := t.TempDir()
	repos := []string{"acme-tools/service", "acme/tools-service", "acme/one-two", "acme/one_two"}
	for _, repo := range repos {
		if err := os.WriteFile(filepath.Join(dir, store.RepositorySlug(repo)), []byte("token-for-"+repo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	values := map[string]string{config.GitHubTokensDirEnv: dir}
	loaded, err := loadSecretFiles(func(k string) (string, bool) { v, ok := values[k]; return v, ok }, func(k, v string) error { values[k] = v; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range repos {
		if value, ok := loaded(config.RepoGitHubTokenEnv(repo)); !ok || value != "token-for-"+repo {
			t.Fatalf("wrong token file loaded for %s", repo)
		}
	}
}

func TestRestrictedMergerSecretFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merger")
	if err := os.WriteFile(path, []byte("restricted-merger-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"OCTOMUS_MERGE_TOKEN_FILE": path}
	env := func(k string) (string, bool) { v, ok := values[k]; return v, ok }
	loaded, err := loadSecretFiles(env, func(k, v string) error { values[k] = v; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := loaded(config.MergeTokenEnv); !ok || value != "restricted-merger-secret" {
		t.Fatal("merger file was not loaded")
	}
	if _, err := loadSecretFiles(env, func(string, string) error { t.Fatal("conflicting secrets were applied"); return nil }); err == nil {
		t.Fatal("both merger environment and file were accepted")
	}
}
