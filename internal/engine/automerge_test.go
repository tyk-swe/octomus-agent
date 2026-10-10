package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	gitops "github.com/tyk-swe/octomus-agent/internal/git"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/runner/runnertest"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func maintenanceReview(qualifies, manual bool) string {
	return fmt.Sprintf(`{"review":{"completed":true,"summary":"Reviewed the complete diff; no actionable findings remain.","findings":[]},"maintenance":{"qualifies":%t,"manual_merge_required":%t,"reason":"Restores documented behavior without expanding scope."}}`, qualifies, manual)
}

func maintenanceFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) {
		cfg.DeliveryMode = config.DeliveryModeMaintenance
		cfg.VerificationCommands = []string{"grep -q fixed feature.txt"}
	})
	return f
}

func maintenanceTask(t *testing.T, f *fixture, review string, effect func(string) error) model.Task {
	t.Helper()
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Documented the existing behavior", Effect: effect})
	script.Answer(routes.Reviewer, review)
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Proposal.Category = "documentation"
	putTask(t, f, task)
	return task
}

func prObservation(t *testing.T, f *fixture, number uint64) model.PRObservation {
	t.Helper()
	prs, err := store.List[model.PRObservation](f.state, "pr")
	if err != nil {
		t.Fatal(err)
	}
	for _, observation := range prs {
		if observation.PR.Number == number {
			return observation
		}
	}
	t.Fatalf("no PR observation for %d: %+v", number, prs)
	return model.PRObservation{}
}

func driveMerge(t *testing.T, f *fixture, app *App, number uint64, wanted ...model.AutoMergeStatus) model.AutoMergeState {
	t.Helper()
	deadline := time.Now().Add(taskWaitTimeout)
	for time.Now().Before(deadline) {
		observation := prObservation(t, f, number)
		if observation.AutoMerge != nil {
			for _, status := range wanted {
				if observation.AutoMerge.Status == status {
					return *observation.AutoMerge
				}
			}
		}
		app.runtimeMu.Lock()
		app.runtime.lastMergeCheck = time.Now().Add(-autoMergeInterval)
		app.runtimeMu.Unlock()
		if err := app.tick(); err != nil {
			t.Fatalf("tick during merge drive: %v", err)
		}
		app.wg.Wait()
		time.Sleep(10 * time.Millisecond)
	}
	observation := prObservation(t, f, number)
	t.Fatalf("merge evidence did not reach %v: %+v", wanted, observation.AutoMerge)
	return model.AutoMergeState{}
}

func mergeAttempts(t *testing.T, f *fixture) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "merge-attempts.jsonl"))
	if err != nil {
		return nil
	}
	attempts := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, entry)
	}
	return attempts
}

func patchPRs(t *testing.T, f *fixture, patch map[string]any) {
	t.Helper()
	path := filepath.Join(f.root, "prs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var prs []map[string]any
	if err := json.Unmarshal(data, &prs); err != nil {
		t.Fatal(err)
	}
	for i := range prs {
		for key, value := range patch {
			prs[i][key] = value
		}
	}
	out, err := json.Marshal(prs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePRPatch(t *testing.T, f *fixture, patch map[string]any) {
	t.Helper()
	data, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "pr-create-patch.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceDeliveryFreezesFootprint(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	task := maintenanceTask(t, f, maintenanceReview(true, false), writeFile("feature.txt", "fixed\n"))
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if saved.Status != model.StatusPublished || saved.PRNumber == nil {
		t.Fatalf("maintenance task did not publish: %s %+v", saved.Status, saved.Error)
	}
	if saved.MaintenanceFootprint == nil || !saved.MaintenanceFootprint.Complete ||
		saved.MaintenanceFootprint.ChangedLines == nil || *saved.MaintenanceFootprint.ChangedLines != 1 ||
		len(saved.MaintenanceFootprint.Paths) != 1 || saved.MaintenanceFootprint.Paths[0] != "feature.txt" {
		t.Fatalf("frozen footprint = %+v", saved.MaintenanceFootprint)
	}
	round := saved.Reviews[len(saved.Reviews)-1]
	if round.Maintenance == nil || !round.Maintenance.Qualifies || !round.TrustedDiffComplete {
		t.Fatalf("latest review evidence = %+v", round)
	}
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge == nil || !merge.Authorized || merge.Status != model.AutoMergeWaiting ||
		merge.Head != *saved.OutputCommit || merge.ComparisonBase != saved.ComparisonBase ||
		merge.Footprint == nil || !merge.Footprint.Complete {
		t.Fatalf("initial merge evidence = %+v", merge)
	}
}

func TestMaintenanceMergeControllerSquashMerges(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	task := maintenanceTask(t, f, maintenanceReview(true, false), writeFile("feature.txt", "fixed\n"))
	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task did not publish: %s", saved.Status)
	}

	merged := driveMerge(t, f, app, *saved.PRNumber, model.AutoMergeMerged)
	if merged.MergeCommit == nil || merged.ResultSource == nil || *merged.ResultSource != store.MergeResultConfirmed {
		t.Fatalf("merged evidence = %+v", merged)
	}
	attempts := mergeAttempts(t, f)
	if len(attempts) != 1 || attempts[0]["sha"] != *saved.OutputCommit || attempts[0]["method"] != "squash" {
		t.Fatalf("merge attempts = %+v", attempts)
	}
	remote := filepath.Join(f.root, "remote.git")
	head := git(t, f.root, "--git-dir", remote, "rev-parse", "main")
	if head != *merged.MergeCommit {
		t.Fatalf("remote main = %s; want squash commit %s", head, *merged.MergeCommit)
	}
	if parents := strings.Fields(git(t, f.root, "--git-dir", remote, "rev-list", "--parents", "-n", "1", "main")); len(parents) != 2 {
		t.Fatalf("squash commit parents = %v", parents)
	}
	if content := git(t, f.root, "--git-dir", remote, "show", head+":feature.txt"); content != "fixed" {
		t.Fatalf("merged content = %q", content)
	}
	prs := prsJSON(t, f)
	if len(prs) != 1 || prs[0]["state"] != "merged" {
		t.Fatalf("fixture PR state = %+v", prs)
	}
	if prObservation(t, f, *saved.PRNumber).PR.State != "merged" {
		t.Fatal("the durable observation did not record the merged state")
	}
}

func TestArchivingPublishedTaskKeepsConfirmedMergeEvidence(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	task := maintenanceTask(t, f, maintenanceReview(true, false), writeFile("feature.txt", "fixed\n"))
	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task did not publish: %s", saved.Status)
	}
	merged := driveMerge(t, f, app, *saved.PRNumber, model.AutoMergeMerged)
	if merged.ResultSource == nil || *merged.ResultSource != store.MergeResultConfirmed || merged.MergeCommit == nil {
		t.Fatalf("merge evidence = %+v", merged)
	}
	observedAt := merged.ObservedAt
	commit := *merged.MergeCommit

	must0(t, app.TaskAction(saved.ID, "archive"))
	if archived := loadTask(t, f.state, saved.ID); archived.Lifecycle.ArchivedAt == nil {
		t.Fatalf("the published task was not archived: %+v", archived)
	}

	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded == nil || recorded.Status != model.AutoMergeMerged || recorded.Authorized ||
		recorded.ResultSource == nil || *recorded.ResultSource != store.MergeResultConfirmed ||
		recorded.MergeCommit == nil || *recorded.MergeCommit != commit ||
		recorded.Reason != "Squash merged by Octomus" || recorded.ObservedAt != observedAt {
		t.Fatalf("archival rewrote the confirmed merge: %+v", recorded)
	}

	observation := prObservation(t, f, *saved.PRNumber)
	must0(t, f.state.RecordPRObservation(observation.Repository, observation.PR, false))
	reobserved := prObservation(t, f, *saved.PRNumber).AutoMerge
	if reobserved == nil || reobserved.Status != model.AutoMergeMerged ||
		reobserved.ResultSource == nil || *reobserved.ResultSource != store.MergeResultConfirmed ||
		reobserved.Reason != "Squash merged by Octomus" ||
		reobserved.MergeCommit == nil || *reobserved.MergeCommit != commit {
		t.Fatalf("a same-head observation downgraded confirmed provenance: %+v", reobserved)
	}
	counts, err := f.state.MergeCounts(f.cfg.GitHubRepo)
	must0(t, err)
	if counts["merged"] != 1 || len(counts) != 1 {
		t.Fatalf("merge counts after archival = %+v", counts)
	}
}

func TestMaintenanceMergeWaitsForPendingChecks(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	task := maintenanceTask(t, f, maintenanceReview(true, false), writeFile("feature.txt", "fixed\n"))
	writePRPatch(t, f, map[string]any{"check_status": "EXPECTED"})
	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task did not publish: %s", saved.Status)
	}

	app.runtimeMu.Lock()
	app.runtime.lastMergeCheck = time.Now().Add(-autoMergeInterval)
	app.runtimeMu.Unlock()
	if err := app.tick(); err != nil {
		t.Fatal(err)
	}
	app.wg.Wait()
	waiting := prObservation(t, f, *saved.PRNumber).AutoMerge
	if waiting == nil || waiting.Status != model.AutoMergeWaiting ||
		!strings.Contains(waiting.Reason, "Waiting for checks") {
		t.Fatalf("pending-check evidence = %+v", waiting)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a merge attempt ran while checks were pending")
	}
	patchPRs(t, f, map[string]any{"check_status": "SUCCESS"})
	merged := driveMerge(t, f, app, *saved.PRNumber, model.AutoMergeMerged)
	if merged.MergeCommit == nil {
		t.Fatalf("post-check merge = %+v", merged)
	}
}

