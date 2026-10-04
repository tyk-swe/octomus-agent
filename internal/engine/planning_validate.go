package engine

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type idCoverageErrors struct {
	invented, duplicate, omitted func(id string) string
}

func proposalIDs(proposals []model.Proposal) []string {
	ids := make([]string, len(proposals))
	for i, proposal := range proposals {
		ids[i] = proposal.ID
	}
	return ids
}

func exactIDs(wantList, got []string, messages idCoverageErrors, each func(i int, id string) error) error {
	want := make(map[string]struct{}, len(wantList))
	for _, id := range wantList {
		want[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(got))
	for i, id := range got {
		if _, ok := want[id]; !ok {
			return errors.New(messages.invented(id))
		}
		if _, duplicate := seen[id]; duplicate {
			return errors.New(messages.duplicate(id))
		}
		if each != nil {
			if err := each(i, id); err != nil {
				return err
			}
		}
		seen[id] = struct{}{}
	}
	for _, id := range wantList {
		if _, ok := seen[id]; !ok {
			return errors.New(messages.omitted(id))
		}
	}
	return nil
}

func ResolveTarget(cfg config.Config, prs []model.PullRequest, target string) (*model.PullRequest, error) {
	if target == cfg.DefaultBranch {
		return nil, nil
	}
	var found *model.PullRequest
	for i := range prs {
		pr := &prs[i]
		if pr.Branch == target && pr.OwnedOpen() && pr.Base == cfg.DefaultBranch && config.EqualASCII(pr.BaseRepository, cfg.GitHubRepo) {
			if found != nil {
				return nil, errors.New("Target matches more than one owned open PR")
			}
			copy := pr.Clone()
			found = &copy
		}
	}
	if found == nil {
		return nil, errors.New("Target is not an owned open PR or default branch")
	}
	return found, nil
}

func ValidateProposals(cfg config.Config, proposals []model.Proposal, grounding model.Grounding, history []model.Task) error {
	accepted := map[string]model.Proposal{}
	acceptedInOrder := []model.Proposal{}
	allIDs := map[string]struct{}{}
	for _, proposal := range proposals {
		if strings.TrimSpace(proposal.ID) == "" {
			return errors.New("Proposal identity is empty")
		}
		if _, duplicate := allIDs[proposal.ID]; duplicate {
			return fmt.Errorf("Duplicate proposal identity %q", proposal.ID)
		}
		allIDs[proposal.ID] = struct{}{}
		if !slices.Contains(model.Assessments(), proposal.Decision) {
			return fmt.Errorf("Every proposal needs a decision and rationale: proposal %q has invalid decision %q", proposal.ID, proposal.Decision)
		}
		if strings.TrimSpace(proposal.Reason) == "" {
			return fmt.Errorf("Every proposal needs a decision and rationale: proposal %q has no rationale", proposal.ID)
		}
		if proposal.Decision != model.DecisionAccepted {
			continue
		}
		for _, other := range acceptedInOrder {
			if other.SameWork(proposal) {
				return fmt.Errorf("Duplicate accepted proposal: %q repeats the work of %q", proposal.ID, other.ID)
			}
		}
		accepted[proposal.ID] = proposal
		acceptedInOrder = append(acceptedInOrder, proposal)
		if len(proposal.Title) > 200 || len(proposal.Prompt) > 32000 || len(proposal.Evidence) > 40 {
			return fmt.Errorf("Proposal %q exceeds task size limits: title %d bytes (limit 200), prompt %d bytes (limit 32000), %d evidence items (limit 40)", proposal.ID, len(proposal.Title), len(proposal.Prompt), len(proposal.Evidence))
		}
		if field := missingExecutionContext(proposal); field != "" {
			return fmt.Errorf("Accepted proposal is missing grounding or execution context: proposal %q has no %s", proposal.ID, field)
		}
		if _, ok := cfg.Tiers[proposal.Tier]; !ok || !slices.Contains(cfg.Categories, proposal.Category) {
			return fmt.Errorf("Unknown tier or disabled category for proposal %q (tier %q, category %q)", proposal.ID, proposal.Tier, proposal.Category)
		}
		if _, err := ResolveTarget(cfg, grounding.PRs, proposal.Target); err != nil {
			return fmt.Errorf("Proposal %q target %q: %w", proposal.ID, proposal.Target, err)
		}
		for _, task := range history {
			if task.Status != model.StatusCancelled && task.Proposal.SameWork(proposal) {
				return fmt.Errorf("Proposal %q duplicates recorded work (task %s, %s)", proposal.ID, task.ID, task.Status)
			}
		}
	}
	if acceptedCount := uint64(len(acceptedInOrder)); acceptedCount > cfg.MaxTasksPerCycle {
		return fmt.Errorf("Accepted task limit exceeded: %d accepted (limit %d)", acceptedCount, cfg.MaxTasksPerCycle)
	}
	for _, proposal := range acceptedInOrder {
		stack := append([]string(nil), proposal.Dependencies...)
		seen := map[string]struct{}{}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if id == proposal.ID {
				return fmt.Errorf("Task dependency cycle detected through proposal %q", proposal.ID)
			}
			if _, visited := seen[id]; visited {
				continue
			}
			seen[id] = struct{}{}
			dependency, ok := accepted[id]
			if !ok {
				return fmt.Errorf("Dependency must be an accepted proposal: proposal %q depends on %q", proposal.ID, id)
			}
			if proposal.Target == cfg.DefaultBranch || dependency.Target != proposal.Target {
				return fmt.Errorf("Code dependencies must be delivered on the same existing PR branch; consolidate or defer default-branch dependencies: proposal %q (target %q) depends on %q (target %q)", proposal.ID, proposal.Target, id, dependency.Target)
			}
			stack = append(stack, dependency.Dependencies...)
		}
	}
	return validateBranchOrder(cfg, acceptedInOrder)
}

