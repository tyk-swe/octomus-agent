package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

type rediscoveryRequest struct {
	ID, Target string
	entry      map[string]any
}

type decisionMemory struct {
	decisions []model.DecisionRecord
	requests  []rediscoveryRequest
}

// promptEntries is what the planning roles are shown: every decision with its kind and reconsideration flag spelled
// out, then the rediscovery requests.
func (m decisionMemory) promptEntries() ([]any, error) {
	entries := make([]any, 0, len(m.decisions)+len(m.requests))
	for _, record := range m.decisions {
		entry, err := wirejson.GenericMap(record)
		if err != nil {
			return nil, err
		}
		entry["kind"], entry["reconsideration_due"] = record.Kind, record.ReconsiderationDue
		entries = append(entries, entry)
	}
	for _, request := range m.requests {
		entry := map[string]any{"kind": "rediscovery"}
		for key, item := range request.entry {
			entry[key] = item
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (a *App) planningMemory(ctx context.Context, cfg config.Config, grounding model.Grounding) (decisionMemory, error) {
	targets := []string{cfg.DefaultBranch}
	for _, pr := range grounding.PRs {
		if _, err := resolveTarget(cfg, grounding.PRs, pr.Branch); err == nil && pr.Branch != cfg.DefaultBranch {
			targets = append(targets, pr.Branch)
		}
	}
	records, err := a.Store.DecisionMemory(cfg.GitHubRepo, targets)
	if err != nil {
		return decisionMemory{}, err
	}
	decisions, err := currentDecisions(ctx, cfg, grounding, records)
	if err != nil {
		return decisionMemory{}, err
	}
	memory := decisionMemory{decisions: decisions, requests: []rediscoveryRequest{}}
	requests, err := a.Store.RediscoveryRequests(cfg.GitHubRepo)
	if err != nil {
		return decisionMemory{}, err
	}
	for _, value := range requests {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		target, _ := entry["target"].(string)
		memory.requests = append(memory.requests, rediscoveryRequest{ID: id, Target: target, entry: entry})
	}
	return memory, nil
}

// Admission uses exact stored identities independently of the bounded prompt selection.
func (a *App) enforceDecisionMemory(ctx context.Context, cfg config.Config, grounding model.Grounding, proposals []model.Proposal, requests []rediscoveryRequest) error {
	memory := decisionMemory{requests: requests}
	if err := validateDecisionMemory(proposals, memory); err != nil {
		return err
	}
	for _, proposal := range proposals {
		if proposal.Decision != model.DecisionAccepted {
			continue
		}
		records, err := a.Store.DecisionsForProblem(cfg.GitHubRepo, proposal.Target, proposal.ProblemIdentity())
		if err != nil {
			return err
		}
		memory.decisions, err = currentDecisions(ctx, cfg, grounding, records)
		if err != nil {
			return err
		}
		if err := validateDecisionMemory([]model.Proposal{proposal}, memory); err != nil {
			return err
		}
	}
	return nil
}

func currentDecisions(ctx context.Context, cfg config.Config, grounding model.Grounding, records []model.DecisionRecord) ([]model.DecisionRecord, error) {
	decisions := make([]model.DecisionRecord, 0, len(records))
	// Historical decisions often cover the same paths. Cache only for this refresh;
	// reconsideration is evaluated against the current grounding and time.
	type fingerprintKey struct{ revision, paths string }
	fingerprints := make(map[fingerprintKey]string)
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.ID == "" || record.Repository == "" || record.Target == "" || record.ProblemKey == "" {
			continue
		}
		target, err := resolveTarget(cfg, grounding.PRs, record.Target)
		if err != nil {
			continue
		}
		revision := grounding.Revision
		if target != nil {
			revision = target.Head
		}
		paths, err := json.Marshal(record.RelevantPaths)
		if err != nil {
			return nil, err
		}
		key := fingerprintKey{revision, string(paths)}
		fingerprint, found := fingerprints[key]
		if !found {
			fingerprint, err = decisionFingerprint(ctx, cfg, revision, record.RelevantPaths)
			if err != nil {
				return nil, err
			}
			fingerprints[key] = fingerprint
		}
		due := fingerprint != record.ContextFingerprint
		if until, err := time.Parse(time.RFC3339, record.ReconsiderAfter); err == nil && !time.Now().Before(until) {
			due = true
		}
		record.Kind, record.ReconsiderationDue = "decision", due
		decisions = append(decisions, record)
	}
	return decisions, nil
}

// decisionFingerprint identifies the repository state a decision was made against: the revision itself when the decision
// names no paths, otherwise a digest of those paths' tree entries at that revision.
func decisionFingerprint(ctx context.Context, cfg config.Config, revision string, paths []string) (string, error) {
	if len(paths) > 40 {
		return "", fmt.Errorf("Decision has too many relevant paths")
	}
	for _, path := range paths {
		if path == "" || strings.HasPrefix(path, "/") {
			return "", fmt.Errorf("Decision paths must be repository-relative files")
		}
		for i, part := range strings.Split(path, "/") {
			if part == ".." || (part == "." && i == 0) {
				return "", fmt.Errorf("Decision paths must be repository-relative files")
			}
		}
	}
	if len(paths) == 0 {
		return revision, nil
	}
	args := append([]string{"--literal-pathspecs", "ls-tree", "-r", revision, "--"}, paths...)
	output, err := gitops.Git(ctx, cfg, cfg.Repository, args)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(output)))), nil
}

