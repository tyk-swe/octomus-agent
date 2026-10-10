package git

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func destinationPolicy() map[string]any {
	return map[string]any{
		"id": 17, "target": "branch", "source_type": "Repository", "source": "fixture/project", "enforcement": "active",
		"bypass_actors": []any{map[string]any{"actor_id": 1001, "actor_type": "User", "bypass_mode": "always"}},
		"conditions":    map[string]any{"ref_name": map[string]any{"include": []string{"~ALL"}, "exclude": []string{"refs/heads/main"}}},
		"rules":         []any{map[string]any{"type": "update", "parameters": map[string]any{"update_allows_fetch_and_merge": false}}},
	}
}

func TestMergeDestinationRequiresAnEnforcedUnambiguousPolicy(t *testing.T) {
	c := config.Default()
	c.GitHubRepo = "fixture/project"
	if err := validateMergeRuleset(marshal(destinationPolicy()), c, 17, 2002); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"wrong id", func(p map[string]any) { p["id"] = 18 }},
		{"tag policy", func(p map[string]any) { p["target"] = "tag" }},
		{"inherited policy", func(p map[string]any) { p["source_type"] = "Organization" }},
		{"wrong repository", func(p map[string]any) { p["source"] = "fixture/other" }},
		{"disabled", func(p map[string]any) { p["enforcement"] = "disabled" }},
		{"evaluate", func(p map[string]any) { p["enforcement"] = "evaluate" }},
		{"partial include", func(p map[string]any) {
			p["conditions"] = map[string]any{"ref_name": map[string]any{"include": []string{"refs/heads/release"}, "exclude": []string{"refs/heads/main"}}}
		}},
		{"wildcard exclusion", func(p map[string]any) {
			p["conditions"] = map[string]any{"ref_name": map[string]any{"include": []string{"~ALL"}, "exclude": []string{"refs/heads/ma*"}}}
		}},
		{"movable default exclusion", func(p map[string]any) {
			p["conditions"] = map[string]any{"ref_name": map[string]any{"include": []string{"~ALL"}, "exclude": []string{"~DEFAULT_BRANCH"}}}
		}},
		{"extra exclusion", func(p map[string]any) {
			p["conditions"] = map[string]any{"ref_name": map[string]any{"include": []string{"~ALL"}, "exclude": []string{"refs/heads/main", "refs/heads/release"}}}
		}},
		{"hidden bypass", func(p map[string]any) { delete(p, "bypass_actors") }},
		{"null bypass", func(p map[string]any) { p["bypass_actors"] = nil }},
		{"merger bypass", func(p map[string]any) {
			p["bypass_actors"] = []any{map[string]any{"actor_id": 2002, "actor_type": "User", "bypass_mode": "pull_request"}}
		}},
		{"role bypass", func(p map[string]any) {
			p["bypass_actors"] = []any{map[string]any{"actor_id": 1, "actor_type": "RepositoryRole"}}
		}},
		{"team bypass", func(p map[string]any) {
			p["bypass_actors"] = []any{map[string]any{"actor_id": 1, "actor_type": "Team"}}
		}},
		{"unknown user", func(p map[string]any) {
			p["bypass_actors"] = []any{map[string]any{"actor_type": "User"}}
		}},
		{"no update restriction", func(p map[string]any) { p["rules"] = []any{map[string]any{"type": "deletion"}} }},
		{"upstream exception", func(p map[string]any) {
			p["rules"] = []any{map[string]any{"type": "update", "parameters": map[string]any{"update_allows_fetch_and_merge": true}}}
		}},
		{"unknown exception", func(p map[string]any) { p["rules"] = []any{map[string]any{"type": "update"}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := destinationPolicy()
			test.edit(policy)
			if err := validateMergeRuleset(marshal(policy), c, 17, 2002); err == nil {
				t.Fatal("unsafe or unverifiable ruleset was accepted")
			}
		})
	}
	p := destinationPolicy()
	p["bypass_actors"] = []any{}
	if err := validateMergeRuleset(marshal(p), c, 17, 2002); err != nil {
		t.Fatalf("explicit empty bypass is restrictive: %v", err)
	}
}

func TestMergeDestinationRefusesMissingConfigurationBeforeCallingGitHub(t *testing.T) {
	c := config.Default()
	c.DeliveryMode = config.DeliveryModeMaintenance
	c.Repository = t.TempDir()
	for _, pair := range [][2]string{{"", "17"}, {"secret", ""}, {"secret", "-1"}, {"secret", "0"}, {"line\nbreak", "17"}} {
		t.Setenv(config.MergeTokenEnv, pair[0])
		t.Setenv(config.MergeRulesetEnv, pair[1])
		if _, err := SquashMerge(t.Context(), c, 7, strings.Repeat("a", 40), "octomus/task"); !errors.Is(err, ErrMergeDestination) {
			t.Fatalf("missing merge policy did not refuse before gh: %v", err)
		}
	}
}

func TestMergeDestinationSeparatesCredentialsAndRejectsUnknownIdentities(t *testing.T) {
	root := t.TempDir()
	if err := testutil.MarkFixtureRoot(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The dispatcher installed by this package's TestMain resolves this local gh.
	script := `#!/bin/sh
set -eu
test "${OCTOMUS_MERGE_TOKEN-unset}" = unset
test "${OCTOMUS_MERGE_TOKEN_FILE-unset}" = unset
case "$*" in
  'api --hostname github.com user')
    test "$GH_TOKEN" = restricted-secret
    test -z "$GITHUB_TOKEN"
    test -z "$GH_DEBUG"
    cat identity.json ;;
  'api --hostname github.com repos/fixture/project/rulesets/17?includes_parents=false')
    test "$GH_TOKEN" = publication-secret
    cat ruleset.json ;;
  *) exit 99 ;;
esac
`
	if err := testutil.WriteExecutable(filepath.Join(root, "bin", "gh"), []byte(script)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ruleset.json"), []byte(marshal(destinationPolicy())), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.MergeTokenEnv, "restricted-secret")
	t.Setenv(config.MergeRulesetEnv, "17")
	t.Setenv("GH_TOKEN", "publication-secret")
	t.Setenv("GITHUB_TOKEN", "ambient-secret")
	t.Setenv("GH_DEBUG", "api")
	c := config.Default()
	c.Repository, c.GitHubRepo = root, "fixture/project"
	for _, identity := range []string{`{"id":2002,"type":"User"}`, `{"id":2002,"type":"Bot"}`, `{"type":"User"}`, `not json`} {
		if err := os.WriteFile(filepath.Join(root, "identity.json"), []byte(identity), 0o600); err != nil {
			t.Fatal(err)
		}
		token, err := mergeDestinationToken(t.Context(), c)
		if identity == `{"id":2002,"type":"User"}` {
			if err != nil || token != "restricted-secret" {
				t.Fatalf("dedicated credential/policy read: %v", err)
			}
		} else if !errors.Is(err, ErrMergeDestination) {
			t.Fatalf("unverifiable identity was accepted: %v", err)
		}
	}
}