func TestMaintenanceMergeNegativeGatesStayManual(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		patch      map[string]any
		reason     string
		authorized bool
	}{
		{"failed-checks", map[string]any{"check_status": "FAILURE"}, "Checks failed", true},
		{"changes-requested", map[string]any{"review_decision": "CHANGES_REQUESTED"}, "requested changes", true},
		{"conflicting", map[string]any{"mergeability": "CONFLICTING"}, "conflicts", true},
		{"merge-queue", map[string]any{"merge_queue": true}, "merge queue", true},
		{"draft", map[string]any{"is_draft": true}, "draft", true},
		{"no-squash", map[string]any{"squash_merge_allowed": false}, "squash", true},
		{"oversized-report", map[string]any{"merge_additions": 9001, "merge_deletions": 0, "merge_changed_files": 2}, "footprint exceeds", true},
		{"malformed-stats", map[string]any{"merge_additions": -4}, "footprint", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := maintenanceFixture(t)
			task := maintenanceTask(t, f, maintenanceReview(true, false), writeFile("feature.txt", "fixed\n"))
			writePRPatch(t, f, test.patch)
			app := f.newApp(t)
			saved := driveTask(t, f, app, task.ID)
			if saved.Status != model.StatusPublished {
				t.Fatalf("task did not publish: %s", saved.Status)
			}
			settled := driveMerge(t, f, app, *saved.PRNumber, model.AutoMergeManual)
			if !strings.Contains(settled.Reason, test.reason) {
				t.Fatalf("%s outcome = %+v", test.name, settled)
			}
			if settled.Authorized != test.authorized {
				t.Fatalf("%s authorized = %v; want %v", test.name, settled.Authorized, test.authorized)
			}
			if len(mergeAttempts(t, f)) != 0 {
				t.Fatalf("%s reached the merge mutation", test.name)
			}
		})
	}
}

func TestMaintenanceMergeConditionalKeepsAuthorization(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	task := maintenanceTask(t, f, maintenanceReview(true, false), writeFile("feature.txt", "fixed\n"))
	writePRPatch(t, f, map[string]any{"check_status": "FAILURE"})
	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	settled := driveMerge(t, f, app, *saved.PRNumber, model.AutoMergeManual)
	if !settled.Authorized || !strings.Contains(settled.Reason, "Checks failed") {
		t.Fatalf("conditional manual = %+v", settled)
	}
	patchPRs(t, f, map[string]any{"check_status": "SUCCESS"})
	merged := driveMerge(t, f, app, *saved.PRNumber, model.AutoMergeMerged)
	if merged.MergeCommit == nil {
		t.Fatalf("continuous recheck did not merge: %+v", merged)
	}
}

func TestMaintenanceEvidenceStaysManual(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		review string
		effect func(string) error
	}{
		{"manual-assessment", maintenanceReview(true, true), writeFile("feature.txt", "fixed\n")},
		{"sensitive-path", maintenanceReview(true, false), func(cwd string) error {
			if err := os.MkdirAll(filepath.Join(cwd, ".github", "workflows"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(cwd, ".github", "workflows", "checks.yml"), []byte("name: checks\n"), 0o644); err != nil {
				return err
			}
			return writeFile("feature.txt", "fixed\n")(cwd)
		}},
		{"negative-assessment", maintenanceReview(false, false), writeFile("feature.txt", "fixed\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := maintenanceFixture(t)
			task := maintenanceTask(t, f, test.review, test.effect)
			app := f.newApp(t)
			saved := driveTask(t, f, app, task.ID)
			if test.name == "negative-assessment" {
				if saved.Status == model.StatusPublished {
					t.Fatal("a non-qualifying assessment published")
				}
				return
			}
			if saved.Status != model.StatusPublished {
				t.Fatalf("manual-only maintenance must still publish: %s", saved.Status)
			}
			merge := prObservation(t, f, *saved.PRNumber).AutoMerge
			if merge == nil || merge.Status != model.AutoMergeManual ||
				merge.Authorized || merge.Reason == "" {
				t.Fatalf("%s merge evidence = %+v", test.name, merge)
			}
			if len(mergeAttempts(t, f)) != 0 {
				t.Fatalf("%s reached the merge mutation", test.name)
			}
		})
	}
}

func TestStandardDeliveryRecordsNoMergeEvidence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.configure(t, func(cfg *config.Config) { cfg.VerificationCommands = []string{"grep -q fixed feature.txt"} })
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Created feature.txt", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, cleanReview("clean"))
	task := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, task)

	saved := driveTask(t, f, f.newApp(t), task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task did not publish: %s", saved.Status)
	}
	if saved.MaintenanceFootprint != nil {
		t.Fatalf("standard delivery recorded a footprint: %+v", saved.MaintenanceFootprint)
	}
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge != nil {
		t.Fatalf("standard delivery recorded merge evidence: %+v", merge)
	}
	round := saved.Reviews[len(saved.Reviews)-1]
	if round.Maintenance != nil {
		t.Fatalf("standard review recorded a maintenance assessment: %+v", round.Maintenance)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("standard delivery attempted a merge")
	}
}