func (a *App) recordDecisions(ctx context.Context, cfg config.Config, cycle model.Cycle) ([]model.DecisionRecord, error) {
	if cycle.Grounding == nil {
		return nil, errors.New("Decision memory requires cycle grounding")
	}
	accepted := map[string]struct{}{}
	for _, proposal := range cycle.Proposals {
		if proposal.Decision == model.DecisionAccepted {
			key := strings.ToLower(cfg.GitHubRepo) + "\x00" + proposal.Target + "\x00" + proposal.ProblemIdentity()
			accepted[key] = struct{}{}
		}
	}
	records := []model.DecisionRecord{}
	for _, proposal := range cycle.Proposals {
		key := strings.ToLower(cfg.GitHubRepo) + "\x00" + proposal.Target + "\x00" + proposal.ProblemIdentity()
		if proposal.Decision != model.DecisionAccepted {
			if _, absorbed := accepted[key]; absorbed {
				continue
			}
		}
		revision := cycle.Grounding.Revision
		if target, resolveErr := resolveTarget(cfg, cycle.Grounding.PRs, proposal.Target); resolveErr == nil && target != nil {
			revision = target.Head
		}
		fingerprint, err := decisionFingerprint(ctx, cfg, revision, proposal.RelevantPaths)
		if err != nil {
			return nil, err
		}
		records = append(records, model.DecisionRecord{
			ID:                 cycle.ID + ":" + proposal.ID,
			CycleMode:          cycle.Mode,
			Repository:         cfg.GitHubRepo,
			Target:             proposal.Target,
			ProblemKey:         proposal.ProblemIdentity(),
			RelevantPaths:      append([]string{}, proposal.RelevantPaths...),
			Decision:           proposal.Decision,
			Reason:             redact.Text(proposal.Reason),
			SourceRevision:     revision,
			ContextFingerprint: fingerprint,
			ReconsiderAfter:    time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
			CycleID:            cycle.ID,
		})
	}
	return records, nil
}

func validateDecisionMemory(proposals []model.Proposal, memory decisionMemory) error {
	requests := map[string]string{}
	for _, request := range memory.requests {
		if request.ID != "" {
			requests[request.ID] = request.Target
		}
	}
	for _, proposal := range proposals {
		if len(proposal.ProblemKey) > 200 {
			return fmt.Errorf("Proposal %q decision metadata exceeds bounds: problem identity is %d bytes (limit 200; an empty problem_key falls back to the title)", proposal.ID, len(proposal.ProblemKey))
		}
		if len(proposal.Reconsiders) > 100 {
			return fmt.Errorf("Proposal %q decision metadata exceeds bounds: %d reconsiders (limit 100)", proposal.ID, len(proposal.Reconsiders))
		}
		if len(proposal.RelevantPaths) > 40 {
			return fmt.Errorf("Proposal %q decision metadata exceeds bounds: %d relevant_paths (limit 40)", proposal.ID, len(proposal.RelevantPaths))
		}
		for _, id := range proposal.Reconsiders {
			target, ok := requests[id]
			if !ok {
				return fmt.Errorf("Rediscovery identity or target does not match a pending request: proposal %q reconsiders %q, which is not pending", proposal.ID, id)
			}
			if target != proposal.Target {
				return fmt.Errorf("Rediscovery identity or target does not match a pending request: proposal %q targets %q but request %q targets %q", proposal.ID, proposal.Target, id, target)
			}
		}
		if proposal.Decision != model.DecisionAccepted {
			continue
		}
		for _, record := range memory.decisions {
			if record.Target != proposal.Target || record.ProblemKey != proposal.ProblemIdentity() {
				continue
			}
			if record.ReconsiderationDue || (record.CycleMode == model.CycleModeAudit && record.Decision == model.DecisionAccepted) {
				continue
			}
			validRequest := false
			for _, id := range proposal.Reconsiders {
				if requests[id] == proposal.Target {
					validRequest = true
				}
			}
			if !validRequest {
				return fmt.Errorf("Accepted proposal repeats a current recorded decision without an explicit rediscovery request (proposal %q, decision %q)", proposal.ID, record.ID)
			}
		}
	}
	return nil
}
