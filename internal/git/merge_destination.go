package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/process"
)

// ErrMergeDestination means no mutation was attempted: GitHub's protection of the
// configured destination could not be established. Installing the required policy
// can make the same reviewed delivery eligible again.
var ErrMergeDestination = errors.New("Automatic merge destination is not enforced")

// GitHub's merge API guards the head SHA, but cannot bind the PR base. A separate
// merger must therefore be denied updates to every other branch by an active
// server-side ruleset. See docs/maintenance-merging.md for the trust assumptions.
func mergeDestinationToken(ctx context.Context, c config.Config) (string, error) {
	token := os.Getenv(config.MergeTokenEnv)
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n\x00") {
		return "", fmt.Errorf("%w: configure %s or its secret file", ErrMergeDestination, config.MergeTokenEnv)
	}
	rulesetID, err := strconv.ParseUint(os.Getenv(config.MergeRulesetEnv), 10, 64)
	if err != nil || rulesetID == 0 {
		return "", fmt.Errorf("%w: configure a positive %s", ErrMergeDestination, config.MergeRulesetEnv)
	}
	identity, err := mergeGH(ctx, c, token, []string{"api", "--hostname", "github.com", "user"})
	if err != nil {
		return "", fmt.Errorf("%w: reading the merger identity: %w", ErrMergeDestination, err)
	}
	var user struct {
		ID   uint64 `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(identity), &user); err != nil || user.ID == 0 || user.Type != "User" {
		return "", fmt.Errorf("%w: the merger must be an identifiable dedicated user", ErrMergeDestination)
	}
	// Read using the operator's normal gh identity. GitHub omits bypass_actors
	// for readers without ruleset write access; omitted is not an empty list.
	rules, err := gh(ctx, c, []string{"api", "--hostname", "github.com",
		fmt.Sprintf("repos/%s/rulesets/%d?includes_parents=false", c.GitHubRepo, rulesetID)})
	if err != nil {
		return "", fmt.Errorf("%w: reading the destination ruleset: %w", ErrMergeDestination, err)
	}
	if err := validateMergeRuleset(rules, c, rulesetID, user.ID); err != nil {
		return "", fmt.Errorf("%w: %w", ErrMergeDestination, err)
	}
	return token, nil
}

func mergeGH(ctx context.Context, c config.Config, token string, args []string) (string, error) {
	return process.RunMachine(ctx, "gh", args, c.Repository, c.CommandTimeoutSeconds,
		[]string{"GH_TOKEN=" + token, "GITHUB_TOKEN=", "GH_HOST=github.com", "GH_DEBUG="})
}

func validateMergeRuleset(raw string, c config.Config, rulesetID, mergerID uint64) error {
	var policy struct {
		ID          uint64 `json:"id"`
		Target      string `json:"target"`
		SourceType  string `json:"source_type"`
		Source      string `json:"source"`
		Enforcement string `json:"enforcement"`
		Bypass      *[]struct {
			ID   uint64 `json:"actor_id"`
			Type string `json:"actor_type"`
		} `json:"bypass_actors"`
		Conditions struct {
			RefName struct {
				Include []string `json:"include"`
				Exclude []string `json:"exclude"`
			} `json:"ref_name"`
		} `json:"conditions"`
		Rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				AllowsFetchAndMerge *bool `json:"update_allows_fetch_and_merge"`
			} `json:"parameters"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return errors.New("Destination ruleset response is unreadable")
	}
	if rulesetID == 0 || mergerID == 0 || policy.ID != rulesetID || policy.Target != "branch" ||
		policy.SourceType != "Repository" || !config.EqualASCII(policy.Source, c.GitHubRepo) || policy.Enforcement != "active" {
		return errors.New("Destination ruleset must be active and belong to this repository")
	}
	refs := policy.Conditions.RefName
	if !config.ValidBranch(c.DefaultBranch) || len(refs.Include) != 1 || refs.Include[0] != "~ALL" ||
		len(refs.Exclude) != 1 || refs.Exclude[0] != "refs/heads/"+c.DefaultBranch {
		return errors.New("Destination ruleset must restrict every branch except the exact configured default branch")
	}
	if policy.Bypass == nil {
		return errors.New("Destination ruleset bypass actors are hidden; the policy reader needs ruleset write access")
	}
	for _, actor := range *policy.Bypass {
		// Team/role/App membership cannot be inferred from a user ID. Support
		// only an explicit list of other users, never an ambiguous bypass.
		if actor.ID == 0 || actor.Type != "User" || actor.ID == mergerID {
			return errors.New("Destination ruleset bypass actors must be explicit users other than the merger")
		}
	}
	for _, rule := range policy.Rules {
		if rule.Type == "update" && rule.Parameters.AllowsFetchAndMerge != nil && !*rule.Parameters.AllowsFetchAndMerge {
			return nil
		}
	}
	return errors.New("Destination ruleset must restrict updates without an upstream fetch-and-merge exception")
}