func missingExecutionContext(proposal model.Proposal) string {
	for _, field := range []struct{ name, value string }{
		{"title", proposal.Title}, {"problem", proposal.Problem}, {"benefit", proposal.Benefit},
		{"scope", proposal.Scope}, {"prompt", proposal.Prompt},
	} {
		if strings.TrimSpace(field.value) == "" {
			return field.name
		}
	}
	if len(proposal.Evidence) == 0 {
		return "evidence"
	}
	return ""
}

func validateBranchOrder(cfg config.Config, accepted []model.Proposal) error {
	branches := []string{}
	members := map[string][]model.Proposal{}
	for _, proposal := range accepted {
		if proposal.Target == cfg.DefaultBranch {
			continue
		}
		if _, known := members[proposal.Target]; !known {
			branches = append(branches, proposal.Target)
		}
		members[proposal.Target] = append(members[proposal.Target], proposal)
	}
	for _, branch := range branches {
		remaining := map[string]struct{}{}
		for _, member := range members[branch] {
			remaining[member.ID] = struct{}{}
		}
		for len(remaining) > 0 {
			ready := ""
			count := 0
			for _, member := range members[branch] {
				if _, present := remaining[member.ID]; !present {
					continue
				}
				blocked := false
				for _, dependency := range member.Dependencies {
					if _, present := remaining[dependency]; present {
						blocked = true
					}
				}
				if !blocked {
					ready = member.ID
					count++
				}
			}
			if count != 1 {
				return fmt.Errorf("Accepted tasks on %s need a complete dependency order; unordered or forked branch plans cannot execute", branch)
			}
			delete(remaining, ready)
		}
	}
	return nil
}

const (
	MaxExternalPRs    = 100
	MaxPRTitleChars   = 200
	MaxPRBodyChars    = 2000
	MaxPRContextBytes = 512 * 1024
)

func ExternalContext(inventory model.OpenPrInventory) ([]model.ExternalPrContext, model.PrCoverage, error) {
	external := []model.PullRequest{}
	for _, pr := range inventory.PRs {
		if !pr.Owned {
			external = append(external, pr)
		}
	}
	sort.Slice(external, func(i, j int) bool { return external[i].Number < external[j].Number })
	total := len(external)
	result := []model.ExternalPrContext{}
	bytesUsed := 2
	for _, pr := range external {
		if len(result) >= MaxExternalPRs {
			break
		}
		title, titleCut := truncateRunes(pr.Title, MaxPRTitleChars)
		body, bodyCut := truncateRunes(pr.Body, MaxPRBodyChars)
		entry := model.ExternalPrContext{Number: pr.Number, URL: pr.URL, Title: title, Body: body, Branch: pr.Branch, Head: pr.Head, Base: pr.Base, HeadRepository: pr.HeadRepository, BaseRepository: pr.BaseRepository, TitleTruncated: titleCut, BodyTruncated: bodyCut}
		encoded, err := wirejson.Marshal(entry)
		if err != nil {
			return nil, model.PrCoverage{}, err
		}
		extra := len(encoded)
		if len(result) > 0 {
			extra++
		}
		if bytesUsed+extra > MaxPRContextBytes {
			break
		}
		bytesUsed += extra
		result = append(result, entry)
	}
	coverage := model.PrCoverage{ObservedAt: stringPointer(inventory.ObservedAt), Complete: true, TotalOpen: uint64(len(inventory.PRs)), TotalExternal: uint64(total), IncludedExternal: uint64(len(result)), OmittedExternal: uint64(total - len(result)), MaxExternal: MaxExternalPRs, MaxTitleChars: MaxPRTitleChars, MaxBodyChars: MaxPRBodyChars, MaxContextBytes: MaxPRContextBytes}
	return result, coverage, nil
}

func truncateRunes(value string, limit int) (string, bool) {
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	runes := []rune(value)
	return string(runes[:limit]), true
}
