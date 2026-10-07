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

// depNode is one unit of planned work, a proposal or a task: its id, the branch it writes and the ids it depends on.
type depNode struct {
	id, target string
	deps       []string
}

// validateDependencies checks a plan's dependency graph: every dependency names a node of the plan, default-branch
// work has none, a dependency stays on its dependent's existing PR branch, the graph is acyclic and the writers to
// each branch form exactly one order. noun names the nodes in messages.
func validateDependencies(noun, defaultBranch string, nodes []depNode) error {
	byID := make(map[string]depNode, len(nodes))
	for _, node := range nodes {
		byID[node.id] = node
	}
	for _, node := range nodes {
		for _, id := range node.deps {
			dependency, ok := byID[id]
			if !ok {
				return fmt.Errorf("%s %q depends on %q, which is not an accepted %s in this plan", noun, node.id, id, noun)
			}
			if node.target == defaultBranch {
				return fmt.Errorf("Default-branch work cannot depend on another %s; consolidate or defer it until the prerequisite PR has merged: %s %q depends on %q", noun, noun, node.id, id)
			}
			if dependency.target != node.target {
				return fmt.Errorf("Dependent %ss must write the same existing pull request: %s %q (target %q) depends on %q (target %q)", noun, noun, node.id, node.target, id, dependency.target)
			}
		}
	}
	state := map[string]uint8{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("Dependency cycle through %s %q", noun, id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependency := range byID[id].deps {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, node := range nodes {
		if err := visit(node.id); err != nil {
			return err
		}
	}
	// On an acyclic graph the writers to a branch have exactly one order when, at every step, exactly one of the
	// remaining writers has no remaining prerequisite.
	branches := []string{}
	members := map[string][]depNode{}
	for _, node := range nodes {
		if node.target == defaultBranch {
			continue
		}
		if _, known := members[node.target]; !known {
			branches = append(branches, node.target)
		}
		members[node.target] = append(members[node.target], node)
	}
	for _, branch := range branches {
		remaining := map[string]struct{}{}
		for _, member := range members[branch] {
			remaining[member.id] = struct{}{}
		}
		for len(remaining) > 0 {
			ready := ""
			count := 0
			for _, member := range members[branch] {
				if _, present := remaining[member.id]; !present {
					continue
				}
				blocked := false
				for _, dependency := range member.deps {
					if _, present := remaining[dependency]; present {
						blocked = true
					}
				}
				if !blocked {
					ready = member.id
					count++
				}
			}
			if count != 1 {
				return fmt.Errorf("Accepted %ss on %s need a complete dependency order; unordered or forked branch plans cannot execute", noun, branch)
			}
			delete(remaining, ready)
		}
	}
	return nil
}

func validateProposals(cfg config.Config, proposals []model.Proposal, grounding model.Grounding, history []model.Task) error {
	accepted := []model.Proposal{}
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
		for _, other := range accepted {
			if other.SameWork(proposal) {
				return fmt.Errorf("Duplicate accepted proposal: %q repeats the work of %q", proposal.ID, other.ID)
			}
		}
		accepted = append(accepted, proposal)
		if len(proposal.Title) > 200 || len(proposal.Prompt) > 32000 || len(proposal.Evidence) > 40 {
			return fmt.Errorf("Proposal %q exceeds task size limits: title %d bytes (limit 200), prompt %d bytes (limit 32000), %d evidence items (limit 40)", proposal.ID, len(proposal.Title), len(proposal.Prompt), len(proposal.Evidence))
		}
		if field := missingField(proposal); field != "" {
			return fmt.Errorf("Accepted proposal is missing grounding or execution context: proposal %q has no %s", proposal.ID, field)
		}
		if _, ok := cfg.Tiers[proposal.Tier]; !ok || !slices.Contains(cfg.EffectiveCategories(), proposal.Category) {
			return fmt.Errorf("Unknown tier or ineligible category for proposal %q (tier %q, category %q)", proposal.ID, proposal.Tier, proposal.Category)
		}
		if _, err := resolveTarget(cfg, grounding.PRs, proposal.Target); err != nil {
			return fmt.Errorf("Proposal %q target %q: %w", proposal.ID, proposal.Target, err)
		}
		for _, task := range history {
			if task.Status != model.StatusCancelled && task.Proposal.SameWork(proposal) {
				return fmt.Errorf("Proposal %q duplicates recorded work (task %s, %s)", proposal.ID, task.ID, task.Status)
			}
		}
	}
	if acceptedCount := uint64(len(accepted)); acceptedCount > cfg.MaxTasksPerCycle {
		return fmt.Errorf("Accepted task limit exceeded: %d accepted (limit %d)", acceptedCount, cfg.MaxTasksPerCycle)
	}
	nodes := make([]depNode, len(accepted))
	for i, proposal := range accepted {
		nodes[i] = depNode{id: proposal.ID, target: proposal.Target, deps: proposal.Dependencies}
	}
	return validateDependencies("proposal", cfg.DefaultBranch, nodes)
}

// validateTaskPlan rechecks a committed cycle's dependency plan before dispatch. The tasks of one cycle share the
// configuration snapshot their plan was committed with.
func validateTaskPlan(tasks []model.Task, live config.Config) error {
	nodes := make([]depNode, 0, len(tasks))
	seen := map[string]struct{}{}
	defaultBranch := ""
	for _, task := range tasks {
		if _, duplicate := seen[task.ID]; duplicate {
			return fmt.Errorf("Duplicate task identity %s", task.ID)
		}
		seen[task.ID] = struct{}{}
		if task.Config.DeliveryMode != live.DeliveryMode ||
			!slices.Contains(task.Config.EffectiveCategories(), task.Proposal.Category) {
			return fmt.Errorf("Task %s was planned under an incompatible delivery mode or category (mode %s, category %q)", task.ID, task.Config.DeliveryMode, task.Proposal.Category)
		}
		nodes = append(nodes, depNode{id: task.ID, target: task.Proposal.Target, deps: task.Proposal.Dependencies})
		defaultBranch = task.Config.DefaultBranch
	}
	return validateDependencies("task", defaultBranch, nodes)
}

func missingField(proposal model.Proposal) string {
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

func resolveTarget(cfg config.Config, prs []model.PullRequest, target string) (*model.PullRequest, error) {
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

type idMessages struct {
	invented, duplicate, omitted func(id string) string
}

func proposalIDs(proposals []model.Proposal) []string {
	ids := make([]string, len(proposals))
	for i, proposal := range proposals {
		ids[i] = proposal.ID
	}
	return ids
}

func matchIDs(wantList, got []string, messages idMessages, each func(i int, id string) error) error {
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

func checkAssessments(reviewer string, candidates []model.Proposal, assessments []model.Assessment) error {
	ids := make([]string, len(assessments))
	for i, item := range assessments {
		ids[i] = item.ID
	}
	return matchIDs(proposalIDs(candidates), ids, idMessages{
		invented: func(id string) string {
			return fmt.Sprintf("Adversarial reviewer %s invented proposal %q", reviewer, id)
		},
		duplicate: func(id string) string {
			return fmt.Sprintf("Adversarial reviewer %s assessed proposal %q more than once", reviewer, id)
		},
		omitted: func(id string) string {
			return fmt.Sprintf("Adversarial reviewer %s omitted proposal %q", reviewer, id)
		},
	}, func(i int, id string) error {
		item := assessments[i]
		if !slices.Contains(model.Assessments(), item.Decision) {
			return fmt.Errorf("Adversarial reviewer %s gave proposal %q an invalid decision %q", reviewer, id, item.Decision)
		}
		if strings.TrimSpace(item.Reason) == "" {
			return fmt.Errorf("Adversarial reviewer %s gave proposal %q no rationale", reviewer, id)
		}
		return nil
	})
}

func checkConsolidation(candidates, returned []model.Proposal) error {
	return matchIDs(proposalIDs(candidates), proposalIDs(returned), idMessages{
		invented: func(id string) string {
			return fmt.Sprintf("Orchestrator omitted or invented proposal IDs: invented %q", id)
		},
		duplicate: func(id string) string {
			return fmt.Sprintf("Orchestrator omitted or invented proposal IDs: returned %q twice", id)
		},
		omitted: func(id string) string {
			return fmt.Sprintf("Orchestrator omitted or invented proposal IDs: omitted %q", id)
		},
	}, nil)
}

func checkRediscoveries(requests []rediscoveryRequest, proposals []model.Proposal) error {
	for _, request := range requests {
		id := request.ID
		count := 0
		for _, proposal := range proposals {
			if slices.Contains(proposal.Reconsiders, id) {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("Every rediscovery request needs exactly one fresh decision (request %s had %d)", id, count)
		}
	}
	return nil
}

func plannedStatus(proposals []model.Proposal) model.CycleStatus {
	for _, proposal := range proposals {
		if proposal.Decision == model.DecisionAccepted {
			return model.CycleCompleted
		}
	}
	return model.CycleIdle
}

const maxPlanningProposals = 100

func discoveryProposalLimit(seeded int, agents uint64) int {
	room := maxPlanningProposals - seeded
	if room <= 0 || agents == 0 || agents > uint64(room) {
		return 0
	}
	return room / int(agents)
}

const (
	maxExternalPRs    = 100
	maxPRTitleChars   = 200
	maxPRBodyChars    = 2000
	maxPRContextBytes = 512 * 1024
)

func externalContext(inventory model.OpenPRInventory) ([]model.ExternalPRContext, model.PRCoverage, error) {
	external := []model.PullRequest{}
	for _, pr := range inventory.PRs {
		if !pr.Owned {
			external = append(external, pr)
		}
	}
	sort.Slice(external, func(i, j int) bool { return external[i].Number < external[j].Number })
	total := len(external)
	result := []model.ExternalPRContext{}
	bytesUsed := 2
	for _, pr := range external {
		if len(result) >= maxExternalPRs {
			break
		}
		title, titleCut := truncateRunes(pr.Title, maxPRTitleChars)
		body, bodyCut := truncateRunes(pr.Body, maxPRBodyChars)
		entry := model.ExternalPRContext{Number: pr.Number, URL: pr.URL, Title: title, Body: body, Branch: pr.Branch, Head: pr.Head, Base: pr.Base, HeadRepository: pr.HeadRepository, BaseRepository: pr.BaseRepository, TitleTruncated: titleCut, BodyTruncated: bodyCut}
		encoded, err := wirejson.Marshal(entry)
		if err != nil {
			return nil, model.PRCoverage{}, err
		}
		extra := len(encoded)
		if len(result) > 0 {
			extra++
		}
		if bytesUsed+extra > maxPRContextBytes {
			break
		}
		bytesUsed += extra
		result = append(result, entry)
	}
	coverage := model.PRCoverage{ObservedAt: new(inventory.ObservedAt), Complete: true, TotalOpen: uint64(len(inventory.PRs)), TotalExternal: uint64(total), IncludedExternal: uint64(len(result)), OmittedExternal: uint64(total - len(result)), MaxExternal: maxExternalPRs, MaxTitleChars: maxPRTitleChars, MaxBodyChars: maxPRBodyChars, MaxContextBytes: maxPRContextBytes}
	return result, coverage, nil
}

func truncateRunes(value string, limit int) (string, bool) {
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	runes := []rune(value)
	return string(runes[:limit]), true
}