func TestMaintenanceProposalRejection(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	cfg := f.cfg.Clone()
	grounding := model.Grounding{Revision: "ground"}
	if err := validateProposals(cfg, []model.Proposal{proposal("d0-feature", "main")}, grounding, nil); err == nil ||
		!strings.Contains(err.Error(), "ineligible category") {
		t.Fatalf("features proposal under maintenance = %v", err)
	}
	eligible := proposal("d0-fix", "main")
	eligible.Category = "correctness"
	if err := validateProposals(cfg, []model.Proposal{eligible}, grounding, nil); err != nil {
		t.Fatalf("maintenance proposal rejected: %v", err)
	}
	standard := queuedTask(cfg, "queued-feature", "main", "octomus/queued-feature")
	if err := validateTaskPlan([]model.Task{standard}, cfg); err == nil {
		t.Fatal("a features task passed the live maintenance gate")
	}
}

func TestMergeReadinessClassification(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t.TempDir())
	cfg.DeliveryMode = config.DeliveryModeMaintenance
	head := strings.Repeat("a", 40)
	task := model.Task{Branch: "octomus/work", Config: cfg}
	base := func() gitops.MergeStatus {
		success := "SUCCESS"
		return gitops.MergeStatus{
			State: "open", HeadBranch: "octomus/work", Head: head, HeadRepository: "fixture/project",
			BaseBranch: "main", BaseOID: strings.Repeat("b", 40), Repository: "fixture/project",
			SquashAllowed: true, Mergeable: "MERGEABLE", MergeState: "CLEAN",
			CheckState: &success, CheckContexts: 1,
			Additions: new(uint64(3)), Deletions: new(uint64(1)), ChangedFiles: new(uint64(2)),
		}
	}
	for _, test := range []struct {
		name    string
		mutate  func(status *gitops.MergeStatus)
		reason  string
		waiting bool
	}{
		{"ready", func(s *gitops.MergeStatus) {}, "", false},
		{"draft", func(s *gitops.MergeStatus) { s.Draft = true }, "draft", false},
		{"queue", func(s *gitops.MergeStatus) { s.MergeQueue = true }, "merge queue", false},
		{"no-squash", func(s *gitops.MergeStatus) { s.SquashAllowed = false }, "squash", false},
		{"changes-requested", func(s *gitops.MergeStatus) { v := "CHANGES_REQUESTED"; s.ReviewDecision = &v }, "requested changes", false},
		{"review-required", func(s *gitops.MergeStatus) { v := "REVIEW_REQUIRED"; s.ReviewDecision = &v }, "Waiting for a required pull request review", true},
		{"checks-pending", func(s *gitops.MergeStatus) { v := "PENDING"; s.CheckState = &v }, "Waiting for checks", true},
		{"checks-failed", func(s *gitops.MergeStatus) { v := "FAILURE"; s.CheckState = &v }, "Checks failed", false},
		{"checks-absent", func(s *gitops.MergeStatus) { s.CheckState = nil }, "Waiting for check evidence", true},
		{"checks-empty", func(s *gitops.MergeStatus) { s.CheckContexts = 0 }, "Waiting for check evidence", true},
		{"conflicts", func(s *gitops.MergeStatus) { s.Mergeable = "CONFLICTING" }, "conflicts", false},
		{"mergeable-unknown", func(s *gitops.MergeStatus) { s.Mergeable = "UNKNOWN" }, "Waiting for GitHub to report mergeability", true},
		{"behind", func(s *gitops.MergeStatus) { s.MergeState = "BEHIND" }, "protections", false},
		{"unstable", func(s *gitops.MergeStatus) { s.MergeState = "UNSTABLE" }, "Waiting for checks", true},
		{"stats-absent", func(s *gitops.MergeStatus) { s.Additions = nil }, "footprint", false},
		{"stats-over-limit", func(s *gitops.MergeStatus) { s.Additions = new(uint64(9001)) }, "exceeds", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := base()
			test.mutate(&status)
			reason, waiting := mergeReadiness(task, cfg, status)
			if test.reason != "" && !strings.Contains(reason, test.reason) {
				t.Fatalf("reason = %q; want it to contain %q", reason, test.reason)
			}
			if waiting != test.waiting {
				t.Fatalf("waiting = %v; want %v (reason %q)", waiting, test.waiting, reason)
			}
		})
	}
}

