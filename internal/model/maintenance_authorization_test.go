package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func authorizedMaintenanceTask() Task {
	cfg := config.Default()
	cfg.DeliveryMode = config.DeliveryModeMaintenance
	cfg.GitHubRepo = "fixture/project"
	cfg.VerificationCommands = []string{"go test ./..."}
	head, base := strings.Repeat("b", 40), strings.Repeat("a", 40)
	lines, files := uint64(500), uint64(10)
	paths := make([]string, files)
	for i := range paths {
		paths[i] = fmt.Sprintf("file-%d.go", i)
	}
	return Task{
		Config: cfg, Proposal: Proposal{Category: "correctness"}, ComparisonBase: base,
		OutputCommit: &head,
		Sessions:     []Session{{ID: "review", Role: "reviewer", Status: SessionCompleted}},
		Reviews: []ReviewRound{{
			SessionID: "review", Revision: head, ComparisonBase: base,
			Result:              Review{Completed: true, Summary: "Reviewed complete maintenance.", Findings: []Finding{}},
			Maintenance:         &MaintenanceAssessment{Qualifies: true, ManualMergeRequired: false, Reason: "Fixes documented existing behavior without expansion."},
			TrustedDiffComplete: true,
		}},
		Verification: []Verification{{Command: cfg.VerificationCommands[0], Success: true, Revision: head}},
		MaintenanceFootprint: &MaintenanceFootprint{
			ComparisonBase: base, Revision: head, ChangedLines: &lines, ChangedFiles: &files,
			Paths: paths, Complete: true, ManualReasons: []string{},
		},
	}
}

