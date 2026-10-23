package config

import (
	"os"
	"strings"
)

// Per-repository deployment credentials live in variables suffixed with the
// repository identity, next to the original single-repository variables,
// which keep serving the first (primary) repository:
//
//	OCTOMUS_GITHUB_TOKEN__OWNER_2FNAME    GitHub token for owner/name
//	OCTOMUS_MERGE_TOKEN__OWNER_2FNAME     merger token for owner/name
//	OCTOMUS_MERGE_RULESET_ID__OWNER_2FNAME destination ruleset for owner/name
//
// The suffix uppercases the repository and encodes every byte outside
// [A-Z0-9] as _XX (including underscores), preserving repository boundaries
// and punctuation. Resolved token values are injected
// into orchestrator git/gh children per repository; the suffixed variables
// themselves are stripped from ordinary children (see internal/process).
const (
	GitHubTokensDirEnv = "OCTOMUS_GITHUB_TOKENS_DIR"
	gitHubTokenPrefix  = "OCTOMUS_GITHUB_TOKEN__"
	mergeTokenPrefix   = "OCTOMUS_MERGE_TOKEN__"
	mergeRulesetPrefix = "OCTOMUS_MERGE_RULESET_ID__"
)

// RepoEnvSuffix maps a repository identity to an unambiguous, case-insensitive
// environment suffix shared by the loader and the consumers.
func RepoEnvSuffix(repo string) string {
	const hex = "0123456789ABCDEF"
	var suffix strings.Builder
	for _, b := range []byte(strings.ToUpper(repo)) {
		if (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
			suffix.WriteByte(b)
		} else {
			suffix.WriteByte('_')
			suffix.WriteByte(hex[b>>4])
			suffix.WriteByte(hex[b&15])
		}
	}
	return suffix.String()
}

func RepoGitHubTokenEnv(repo string) string  { return gitHubTokenPrefix + RepoEnvSuffix(repo) }
func RepoMergeTokenEnv(repo string) string   { return mergeTokenPrefix + RepoEnvSuffix(repo) }
func RepoMergeRulesetEnv(repo string) string { return mergeRulesetPrefix + RepoEnvSuffix(repo) }
func IsPerRepoSecretEnv(key string) bool {
	return strings.HasPrefix(key, gitHubTokenPrefix) ||
		strings.HasPrefix(key, mergeTokenPrefix) ||
		strings.HasPrefix(key, mergeRulesetPrefix) ||
		key == GitHubTokensDirEnv
}

func envValue(name string) (string, bool) {
	value, ok := os.LookupEnv(name)
	return value, ok && strings.TrimSpace(value) != ""
}

// GitHubTokenFor resolves the token for one repository: its own suffixed
// variable when set, else the deployment's GH_TOKEN.
func GitHubTokenFor(repo string) (string, bool) {
	if repo != "" {
		if value, ok := envValue(RepoGitHubTokenEnv(repo)); ok {
			return value, true
		}
	}
	return envValue("GH_TOKEN")
}

// HasOwnGitHubToken reports whether the repository has a dedicated token
// variable, as multi-repository operation requires for every added repository.
func HasOwnGitHubToken(repo string) bool {
	_, ok := envValue(RepoGitHubTokenEnv(repo))
	return repo != "" && ok
}

// MergeTokenFor resolves the merger token for one repository with the
// single-repository variable as fallback, naming the variable to configure.
func MergeTokenFor(repo string) (token, variable string) {
	if repo != "" {
		if value, ok := envValue(RepoMergeTokenEnv(repo)); ok {
			return value, RepoMergeTokenEnv(repo)
		}
	}
	value, _ := envValue(MergeTokenEnv)
	return value, MergeTokenEnv
}

// MergeRulesetFor resolves the destination ruleset ID text for one
// repository with the single-repository variable as fallback.
func MergeRulesetFor(repo string) (ruleset, variable string) {
	if repo != "" {
		if value, ok := envValue(RepoMergeRulesetEnv(repo)); ok {
			return value, RepoMergeRulesetEnv(repo)
		}
	}
	value, _ := envValue(MergeRulesetEnv)
	return value, MergeRulesetEnv
}
