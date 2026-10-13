package engine

import (
	"encoding/json"
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
	prs, err := store.List(f.state, store.PRs)
	must0(t, err)
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
	return readJSONL(t, filepath.Join(f.root, "merge-attempts.jsonl"))
}

func patchPRs(t *testing.T, f *fixture, patch map[string]any) {
	t.Helper()
	path := filepath.Join(f.root, "prs.json")
	prs := prsJSON(t, f)
	for i := range prs {
		for key, value := range patch {
			prs[i][key] = value
		}
	}
	out, err := json.Marshal(prs)
	must0(t, err)
	must0(t, os.WriteFile(path, out, 0o644))
}

func writePRPatch(t *testing.T, f *fixture, patch map[string]any) {
	t.Helper()
	data, err := json.Marshal(patch)
	must0(t, err)
	must0(t, os.WriteFile(filepath.Join(f.root, "pr-create-patch.json"), data, 0o600))
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

func statusReads(t *testing.T, f *fixture) int {
	t.Helper()
	n := 0
	for _, entry := range readJSONL(t, filepath.Join(f.root, "github-status.jsonl")) {
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
	must0(t, store.Put(s, store.PRs.Key(fmt.Sprintf("fixture/project:%d", number)), observation))
}

func setContinuous(t *testing.T, app *App) {
	t.Helper()
	control, err := app.Control()
	must0(t, err)
	control.Paused = false
	control.SetMode(model.OperatingModeContinuous)
	must0(t, app.store.SaveControl(control))
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

	must0(t, app.TaskAction(saved.ID, model.TaskActionArchive))
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
	must0(t, app.tick())
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

func TestMergePacingAndFairPaging(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	writePRPatch(t, f, map[string]any{"check_status": "EXPECTED"})
	publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)
	control, err := app.Control()
	must0(t, err)

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

func TestDeliveryModeSwitchGuardedByUnresolvedTasks(t *testing.T) {
	f := maintenanceFixture(t)
	app := f.pausedApp(t)
	settings, err := app.Settings()
	must0(t, err)
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
	must0(t, app.TaskAction(queued.ID, model.TaskActionCancel))
	if err := patch("standard"); err == nil {
		t.Fatal("a mode switch slipped past the still-failed task")
	}
	failed.Lifecycle.ArchivedAt = new(model.Now())
	must0(t, store.Put(f.state, store.Tasks.Key(failed.ID), failed))
	if err := patch("standard"); err != nil {
		t.Fatalf("archived/cancelled tasks still blocked the switch: %v", err)
	}
}

func TestMergeSquashIncludesIndependentBaseChange(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	pushCommit(t, f, "independent-work", "", "base-only.txt", "base moved independently\n", "independent base change", "HEAD:main", false)

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
	prs, err := store.List(f.state, store.PRs)
	must0(t, err)
	for _, observation := range prs {
		if observation.AutoMerge != nil {
			t.Fatalf("a rejected maintenance delivery recorded merge evidence: %+v", observation.AutoMerge)
		}
	}
	if len(mergeAttempts(t, f)) != 0 {
		t.Fatal("a rejected maintenance delivery reached the mutation")
	}
}

func TestMergeWaitsForInFlightBranchOwner(t *testing.T) {
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	sibling := executionTask(t, f, f.cfg.DefaultBranch)
	sibling.Branch = saved.Branch
	sibling.Status = model.StatusCancelled
	putTask(t, f, sibling)
	app.runtimeMu.Lock()
	app.runtime.tasks[sibling.ID] = &taskJob{branch: saved.Branch, cancel: func() {}}
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
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	setContinuous(t, app)

	heldUploadPack(t, f)
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.mergeCandidate(t.Context(), f.cfg, false, false, true, nil, observation)
	}()
	waitForPreflights(t, f, 1)
	app.runtimeMu.Lock()
	app.runtime.tasks["late-sibling"] = &taskJob{branch: saved.Branch, cancel: func() {}}
	app.runtimeMu.Unlock()
	releasePreflight(t, f)
	<-done
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

func TestMergeRunOnceCancelledSiblingReleases(t *testing.T) {
	t.Parallel()
	f := maintenanceFixture(t)
	saved, observation := publishedMaintenanceTask(t, f)
	app := f.pausedApp(t)
	control, err := app.Control()
	must0(t, err)
	control.SetMode(model.OperatingModeRunOnce)
	control.Batch = &model.RunBatch{ID: "run-once-merge"}
	must0(t, f.state.SaveControl(control))
	runTask := saved.Clone()
	runTask.RunID = new(control.Batch.ID)
	must0(t, store.Put(f.state, store.Tasks.Key(saved.ID), runTask))

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
