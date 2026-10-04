package engine

import (
	"context"
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

// decisionRecord is a saved decision as the store keeps it, without kind or reconsideration_due; planning sets both
// for the prompt only.
type decisionRecord struct {
	Kind               string          `json:"kind,omitempty"`
	ID                 string          `json:"id"`
	CycleMode          model.CycleMode `json:"mode"`
	Repository         string          `json:"repository"`
	Target             string          `json:"target"`
	ProblemKey         string          `json:"problem_key"`
	RelevantPaths      []string        `json:"relevant_paths"`
	Decision           string          `json:"decision"`
	Reason             string          `json:"reason"`
	SourceRevision     string          `json:"source_revision"`
	ContextFingerprint string          `json:"context_fingerprint"`
	ReconsiderAfter    string          `json:"reconsider_after"`
	CycleID            string          `json:"cycle_id"`
	ReconsiderationDue bool            `json:"reconsideration_due,omitempty"`
}

type rediscoveryRequest struct {
	ID, Target string
	entry      map[string]any
}

type decisionMemory struct {
	decisions []decisionRecord
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
	raw, err := a.Store.DecisionMemory(cfg.GitHubRepo)
	if err != nil {
		return decisionMemory{}, err
	}
	records := make([]decisionRecord, 0, len(raw))
	for _, value := range raw {
		data, err := json.Marshal(value)
		if err != nil {
			return decisionMemory{}, err
		}
		var record decisionRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return decisionMemory{}, err
		}
		if record.ID == "" || record.Repository == "" || record.Target == "" || record.ProblemKey == "" {
			continue
		}
		records = append(records, record)
	}
	memory := decisionMemory{decisions: make([]decisionRecord, 0, len(records)), requests: []rediscoveryRequest{}}
	// Historical decisions often cover the same paths. Reuse successful tree
	// fingerprints for this refresh only; still reevaluate each decision's date.
	type fingerprintKey struct{ revision, paths string }
	fingerprints := make(map[fingerprintKey]string)
	for _, record := range records {
		if decisionAbsorbed(record, records) {
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
			return decisionMemory{}, err
		}
		key := fingerprintKey{revision, string(paths)}
		fingerprint, found := fingerprints[key]
		if !found || ctx.Err() != nil {
			fingerprint, err = decisionFingerprint(ctx, cfg, revision, record.RelevantPaths)
			if err != nil {
				return decisionMemory{}, err
			}
			fingerprints[key] = fingerprint
		}
		due := fingerprint != record.ContextFingerprint
		if until, err := time.Parse(time.RFC3339, record.ReconsiderAfter); err == nil && !time.Now().Before(until) {
			due = true
		}
		record.Kind = "decision"
		record.ReconsiderationDue = due
		memory.decisions = append(memory.decisions, record)
	}
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

func decisionAbsorbed(record decisionRecord, records []decisionRecord) bool {
	if record.Decision == model.DecisionAccepted {
		return false
	}
	for _, accepted := range records {
		if accepted.Decision == model.DecisionAccepted && accepted.CycleID == record.CycleID &&
			config.EqualASCII(accepted.Repository, record.Repository) && accepted.Target == record.Target &&
			accepted.ProblemKey == record.ProblemKey {
			return true
		}
	}
	return false
}

func decisionFingerprint(ctx context.Context, cfg config.Config, revision string, paths []string) (string, error) {
	if len(paths) == 0 {
		return model.DecisionMemoryFingerprint(revision, paths, "")
	}
	if _, err := model.DecisionMemoryFingerprint(revision, paths, ""); err != nil {
		return "", err
	}
	args := []string{"--literal-pathspecs", "ls-tree", "-r", revision, "--"}
	args = append(args, paths...)
	output, err := gitops.Git(ctx, cfg, cfg.Repository, args)
	if err != nil {
		return "", err
	}
	return model.DecisionMemoryFingerprint(revision, paths, output)
}

func (a *App) recordDecisions(ctx context.Context, cfg config.Config, cycle model.Cycle) ([]any, error) {
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
	records := []any{}
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
		record, err := wirejson.GenericMap(decisionRecord{
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
		if err != nil {
			return nil, err
		}
		records = append(records, record)
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
