package model

import (
	"slices"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func (t Task) ReviewAuthorizes(revision string) bool {
	if revision == "" || len(t.Reviews) == 0 {
		return false
	}
	round := t.Reviews[len(t.Reviews)-1]
	if round.Revision != revision || !round.Result.Clean() {
		return false
	}
	if t.Config.DeliveryMode == config.DeliveryModeStandard {
		return true
	}
	if t.Config.DeliveryMode != config.DeliveryModeMaintenance ||
		t.Proposal.Category == "features" ||
		!slices.Contains(t.Config.EffectiveCategories(), t.Proposal.Category) ||
		t.ComparisonBase == "" || round.ComparisonBase != t.ComparisonBase ||
		round.Maintenance == nil || !round.Maintenance.Qualifies ||
		strings.TrimSpace(round.Maintenance.Reason) == "" {
		return false
	}
	for _, session := range t.Sessions {
		if session.ID == round.SessionID && session.Role == "reviewer" &&
			session.Status == SessionCompleted {
			return true
		}
	}
	return false
}

func (t Task) MaintenanceMergeAuthorized(live config.Config) bool {
	if live.DeliveryMode != config.DeliveryModeMaintenance ||
		t.Config.DeliveryMode != config.DeliveryModeMaintenance ||
		!live.SameRemoteIdentity(t.Config) || live.BranchPrefix != t.Config.BranchPrefix ||
		t.OutputCommit == nil || t.Lifecycle.ArchivedAt != nil ||
		!slices.Contains(live.EffectiveCategories(), t.Proposal.Category) ||
		!t.ReviewAuthorizes(*t.OutputCommit) {
		return false
	}
	round := t.Reviews[len(t.Reviews)-1]
	if !round.TrustedDiffComplete || round.Maintenance.ManualMergeRequired {
		return false
	}
	footprint := t.MaintenanceFootprint
	if footprint == nil || !footprint.Complete ||
		footprint.ComparisonBase != t.ComparisonBase || footprint.Revision != *t.OutputCommit ||
		footprint.ChangedLines == nil || footprint.ChangedFiles == nil ||
		*footprint.ChangedFiles == 0 || uint64(len(footprint.Paths)) != *footprint.ChangedFiles ||
		len(footprint.ManualReasons) != 0 {
		return false
	}
	for _, name := range footprint.Paths {
		if config.ManualMergePath(name, live.AutoMergeExcludedPaths) {
			return false
		}
	}
	maxLines, maxFiles := AutoMergeLimits(t.Config, live)
	if maxLines == 0 || maxFiles == 0 ||
		*footprint.ChangedLines > maxLines || *footprint.ChangedFiles > maxFiles {
		return false
	}
	commands := t.ExecutionConfig().VerificationCommands
	if len(commands) == 0 {
		return false
	}
	for _, command := range commands {
		result := t.VerificationFor(command).Latest
		if result == nil || !result.Success || result.Revision != *t.OutputCommit {
			return false
		}
	}
	return true
}

// AutoMergeLimits is the effective automatic-merge footprint bound: the tighter of
// the saved and live limits, each zero when either side leaves it unset.
func AutoMergeLimits(saved, live config.Config) (lines, files uint64) {
	return min(saved.AutoMergeMaxLines, live.AutoMergeMaxLines),
		min(saved.AutoMergeMaxFiles, live.AutoMergeMaxFiles)
}