func TestMaintenanceNegativeAssessmentTakesBoundedRepair(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	routes, script := f.routes, f.script
	script.Queue(routes.Executor, runnertest.Reply{Answer: "Documented the existing behavior", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(routes.Reviewer, maintenanceReview(false, false))
	script.Queue(routes.Repair, runnertest.Reply{Answer: "Tightened the change to documented behavior"})
	script.Answer(routes.Reviewer, maintenanceReview(true, false))
	task := executionTask(t, f, f.cfg.DefaultBranch)
	task.Proposal.Category = "documentation"
	putTask(t, f, task)

	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("bounded maintenance repair did not publish: %s %+v", saved.Status, saved.Error)
	}
	if repairs := script.Turns(routes.Repair); len(repairs) != 1 {
		t.Fatalf("clean ordinary findings bypassed repair: %d repair turns", len(repairs))
	}
	merged := driveMerge(t, f, app, *saved.PRNumber, model.AutoMergeMerged)
	if merged.MergeCommit == nil {
		t.Fatalf("repaired maintenance delivery = %+v", merged)
	}
}

func TestPublicationCheckpointReplayKeepsFrozenEvidence(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	frozen := saved.MaintenanceFootprint.Clone()

	checkpoint := saved.Clone()
	checkpoint.Status = model.StatusPublishing
	must0(t, f.state.Put("task", saved.ID, checkpoint))
	replay := saved.Clone()
	if err := app.published(&replay, observation.PR); err != nil {
		t.Fatalf("checkpoint replay failed: %v", err)
	}
	evidence := prObservation(t, f, *saved.PRNumber).AutoMerge
	if evidence == nil || evidence.Footprint == nil ||
		*evidence.Footprint.ChangedLines != *frozen.ChangedLines ||
		*evidence.Footprint.ChangedFiles != *frozen.ChangedFiles ||
		len(evidence.Footprint.Paths) != len(frozen.Paths) ||
		evidence.Status != model.AutoMergeWaiting || evidence.Head != frozen.Revision {
		t.Fatalf("replayed evidence = %+v; frozen = %+v", evidence, frozen)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("the replay issued a merge mutation without new authority gates")
	}

	moved := checkpoint
	otherHead := "c" + strings.Repeat("0", 39)
	moved.OutputCommit = &otherHead
	must0(t, f.state.Put("task", saved.ID, moved))
	stale := saved.Clone()
	stale.Status = model.StatusPublished
	if err := app.published(&stale, observation.PR); err == nil {
		t.Fatal("a stale output commit overwrote a newer checkpoint")
	}
	current, err := store.Get[model.Task](f.state, "task", saved.ID)
	if err != nil || current == nil || *current.OutputCommit != otherHead ||
		current.Status != model.StatusPublishing {
		t.Fatalf("the refused write changed the checkpoint: %+v", current)
	}
}

func publishedMaintenanceTask(t *testing.T, f *fixture) (model.Task, model.PRObservation) {
	t.Helper()
	task := maintenanceTask(t, f, maintenanceReview(true, false), writeFile("feature.txt", "fixed\n"))
	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	if saved.Status != model.StatusPublished {
		t.Fatalf("task did not publish: %s", saved.Status)
	}
	return saved, prObservation(t, f, *saved.PRNumber)
}

func TestMergeAttemptFinalGateRejectsStaleState(t *testing.T) {
	f := maintenanceFixture(t)
	writePRPatch(t, f, map[string]any{"merge_queue": true})
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	if merge := observation.AutoMerge; merge == nil || merge.Status != model.AutoMergeWaiting {
		t.Fatalf("initial evidence = %+v", merge)
	}

	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.Paused = true
	if err := f.state.SaveControl(control); err != nil {
		t.Fatal(err)
	}
	app.mergeAttempt(t.Context(), saved, observation, false)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("paused mode attempted: %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a paused attempt reached the mutation")
	}

	control.Paused = false
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))
	mutated := saved.Clone()
	otherHead := "b" + strings.Repeat("0", 39)
	mutated.OutputCommit = &otherHead
	if err := f.state.Put("task", saved.ID, mutated); err != nil {
		t.Fatal(err)
	}
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a stale canonical head reached the mutation")
	}
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("the stale attempt mutated the record: %+v", merge)
	}
	mutated.OutputCommit = saved.OutputCommit
	mutated.Reviews[len(mutated.Reviews)-1].Result.Findings = []model.Finding{{Title: "Post-check defect", Detail: "A finding recorded after the remote precheck", Priority: "P1"}}
	if err := f.state.Put("task", saved.ID, mutated); err != nil {
		t.Fatal(err)
	}
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a revoked canonical review reached the mutation")
	}
	mutated.Reviews[len(mutated.Reviews)-1].Result.Findings = []model.Finding{}
	if err := f.state.Put("task", saved.ID, mutated); err != nil {
		t.Fatal(err)
	}

	archived := saved.Clone()
	archived.Lifecycle.ArchivedAt = new(model.Now())
	must0(t, f.state.Put("task", saved.ID, archived))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("an archived authorizing task reached the mutation")
	}
	archived.Lifecycle.ArchivedAt = nil
	must0(t, f.state.Put("task", saved.ID, archived))

	revoked := saved.Clone()
	revoked.Verification[len(revoked.Verification)-1].Success = false
	must0(t, f.state.Put("task", saved.ID, revoked))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a revoked canonical verification reached the mutation")
	}
	must0(t, f.state.Put("task", saved.ID, saved.Clone()))

	must0(t, f.state.MarkCancel(saved.ID))
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a cancellation marker at the seam reached the mutation")
	}
	must0(t, f.state.ClearCancel(saved.ID))

	f.configure(t, func(cfg *config.Config) { cfg.AutoMergeExcludedPaths = []string{"feature.txt"} })
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a tightened live exclusion reached the mutation")
	}
	f.configure(t, func(cfg *config.Config) { cfg.AutoMergeExcludedPaths = []string{} })

	original := unfinishedBranchWork
	unfinishedBranchWork = func(_ *store.Store, _, _, _ string) (bool, error) {
		return false, fmt.Errorf("injected branch check failure")
	}
	defer func() { unfinishedBranchWork = original }()
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a failed same-branch check reached the mutation")
	}
	unfinishedBranchWork = func(_ *store.Store, _, _, _ string) (bool, error) {
		return true, nil
	}
	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("queued same-branch work inserted at the seam still merged")
	}
	unfinishedBranchWork = original
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("the refused attempt mutated the record: %+v", merge)
	}
}

func TestMergeAttemptRequiresAuthorizingRun(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeRunOnce)
	control.Batch = &model.RunBatch{ID: "batch-current", Phase: model.BatchPhaseMerging}
	must0(t, f.state.SaveControl(control))

	app.mergeAttempt(t.Context(), saved, observation, true)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("a task outside the run attempted: %+v", merge)
	}
	taskRun := "batch-other"
	mutated := saved.Clone()
	mutated.RunID = &taskRun
	must0(t, f.state.Put("task", saved.ID, mutated))
	app.mergeAttempt(t.Context(), mutated, observation, true)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("a foreign-run task attempted: %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a non-authorizing run reached the mutation")
	}
}

func TestMergeAttemptPausedDuringPutSerializes(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	if err := os.WriteFile(filepath.Join(f.root, "merge-hold"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.mergeAttempt(t.Context(), saved, observation, false)
	}()
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "merge-entered"))
		return err == nil
	}) {
		t.Fatal("the merge request did not reach the hold")
	}
	paused := make(chan error, 1)
	go func() {
		_, err := app.ControlAction("pause")
		paused <- err
	}()
	if err := os.Remove(filepath.Join(f.root, "merge-hold")); err != nil {
		t.Fatal(err)
	}
	<-done
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	merged := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merged.Status != model.AutoMergeMerged || merged.MergeCommit == nil {
		t.Fatalf("in-flight merge did not complete: %+v", merged)
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %d; want exactly one", len(mergeAttempts(t, f)))
	}
	app.mergeCandidate(t.Context(), f.cfg, false, true, false, nil, prObservation(t, f, *saved.PRNumber))
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatal("a paused pass wrote after the in-flight merge")
	}
}

func TestMergeLostAckRestartObservedMerged(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	if err := os.WriteFile(filepath.Join(f.root, "merge-after-hold"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.mergeAttempt(app.ctx, saved, observation, false)
	}()
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "merge-written"))
		return err == nil
	}) {
		t.Fatal("the merge request did not reach the after-write hold")
	}
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerging || recorded.AttemptID == nil {
		t.Fatalf("durable intent = %+v", recorded)
	}
	app.Shutdown()
	<-done
	if err := os.Remove(filepath.Join(f.root, "merge-after-hold")); err != nil {
		t.Fatal(err)
	}
	restarted := f.pausedApp(t)
	driveMerge(t, f, restarted, *saved.PRNumber, model.AutoMergeMerged)
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.ResultSource == nil || *settled.ResultSource != store.MergeResultObserved {
		t.Fatalf("the restarted pass did not read-settle the lost acknowledgement: %+v", settled)
	}
	if prObservation(t, f, *saved.PRNumber).PR.State != "merged" {
		t.Fatal("the observed outcome did not update the saved PR state")
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %+v; want exactly the original PUT", mergeAttempts(t, f))
	}
}

func TestMergeInterruptedBeforeWriteRetriesOnce(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	merge := *observation.AutoMerge
	inflight := merge.Clone()
	inflight.Status = model.AutoMergeMerging
	inflight.AttemptID = new("attempt-1")
	inflight.AttemptedAt = new(model.Now())
	claimed, err := f.state.ClaimMerge(observation.Repository, observation.PR.Number, inflight, false)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	current := prObservation(t, f, *saved.PRNumber)
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, current)
	released := prObservation(t, f, *saved.PRNumber).AutoMerge
	if released.Status != model.AutoMergeWaiting || released.AttemptID != nil || released.AttemptedAt == nil {
		t.Fatalf("interrupted intent = %+v", released)
	}
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, prObservation(t, f, *saved.PRNumber))
	merged := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merged.Status != model.AutoMergeMerged {
		t.Fatalf("retry outcome = %+v", merged)
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %+v; want exactly one PUT", mergeAttempts(t, f))
	}
}