func TestMaintenanceMergeAuthorityRequiresEveryRecordedGate(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Task, *config.Config)
	}{
		{"live-standard", func(_ *Task, live *config.Config) { live.DeliveryMode = config.DeliveryModeStandard }},
		{"legacy-standard-task", func(task *Task, _ *config.Config) { task.Config.DeliveryMode = config.DeliveryModeStandard }},
		{"different-repository", func(_ *Task, live *config.Config) { live.GitHubRepo = "other/project" }},
		{"different-branch-prefix", func(_ *Task, live *config.Config) { live.BranchPrefix = "other/" }},
		{"feature-category", func(task *Task, _ *config.Config) { task.Proposal.Category = "features" }},
		{"removed-live-category", func(_ *Task, live *config.Config) { live.Categories = []string{"documentation"} }},
		{"archived", func(task *Task, _ *config.Config) { at := Now(); task.Lifecycle.ArchivedAt = &at }},
		{"missing-output", func(task *Task, _ *config.Config) { task.OutputCommit = nil }},
		{"old-review-head", func(task *Task, _ *config.Config) { task.Reviews[0].Revision = strings.Repeat("c", 40) }},
		{"different-review-base", func(task *Task, _ *config.Config) { task.Reviews[0].ComparisonBase = strings.Repeat("c", 40) }},
		{"unclean-review", func(task *Task, _ *config.Config) { task.Reviews[0].Result.Findings = []Finding{{Title: "Fix it"}} }},
		{"incomplete-review", func(task *Task, _ *config.Config) { task.Reviews[0].Result.Completed = false }},
		{"missing-assessment", func(task *Task, _ *config.Config) { task.Reviews[0].Maintenance = nil }},
		{"not-maintenance", func(task *Task, _ *config.Config) { task.Reviews[0].Maintenance.Qualifies = false }},
		{"empty-assessment-reason", func(task *Task, _ *config.Config) { task.Reviews[0].Maintenance.Reason = " " }},
		{"sensitive-assessment", func(task *Task, _ *config.Config) { task.Reviews[0].Maintenance.ManualMergeRequired = true }},
		{"incomplete-trusted-diff", func(task *Task, _ *config.Config) { task.Reviews[0].TrustedDiffComplete = false }},
		{"missing-reviewer-session", func(task *Task, _ *config.Config) { task.Sessions = nil }},
		{"running-reviewer", func(task *Task, _ *config.Config) { task.Sessions[0].Status = SessionRunning }},
		{"executor-not-reviewer", func(task *Task, _ *config.Config) { task.Sessions[0].Role = "executor" }},
		{"missing-footprint", func(task *Task, _ *config.Config) { task.MaintenanceFootprint = nil }},
		{"unknown-footprint", func(task *Task, _ *config.Config) { task.MaintenanceFootprint.Complete = false }},
		{"unknown-lines", func(task *Task, _ *config.Config) { task.MaintenanceFootprint.ChangedLines = nil }},
		{"unknown-files", func(task *Task, _ *config.Config) { task.MaintenanceFootprint.ChangedFiles = nil }},
		{"wrong-footprint-head", func(task *Task, _ *config.Config) { task.MaintenanceFootprint.Revision = strings.Repeat("c", 40) }},
		{"wrong-footprint-base", func(task *Task, _ *config.Config) { task.MaintenanceFootprint.ComparisonBase = strings.Repeat("c", 40) }},
		{"501-lines", func(task *Task, _ *config.Config) { *task.MaintenanceFootprint.ChangedLines = 501 }},
		{"11-paths", func(task *Task, _ *config.Config) {
			*task.MaintenanceFootprint.ChangedFiles = 11
			task.MaintenanceFootprint.Paths = append(task.MaintenanceFootprint.Paths, "extra.go")
		}},
		{"no-paths", func(task *Task, _ *config.Config) { task.MaintenanceFootprint.Paths = nil }},
		{"manual-footprint", func(task *Task, _ *config.Config) {
			task.MaintenanceFootprint.ManualReasons = []string{"Binary change"}
		}},
		{"new-live-exclusion", func(_ *Task, live *config.Config) { live.AutoMergeExcludedPaths = []string{"file-0.go"} }},
		{"sensitive-recorded-path", func(task *Task, _ *config.Config) {
			task.MaintenanceFootprint.Paths[0] = ".github/workflows/verify.yml"
		}},
		{"stricter-live-lines", func(_ *Task, live *config.Config) { live.AutoMergeMaxLines = 499 }},
		{"stricter-live-files", func(_ *Task, live *config.Config) { live.AutoMergeMaxFiles = 9 }},
		{"zero-live-limit", func(_ *Task, live *config.Config) { live.AutoMergeMaxLines = 0 }},
		{"no-required-commands", func(task *Task, _ *config.Config) { task.Config.VerificationCommands = nil }},
		{"no-verification", func(task *Task, _ *config.Config) { task.Verification = nil }},
		{"failed-verification", func(task *Task, _ *config.Config) { task.Verification[0].Success = false }},
		{"verification-other-head", func(task *Task, _ *config.Config) { task.Verification[0].Revision = strings.Repeat("c", 40) }},
		{"later-failed-verification", func(task *Task, _ *config.Config) {
			task.Verification = append(task.Verification, Verification{Command: task.Config.VerificationCommands[0], Success: false, Revision: *task.OutputCommit})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := authorizedMaintenanceTask()
			live := task.Config.Clone()
			test.mutate(&task, &live)
			if task.MaintenanceMergeAuthorized(live) {
				t.Fatal("Incomplete, mismatched, or manual-only evidence authorized automatic merging")
			}
		})
	}
}

func TestMaintenanceMergeAuthorityAcceptsInclusiveBoundariesAndIgnoresDiscard(t *testing.T) {
	task := authorizedMaintenanceTask()
	if !task.MaintenanceMergeAuthorized(task.Config) {
		t.Fatal("Exact 500-line/10-path maintenance boundary was refused")
	}
	discarded := Now()
	task.Lifecycle.DiscardedAt = &discarded
	if !task.MaintenanceMergeAuthorized(task.Config) {
		t.Fatal("Ordinary workspace retention invalidated frozen reviewed evidence")
	}
	live := task.Config.Clone()
	live.AutoMergeMaxLines, live.AutoMergeMaxFiles = 1000, 20
	*task.MaintenanceFootprint.ChangedLines = 501
	if task.MaintenanceMergeAuthorized(live) {
		t.Fatal("Raising live limits retroactively authorized an originally oversized PR")
	}
}

func TestReviewAuthorityPreservesStandardReviewShape(t *testing.T) {
	task := authorizedMaintenanceTask()
	task.Config.DeliveryMode = config.DeliveryModeStandard
	task.Reviews[0].Maintenance = nil
	task.Sessions = nil
	if !task.ReviewAuthorizes(*task.OutputCommit) {
		t.Fatal("Standard publication acquired a new maintenance assessment/session requirement")
	}
}
