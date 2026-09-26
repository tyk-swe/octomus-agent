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
	"github.com/tyk-swe/octomus-agent/internal/store"
)

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

func (a *App) planningMemory(ctx context.Context, cfg config.Config, grounding model.Grounding) ([]any, error) {
	raw, err := a.Store.DecisionMemory(cfg.GitHubRepo)
	if err != nil {
		return nil, err
	}
	records := make([]decisionRecord, 0, len(raw))
	for _, value := range raw {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var record decisionRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		if record.ID == "" || record.Repository == "" || record.Target == "" || record.ProblemKey == "" {
			continue
		}
		records = append(records, record)
	}
	memory := make([]any, 0, len(records))
	for _, record := range records {
		if decisionAbsorbed(record, records) {
			continue
		}
		target, err := ResolveTarget(cfg, grounding.PRs, record.Target)
		if err != nil {
			continue
		}
		revision := grounding.Revision
		if target != nil {
			revision = target.Head
		}
		fingerprint, err := decisionFingerprint(ctx, cfg, revision, record.RelevantPaths)
		if err != nil {
			return nil, err
		}
		due := fingerprint != record.ContextFingerprint
		if until, err := time.Parse(time.RFC3339, record.ReconsiderAfter); err == nil && !time.Now().Before(until) {
			due = true
		}
		record.Kind = "decision"
		record.ReconsiderationDue = due
		memory = append(memory, recordToMap(record))
	}
	requests, err := a.Store.RediscoveryRequests(cfg.GitHubRepo)
	if err != nil {
		return nil, err
	}
	for _, value := range requests {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		copy := map[string]any{"kind": "rediscovery"}
		for key, item := range entry {
			copy[key] = item
		}
		memory = append(memory, copy)
	}
	return memory, nil
}

func decisionAbsorbed(record decisionRecord, records []decisionRecord) bool {
	if record.Decision == model.DecisionAccepted {
		return false
	}
	for _, accepted := range records {
		if accepted.Decision == model.DecisionAccepted && accepted.CycleID == record.CycleID &&
			strings.EqualFold(accepted.Repository, record.Repository) && accepted.Target == record.Target &&
			accepted.ProblemKey == record.ProblemKey {
			return true
		}
	}
	return false
}

func rediscoveryRequests(memory []any) []map[string]any {
	result := []map[string]any{}
	for _, value := range memory {
		entry, ok := value.(map[string]any)
		if !ok || entry["kind"] != "rediscovery" {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func decisionFingerprint(ctx context.Context, cfg config.Config, revision string, paths []string) (string, error) {
	if len(paths) == 0 {
		return model.DecisionMemoryFingerprint(revision, paths, "")
	}
	// Validate paths before passing any of them as git pathspecs.
	if _, err := model.DecisionMemoryFingerprint(revision, paths, ""); err != nil {
		return "", err
	}
	// relevant_paths are model-supplied: match them literally, never as
	// pathspec magic such as ":(glob)" or ":!", which ls-tree refuses with a
	// fatal error that would fail the whole plan. Ordinary paths list the same.
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
		if target, resolveErr := ResolveTarget(cfg, cycle.Grounding.PRs, proposal.Target); resolveErr == nil && target != nil {
			revision = target.Head
		}
		fingerprint, err := decisionFingerprint(ctx, cfg, revision, proposal.RelevantPaths)
		if err != nil {
			return nil, err
		}
		record := decisionRecord{
			Kind:               "decision",
			ID:                 cycle.ID + ":" + proposal.ID,
			CycleMode:          cycle.Mode,
			Repository:         cfg.GitHubRepo,
			Target:             proposal.Target,
			ProblemKey:         proposal.ProblemIdentity(),
			RelevantPaths:      append([]string(nil), proposal.RelevantPaths...),
			Decision:           proposal.Decision,
			Reason:             store.Redact(proposal.Reason),
			SourceRevision:     revision,
			ContextFingerprint: fingerprint,
			ReconsiderAfter:    time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
			CycleID:            cycle.ID,
		}
		records = append(records, durableDecisionMap(record))
	}
	return records, nil
}

func durableDecisionMap(record decisionRecord) map[string]any {
	paths := append([]string{}, record.RelevantPaths...)
	return map[string]any{
		"id":                  record.ID,
		"mode":                record.CycleMode,
		"repository":          record.Repository,
		"target":              record.Target,
		"problem_key":         record.ProblemKey,
		"relevant_paths":      paths,
		"decision":            record.Decision,
		"reason":              record.Reason,
		"source_revision":     record.SourceRevision,
		"context_fingerprint": record.ContextFingerprint,
		"reconsider_after":    record.ReconsiderAfter,
		"cycle_id":            record.CycleID,
	}
}

func recordToMap(record decisionRecord) map[string]any {
	value := durableDecisionMap(record)
	value["kind"] = record.Kind
	value["reconsideration_due"] = record.ReconsiderationDue
	return value
}

// ValidateDecisionMemory prevents unchanged rejected or already accepted work
// from silently re-entering the executable queue.
func ValidateDecisionMemory(proposals []model.Proposal, memory []any) error {
	requests := map[string]string{}
	for _, value := range memory {
		entry, ok := value.(map[string]any)
		if !ok || entry["kind"] != "rediscovery" {
			continue
		}
		id, _ := entry["id"].(string)
		target, _ := entry["target"].(string)
		if id != "" {
			requests[id] = target
		}
	}
	for _, proposal := range proposals {
		if len(proposal.ProblemKey) > 200 || len(proposal.Reconsiders) > 100 || len(proposal.RelevantPaths) > 40 {
			return errors.New("Proposal decision metadata exceeds bounds")
		}
		for _, id := range proposal.Reconsiders {
			target, ok := requests[id]
			if !ok || target != proposal.Target {
				return errors.New("Rediscovery identity or target does not match a pending request")
			}
		}
		if proposal.Decision != model.DecisionAccepted {
			continue
		}
		for _, value := range memory {
			entry, ok := value.(map[string]any)
			if !ok || entry["kind"] != "decision" {
				continue
			}
			target, _ := entry["target"].(string)
			problem, _ := entry["problem_key"].(string)
			if target != proposal.Target || problem != proposal.ProblemIdentity() {
				continue
			}
			due, _ := entry["reconsideration_due"].(bool)
			mode := fmt.Sprint(entry["mode"])
			decision, _ := entry["decision"].(string)
			if due || (mode == model.CycleModeAudit.String() && decision == model.DecisionAccepted) {
				continue
			}
			validRequest := false
			for _, id := range proposal.Reconsiders {
				if requests[id] == proposal.Target {
					validRequest = true
				}
			}
			if !validRequest {
				return errors.New("Accepted proposal repeats a current recorded decision without an explicit rediscovery request")
			}
		}
	}
	return nil
}