func TestMergeHeadMovedDuringHoldRejectedByServer(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	initial := remoteHead(t, f, "main")
	if err := os.WriteFile(filepath.Join(f.root, "merge-hold"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.mergeAttempt(t.Context(), saved, observation, false)
	}()
	if !testutil.WaitUntil(30*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(f.root, "merge-entered"))
		return err == nil
	}) {
		t.Fatal("the merge request did not reach the hold")
	}
	work := filepath.Join(f.root, "push-move")
	git(t, f.root, "clone", f.repo, work)
	git(t, work, "config", "user.name", "Fixture")
	git(t, work, "config", "user.email", "fixture@example.com")
	git(t, work, "checkout", "-b", "octomus/moved")
	if err := os.WriteFile(filepath.Join(work, "moved.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "moved head")
	git(t, work, "push", "-f", filepath.Join(f.root, "remote.git"), "octomus/moved:"+saved.Branch)
	if err := os.Remove(filepath.Join(f.root, "merge-hold")); err != nil {
		t.Fatal(err)
	}
	<-done
	if head := remoteHead(t, f, "main"); head != initial {
		t.Fatalf("the remote merged despite the moved head: %s -> %s", initial, head)
	}
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeManual || recorded.Authorized || recorded.MergeCommit != nil {
		t.Fatalf("a moved-head write was not refused: %+v", recorded)
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 1 {
		t.Fatalf("attempts = %+v; want exactly one refused request", attempts)
	}
}

func TestMergeReconcileExternallyMerged(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)

	patchPRs(t, f, map[string]any{"state": "merged", "merged_at": "2026-10-01T00:00:00Z", "merge_commit_sha": "c" + strings.Repeat("0", 39)})
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerged || recorded.ResultSource == nil || *recorded.ResultSource != store.MergeResultObserved {
		t.Fatalf("externally merged outcome = %+v", recorded)
	}
	if prObservation(t, f, *saved.PRNumber).PR.State != "merged" {
		t.Fatal("PR state did not record the merged outcome")
	}

	patchPRs(t, f, map[string]any{"state": "closed", "merged_at": nil, "merge_commit_sha": nil})
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, nil, prObservation(t, f, *saved.PRNumber))
	recorded = prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerged {
		t.Fatalf("a terminal merged record was downgraded: %+v", recorded)
	}
}

func TestMergeCandidateForeignBatchSkipped(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, new("other-run"), observation)
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("foreign-batch candidate was processed: %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("foreign-batch candidate reached the mutation")
	}
}

func TestMergeWaitingSettlesManualWhenModeOff(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	standard := f.cfg.Clone()
	standard.DeliveryMode = config.DeliveryModeStandard
	app.mergeCandidate(t.Context(), standard, true, true, false, nil, observation)
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge.Status != model.AutoMergeManual || merge.Authorized {
		t.Fatalf("disabled-mode evidence = %+v", merge)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a disabled mode reached the mutation")
	}
}

func TestMergePacingAndFairPaging(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	writePRPatch(t, f, map[string]any{"check_status": "EXPECTED"})
	publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	if first := statusReads(t, f); first != 1 {
		t.Fatalf("one pending candidate used %d status reads; want exactly 1", first)
	}
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	if reads := statusReads(t, f); reads != 1 {
		t.Fatalf("an immediate second tick re-read the remote: %d reads", reads)
	}
	forceMergeCheck(app)
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	if reads := statusReads(t, f); reads != 2 {
		t.Fatalf("the forced pass did not re-read exactly once: %d reads", reads)
	}

	for i := range 12 {
		waitingCandidate(t, f.state, uint64(100+i), "missing-task-"+model.ID(), "9"+strings.Repeat("0", 39), "octomus/unrelated-"+model.ID())
	}
	counts, err := f.state.MergeCounts("fixture/project")
	if err != nil || counts["waiting"] != 13 {
		t.Fatalf("seeded candidates = %+v, %v", counts, err)
	}
	forceMergeCheck(app)
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	counts, err = f.state.MergeCounts("fixture/project")
	must0(t, err)
	if counts["manual"] != 7 || counts["waiting"] != 6 {
		t.Fatalf("first pass processed %+v; want 7 manual and 6 waiting", counts)
	}
	forceMergeCheck(app)
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	counts, err = f.state.MergeCounts("fixture/project")
	must0(t, err)
	if counts["waiting"] != 1 || counts["manual"] != 12 {
		t.Fatalf("second pass left %+v; want the remaining 5 seeded records settled", counts)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a candidate with a missing task reached the mutation")
	}
}

func TestMergePausedReadSettlesIntentOnce(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	merge := observation.AutoMerge
	intent := merge.Clone()
	intent.Status = model.AutoMergeMerging
	intent.AttemptID = new("attempt-paused")
	intent.AttemptedAt = new(model.Now())
	claimed, err := f.state.ClaimMerge(observation.Repository, observation.PR.Number, intent, false)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	applied, err := f.state.SettleMerge(observation.Repository, observation.PR.Number, intent,
		model.AutoMergeUncertain, "The merge request outcome is unconfirmed", "", nil, false)
	if err != nil || !applied {
		t.Fatalf("uncertain settle = %v, %v", applied, err)
	}
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	forceMergeCheck(app)
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	if reads := statusReads(t, f); reads != 1 {
		t.Fatalf("paused reconciliation used %d reads; want exactly 1", reads)
	}
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeWaiting || settled.AttemptID != nil {
		t.Fatalf("paused reconciliation = %+v", settled)
	}
	app.checkMerges(f.cfg, control)
	app.wg.Wait()
	if reads := statusReads(t, f); reads != 1 {
		t.Fatalf("an immediate paused tick re-read the remote: %d reads", reads)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a paused pass reached the mutation")
	}
}

func statusReads(t *testing.T, f *fixture) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "github-status.jsonl"))
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["merge"] == true {
			n++
		}
	}
	return n
}

func forceMergeCheck(app *App) {
	app.runtimeMu.Lock()
	app.runtime.lastMergeCheck = time.Time{}
	app.runtimeMu.Unlock()
}

func waitingCandidate(t *testing.T, s *store.Store, number uint64, taskID, head, branch string) {
	t.Helper()
	merge := &model.AutoMergeState{
		TaskID: taskID, Head: head, ComparisonBase: "base", HeadBranch: branch,
		BaseBranch: "main", PolicyRevision: "policy", Authorized: true,
		Status: model.AutoMergeWaiting, Reason: "Waiting", ObservedAt: model.Now(),
	}
	observation := model.PRObservation{
		Repository: "fixture/project", ObservedAt: model.Now(), DeliveredHead: &head,
		PR: model.PullRequest{Number: number, Title: "Candidate", Branch: branch, Head: head,
			Base: "main", URL: "u", State: "open", Owned: true,
			HeadRepository: "fixture/project", BaseRepository: "fixture/project"},
		AutoMerge: merge,
	}
	must0(t, s.Put("pr", fmt.Sprintf("fixture/project:%d", number), observation))
}

func must0(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryModeSwitchGuardedByUnresolvedTasks(t *testing.T) {
	f := maintenanceFixture(t)
	app := f.pausedApp(t)
	settings, err := app.Settings()
	if err != nil {
		t.Fatal(err)
	}
	patch := func(mode string) error {
		_, err := app.SaveConfig(settings.Revision, map[string]json.RawMessage{
			"delivery_mode": json.RawMessage(`"` + mode + `"`),
		})
		if err == nil {
			settings, err = app.Settings()
		}
		return err
	}
	if err := patch("standard"); err != nil {
		t.Fatalf("an unblocked mode switch failed: %v", err)
	}
	if err := patch("maintenance"); err != nil {
		t.Fatalf("switching back to maintenance failed: %v", err)
	}
	queued := executionTask(t, f, f.cfg.DefaultBranch)
	putTask(t, f, queued)
	if err := patch("standard"); err == nil {
		t.Fatal("a mode switch slipped past a queued unarchived task")
	}
	failed := executionTask(t, f, f.cfg.DefaultBranch)
	failed.Status = model.StatusFailed
	putTask(t, f, failed)
	if err := patch("standard"); err == nil {
		t.Fatal("a mode switch slipped past a failed unarchived task")
	}
	if err := app.TaskAction(queued.ID, "cancel"); err != nil {
		t.Fatal(err)
	}
	if err := patch("standard"); err == nil {
		t.Fatal("a mode switch slipped past the still-failed task")
	}
	failed.Lifecycle.ArchivedAt = new(model.Now())
	must0(t, f.state.Put("task", failed.ID, failed))
	if err := patch("standard"); err != nil {
		t.Fatalf("archived/cancelled tasks still blocked the switch: %v", err)
	}
}

func TestMergeSquashIncludesIndependentBaseChange(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	work := filepath.Join(f.root, "independent-work")
	git(t, f.root, "clone", f.repo, work)
	git(t, work, "config", "user.name", "Fixture")
	git(t, work, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(work, "base-only.txt"), []byte("base moved independently\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "independent base change")
	git(t, work, "push", filepath.Join(f.root, "remote.git"), "HEAD:main")

	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerged || recorded.MergeCommit == nil {
		t.Fatalf("the independent base change blocked the merge: %+v", recorded)
	}
	remote := filepath.Join(f.root, "remote.git")
	if content := git(t, f.root, "--git-dir", remote, "show", "main:base-only.txt"); content != "base moved independently" {
		t.Fatalf("the squash merge dropped the independent base change: %q", content)
	}
	if content := git(t, f.root, "--git-dir", remote, "show", "main:feature.txt"); content != "fixed" {
		t.Fatalf("the squash merge dropped the maintenance change: %q", content)
	}
}

func TestMergeReconcileClosedExactAndMovedBase(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)

	patchPRs(t, f, map[string]any{"state": "closed"})
	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeClosed || recorded.ResultSource == nil ||
		*recorded.ResultSource != store.MergeResultObserved {
		t.Fatalf("closed exact-head outcome = %+v", recorded)
	}
	if prObservation(t, f, *saved.PRNumber).PR.State != "closed" {
		t.Fatal("PR state did not record the closed outcome")
	}

	f2 := maintenanceFixture(t)
	saved2, observation2 := publishedMaintenanceTask(t, f2)
	patchPRs(t, f2, map[string]any{"base": map[string]any{"ref": "release", "repo": map[string]any{"full_name": "fixture/project"}}})
	app2 := f2.pausedApp(t)
	app2.mergeCandidate(t.Context(), f2.cfg, false, false, true, nil, observation2)
	recorded = prObservation(t, f2, *saved2.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeManual || recorded.Authorized {
		t.Fatalf("a retargeted base kept authority: %+v", recorded)
	}
	if len(mergeAttempts(t, f2)) != 0 {
		t.Fatal("a moved base reached the mutation")
	}
}

func TestMergeInFlightReadsOriginalRepository(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	merge := *observation.AutoMerge
	intent := merge.Clone()
	intent.Status = model.AutoMergeUncertain
	intent.AttemptID = new("attempt-old")
	intent.AttemptedAt = new(model.Now())
	claimed, err := f.state.ClaimMerge(observation.Repository, observation.PR.Number,
		func() model.AutoMergeState { m := intent; m.Status = model.AutoMergeMerging; return m }(), false)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	applied, err := f.state.SettleMerge(observation.Repository, observation.PR.Number,
		func() model.AutoMergeState { m := intent; m.Status = model.AutoMergeMerging; return m }(),
		model.AutoMergeUncertain, "unconfirmed", "", nil, false)
	if err != nil || !applied {
		t.Fatalf("uncertain = %v, %v", applied, err)
	}
	moved := f.cfg.Clone()
	moved.GitHubRepo = "other/repo"
	before := statusReads(t, f)
	app.mergeCandidate(t.Context(), moved, false, true, false, nil, prObservation(t, f, *saved.PRNumber))
	if reads := statusReads(t, f); reads != before+1 {
		t.Fatalf("the in-flight intent used %d reads against the original repository", reads-before)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a foreign-repository configuration reached the mutation")
	}
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeWaiting || settled.AttemptID != nil {
		t.Fatalf("the original-repository read did not resolve the intent: %+v", settled)
	}
}

func TestMergeBlockedBranchCheckFailureDoesNotMutate(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	original := unfinishedBranchWork
	unfinishedBranchWork = func(_ *store.Store, _, _, _ string) (bool, error) {
		return false, fmt.Errorf("injected branch check failure")
	}
	defer func() { unfinishedBranchWork = original }()
	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeWaiting || recorded.Reason == "" {
		t.Fatalf("a failed precheck settled or stayed silent: %+v", recorded)
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a failed precheck reached the mutation")
	}
}

func TestMergeSettlementFailureBlocksUntilReconciled(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	original := settleMergeRecord
	settleMergeRecord = func(s *store.Store, repo string, number uint64, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) (bool, error) {
		if status == model.AutoMergeMerged && repo == observation.Repository && number == observation.PR.Number {
			return false, fmt.Errorf("injected settlement failure")
		}
		return s.SettleMerge(repo, number, expected, status, reason, source, commit, revoke)
	}
	defer func() { settleMergeRecord = original }()

	app.mergeAttempt(t.Context(), saved, observation, false)
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("the write did not happen: %+v", mergeAttempts(t, f))
	}
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerging || recorded.AttemptID == nil {
		t.Fatalf("the failed settlement did not keep the durable intent: %+v", recorded)
	}
	if a := app.recoveryConflict(); a == nil {
		t.Fatal("a failed settlement did not raise the recovery barrier")
	}
	app.mergeAttempt(t.Context(), saved, prObservation(t, f, *saved.PRNumber), false)
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatal("a claim proceeded while the merge barrier held")
	}
	if err := app.tick(); err != nil {
		t.Fatalf("tick during merge barrier = %v", err)
	}
	app.wg.Wait()
	app.runtimeMu.Lock()
	if len(app.runtime.mergeRecoveryErrors) != 1 {
		t.Fatalf("an unrelated tick cleared the merge barrier: %+v", app.runtime.mergeRecoveryErrors)
	}
	app.runtimeMu.Unlock()
	blocked := queuedTask(f.cfg, "blocked-dispatch", "main", "octomus/blocked-dispatch")
	putTask(t, f, blocked)
	if err := app.tick(); err != nil {
		t.Fatalf("tick during merge barrier = %v", err)
	}
	app.wg.Wait()
	if queued, _ := store.Get[model.Task](f.state, "task", blocked.ID); queued == nil || queued.Status != model.StatusQueued {
		t.Fatalf("dispatch proceeded while the merge barrier held: %+v", queued)
	}
	must0(t, f.state.Put("task", blocked.ID, func() model.Task { dead := blocked; dead.Status = model.StatusFailed; return dead }()))

	settleMergeRecord = original
	app.mergeCandidate(t.Context(), f.cfg, false, true, false, nil, prObservation(t, f, *saved.PRNumber))
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeMerged || settled.ResultSource == nil ||
		*settled.ResultSource != store.MergeResultObserved {
		t.Fatalf("reconciliation after the fault = %+v", settled)
	}
	app.runtimeMu.Lock()
	if len(app.runtime.mergeRecoveryErrors) != 0 {
		t.Fatalf("the barrier survived a successful settlement: %+v", app.runtime.mergeRecoveryErrors)
	}
	app.runtimeMu.Unlock()
	if app.recoveryConflict() != nil {
		t.Fatal("the same process stayed blocked after a successful settlement")
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("a second PUT happened: %+v", mergeAttempts(t, f))
	}
}

func TestAccumulatedPullRequestWithFeatureStaysManual(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	head := existingPRBranch(t, f)
	task := executionTask(t, f, "octomus/existing")
	task.Branch = "octomus/existing"
	task.SourceRevision = head
	number := uint64(42)
	url := "https://github.com/fixture/project/pull/42"
	task.PRNumber = &number
	task.PRURL = &url
	task.Proposal.Category = "documentation"
	task.Proposal.Prompt = "Restore the documented fixture output without expanding behavior. fixture-file=feature.txt"
	putTask(t, f, task)

	script := f.script
	script.Queue(f.routes.Executor, runnertest.Reply{Answer: "Restored the documented output", Effect: writeFile("feature.txt", "fixed\n")})
	script.Answer(f.routes.Reviewer, maintenanceReview(false, false), maintenanceReview(false, false), maintenanceReview(false, false), maintenanceReview(false, false), maintenanceReview(false, false))
	script.Queue(f.routes.Repair, runnertest.Reply{Answer: "Reduced the change"}, runnertest.Reply{Answer: "Reduced the change"}, runnertest.Reply{Answer: "Reduced the change"}, runnertest.Reply{Answer: "Reduced the change"})

	app := f.newApp(t)
	saved := driveTask(t, f, app, task.ID)
	if saved.Status == model.StatusPublished {
		t.Fatalf("a non-maintenance accumulated PR published: %+v", saved)
	}
	reviewers := script.Turns(f.routes.Reviewer)
	if len(reviewers) == 0 || !strings.Contains(reviewers[0].Prompt, "earlier.txt") {
		t.Fatalf("the trusted review diff did not include the accumulated feature file: %+v", reviewers)
	}
	prs, err := store.List[model.PRObservation](f.state, "pr")
	if err != nil {
		t.Fatal(err)
	}
	for _, observation := range prs {
		if observation.AutoMerge != nil {
			t.Fatalf("a rejected maintenance delivery recorded merge evidence: %+v", observation.AutoMerge)
		}
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a rejected maintenance delivery reached the mutation")
	}
}

func TestMergeRecoveryBarriersRemainIndependent(t *testing.T) {
	app := New(testStore(t), t.TempDir())
	defer app.Shutdown()

	app.setRecoveryError(errors.New("ordinary recovery write failed"))
	app.setMergeRecoveryError("fixture/project:1", errors.New("merge settlement failed"))
	app.setMergeRecoveryError("fixture/project:2", errors.New("merge settlement failed"))
	app.clearMergeRecoveryError("fixture/project:1")
	app.clearMergeRecoveryError("fixture/project:2")
	if err := app.recoveryConflict(); err == nil {
		t.Fatal("successful merge settlement cleared the generic recovery barrier")
	}
	app.runtimeMu.Lock()
	app.runtime.activeRecoveryError = nil
	app.runtimeMu.Unlock()
	if err := app.recoveryConflict(); err != nil {
		t.Fatalf("the recovered barriers still block: %v", err)
	}

	app.setMergeRecoveryError("fixture/project:3", errors.New("merge settlement failed"))
	app.setMergeRecoveryError("fixture/project:4", errors.New("merge settlement failed"))
	app.clearMergeRecoveryError("fixture/project:3")
	if err := app.recoveryConflict(); err == nil {
		t.Fatal("clearing one merge key dropped the remaining barrier")
	}
	app.clearMergeRecoveryError("fixture/project:4")
	if err := app.recoveryConflict(); err != nil {
		t.Fatalf("the last merge key did not release the barrier: %v", err)
	}
}

func TestMergeRecoveryErrorShownInStateView(t *testing.T) {
	app := New(testStore(t), t.TempDir())
	defer app.Shutdown()

	app.setMergeRecoveryError("fixture/project:7", errors.New("merge settlement failed"))
	view, err := app.StateView()
	if err != nil {
		t.Fatal(err)
	}
	if view["status"] != "unhealthy" {
		t.Fatalf("merge-only barrier status = %v", view["status"])
	}
	if view["recovery_error"] == nil || view["recovery_error"] == "" {
		t.Fatalf("the view hid the merge barrier: %+v", view["recovery_error"])
	}
	app.clearMergeRecoveryError("fixture/project:7")
	view, err = app.StateView()
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(view["recovery_error"]); got != "<nil>" && got != "" {
		t.Fatalf("a settled key still reports: %v", got)
	}
	if view["status"] == "unhealthy" {
		t.Fatal("a settled key still reports unhealthy")
	}
}

func TestMergeWaitsForInFlightBranchOwner(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	sibling := executionTask(t, f, f.cfg.DefaultBranch)
	sibling.Branch = saved.Branch
	sibling.Status = model.StatusCancelled
	putTask(t, f, sibling)
	app.runtimeMu.Lock()
	app.runtime.tasks[sibling.ID] = taskJob{branch: saved.Branch, cancel: func() {}}
	app.runtimeMu.Unlock()
	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("an in-flight cancelled sibling's branch was merged under it")
	}
	if recorded := prObservation(t, f, *saved.PRNumber).AutoMerge; recorded.Status != model.AutoMergeWaiting {
		t.Fatalf("the waiting record settled while a sibling still owns the branch: %+v", recorded)
	}
	app.runtimeMu.Lock()
	delete(app.runtime.tasks, sibling.ID)
	app.runtimeMu.Unlock()
	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, prObservation(t, f, *saved.PRNumber))
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerged {
		t.Fatalf("the joined sibling still blocks the branch: %+v", recorded)
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %+v; want exactly one merge", mergeAttempts(t, f))
	}
}

func TestMergeFinalGateRejectsLateBranchOwner(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, f.state.SaveControl(control))

	original := unfinishedBranchWork
	unfinishedBranchWork = func(s *store.Store, repository, branch, taskID string) (bool, error) {
		app.runtimeMu.Lock()
		app.runtime.tasks["late-sibling"] = taskJob{branch: saved.Branch, cancel: func() {}}
		app.runtimeMu.Unlock()
		return s.UnfinishedBranchWork(repository, branch, taskID)
	}
	defer func() { unfinishedBranchWork = original }()
	app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a branch owner arriving after preflight reached the mutation")
	}
	if recorded := prObservation(t, f, *saved.PRNumber).AutoMerge; recorded.Status != model.AutoMergeWaiting {
		t.Fatalf("the late-owner refusal mutated the record: %+v", recorded)
	}
	app.runtimeMu.Lock()
	delete(app.runtime.tasks, "late-sibling")
	app.runtimeMu.Unlock()
}

// failWaitingSettlement injects one waiting-state settlement failure and returns
// the restore, so a later pass can exercise the real store function.
func failWaitingSettlement(t *testing.T) func() {
	t.Helper()
	original := settleMergeRecord
	settleMergeRecord = func(s *store.Store, repo string, number uint64, expected model.AutoMergeState, status model.AutoMergeStatus, reason, source string, commit *string, revoke bool) (bool, error) {
		if status == model.AutoMergeWaiting {
			return false, fmt.Errorf("injected waiting settlement failure")
		}
		return s.SettleMerge(repo, number, expected, status, reason, source, commit, revoke)
	}
	return func() { settleMergeRecord = original }
}

func setContinuous(t *testing.T, app *App) {
	t.Helper()
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeContinuous)
	must0(t, app.Store.SaveControl(control))
}

// runMergePass runs one recovery and candidate pass, forcing the pacing gate.
func runMergePass(t *testing.T, app *App) {
	t.Helper()
	forceMergeCheck(app)
	if err := app.tick(); err != nil {
		t.Fatalf("merge tick: %v", err)
	}
	app.wg.Wait()
}

func TestMergePrecheckFailureRecoversToMerge(t *testing.T) {
	f := maintenanceFixture(t)
	saved, _ := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)
	patchPRs(t, f, map[string]any{"check_status": "EXPECTED"})

	restore := failWaitingSettlement(t)
	defer restore()
	runMergePass(t, app)
	restore()
	if app.recoveryConflict() == nil {
		t.Fatal("a failed precheck settlement did not raise a recovery barrier")
	}
	if merge := prObservation(t, f, *saved.PRNumber).AutoMerge; merge.Status != model.AutoMergeWaiting {
		t.Fatalf("pending-check evidence = %+v", merge)
	}

	patchPRs(t, f, map[string]any{"check_status": "SUCCESS"})
	runMergePass(t, app)
	merged := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merged.Status != model.AutoMergeMerged || merged.MergeCommit == nil {
		t.Fatalf("the recovered precheck did not permit the merge: %+v", merged)
	}
	if app.recoveryConflict() != nil {
		t.Fatal("the barrier survived the recovered precheck")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 1 {
		t.Fatalf("attempts = %+v; want exactly one merge", attempts)
	}
}

func TestMergePrecheckFailureArchivedLeavesNoBarrier(t *testing.T) {
	f := maintenanceFixture(t)
	saved, _ := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)
	patchPRs(t, f, map[string]any{"check_status": "EXPECTED"})

	restore := failWaitingSettlement(t)
	defer restore()
	runMergePass(t, app)
	restore()
	if app.recoveryConflict() == nil {
		t.Fatal("a failed precheck settlement did not raise a recovery barrier")
	}

	must0(t, app.TaskAction(saved.ID, "archive"))
	runMergePass(t, app)
	if app.recoveryConflict() != nil {
		t.Fatal("a resolved archival left a stale barrier")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 0 {
		t.Fatalf("an archived delivery still merged: %+v", attempts)
	}
	merge := prObservation(t, f, *saved.PRNumber).AutoMerge
	if merge.Status != model.AutoMergeManual || merge.Authorized {
		t.Fatalf("archived merge evidence = %+v", merge)
	}
	candidates, err := f.state.MergeCandidates(0, 10)
	must0(t, err)
	if len(candidates) != 0 {
		t.Fatalf("the revoked delivery is still a merge candidate: %+v", candidates)
	}
}

func TestMergePrecheckFailureTerminalObservationResolves(t *testing.T) {
	f := maintenanceFixture(t)
	saved, _ := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)
	patchPRs(t, f, map[string]any{"check_status": "EXPECTED"})

	restore := failWaitingSettlement(t)
	defer restore()
	runMergePass(t, app)
	restore()
	if app.recoveryConflict() == nil {
		t.Fatal("a failed precheck settlement did not raise a recovery barrier")
	}

	patchPRs(t, f, map[string]any{"state": "merged", "merged_at": "2026-10-01T00:00:00Z", "merge_commit_sha": "c" + strings.Repeat("0", 39)})
	runMergePass(t, app)
	settled := prObservation(t, f, *saved.PRNumber).AutoMerge
	if settled.Status != model.AutoMergeMerged || settled.ResultSource == nil ||
		*settled.ResultSource != store.MergeResultObserved {
		t.Fatalf("terminal observation = %+v", settled)
	}
	if app.recoveryConflict() != nil {
		t.Fatal("a terminal observation left the precheck barrier")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 0 {
		t.Fatalf("a terminal delivery issued a merge request: %+v", attempts)
	}
}

func TestMergePrecheckRecoveryPreservesUnresolvedBarriers(t *testing.T) {
	f := maintenanceFixture(t)
	_, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)

	pre := mergePrecheck{
		message:    "injected precheck failure",
		repository: observation.Repository,
		number:     observation.PR.Number,
		expected:   *observation.AutoMerge,
		status:     model.AutoMergeWaiting,
		reason:     "Waiting for checks to complete on the reviewed head",
	}
	app.setMergePrecheckError(mergeBarrierKey(pre.repository, pre.number), pre)
	app.setMergeRecoveryError(mergeBarrierKey(observation.Repository, 9999), errors.New("unresolved mutation outcome"))
	if app.recoveryConflict() == nil {
		t.Fatal("the seeded barriers did not block")
	}

	if err := app.tick(); err != nil {
		t.Fatalf("recovery tick: %v", err)
	}
	app.wg.Wait()

	app.runtimeMu.Lock()
	prechecks := len(app.runtime.mergePrecheckErrors)
	mutations := len(app.runtime.mergeRecoveryErrors)
	app.runtimeMu.Unlock()
	if prechecks != 0 {
		t.Fatalf("the recovered precheck survived: %d", prechecks)
	}
	if mutations != 1 {
		t.Fatalf("recovery dropped an unrelated mutation barrier: %d", mutations)
	}
	if app.recoveryConflict() == nil {
		t.Fatal("the unresolved mutation barrier no longer blocks")
	}
	if attempts := mergeAttempts(t, f); len(attempts) != 0 {
		t.Fatalf("a mutation barrier did not block the merge: %+v", attempts)
	}
}

func TestMergeRunOnceCancelledSiblingReleases(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	if err != nil {
		t.Fatal(err)
	}
	control.SetMode(model.OperatingModeRunOnce)
	control.Batch = &model.RunBatch{ID: "run-once-merge"}
	must0(t, f.state.SaveControl(control))
	runTask := saved.Clone()
	runTask.RunID = new(control.Batch.ID)
	must0(t, f.state.Put("task", saved.ID, runTask))

	sibling := executionTask(t, f, f.cfg.DefaultBranch)
	sibling.Branch = saved.Branch
	sibling.Status = model.StatusCancelled
	putTask(t, f, sibling)
	batch := new(control.Batch.ID)
	app.mergeCandidate(t.Context(), f.cfg, false, false, false, batch, observation)
	recorded := prObservation(t, f, *saved.PRNumber).AutoMerge
	if recorded.Status != model.AutoMergeMerged {
		t.Fatalf("a joined cancelled sibling blocked the run-once merge: %+v", recorded)
	}
	if len(mergeAttempts(t, f)) != 1 {
		t.Fatalf("attempts = %+v; want exactly one merge", mergeAttempts(t, f))
	}
}
