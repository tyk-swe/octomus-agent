package store_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func insightCycle(id string, mode model.CycleMode, status model.CycleStatus, number uint64, started string, proposals ...model.Proposal) model.Cycle {
	return model.Cycle{
		ID: id, Mode: mode, Status: status, Number: number,
		StartedAt: started, Repository: "fixture/project",
		Proposals:      proposals,
		DecisionMemory: []model.DecisionRecord{},
	}
}

func insightProposal(id, category, decision string) model.Proposal {
	return model.Proposal{
		ID: id, Title: "Proposal " + id, Category: category, Decision: decision,
		Target: "main", Evidence: []string{}, Dependencies: []string{}, RelevantPaths: []string{}, Reconsiders: []string{},
	}
}

func insightTask(id, cycle, created string, status model.Status, category, tier string, pr *uint64) model.Task {
	cfg := config.Default()
	cfg.GitHubRepo = "fixture/project"
	return model.Task{
		ID: id, CycleID: cycle, Status: status, CreatedAt: created, UpdatedAt: created,
		Config: cfg, PRNumber: pr,
		Proposal: model.Proposal{
			ID: "prop-" + id, Title: "Task " + id, Category: category, Tier: tier,
			Target: "main", Evidence: []string{}, Dependencies: []string{}, RelevantPaths: []string{}, Reconsiders: []string{},
		},
		Sessions: []model.Session{}, Reviews: []model.ReviewRound{}, Verification: []model.Verification{}, SupersededBy: []string{}, Supersedes: []string{},
	}
}

func insightAdmission(at, taskID string) store.Admission {
	var task *string
	if taskID != "" {
		task = &taskID
	}
	a := store.NewAdmission("cycle", task, "discovery", config.NewRoute("fixture", "low"))
	a.At = at
	return a
}

func TestGoldenAdmissionsKeepRepositoryAttribution(t *testing.T) {
	t.Parallel()
	path := golden(t)
	s := open(t, path)
	cfg, err := store.Get(s, store.ConfigKey)
	must(t, err)
	var lastDay string
	must(t, raw(t, path).QueryRow("SELECT max(day) FROM admissions").Scan(&lastDay))
	now, err := time.Parse("2006-01-02", lastDay)
	must(t, err)
	scoped, err := s.ForRepository(cfg.GitHubRepo).Insights(now, 90)
	must(t, err)
	var planning, task, unattributed uint64
	for _, day := range scoped.Daily {
		planning += day.PlanningAdmissions
		task += day.TaskAdmissions
		unattributed += day.UnattributedAdmissions
	}
	if planning != 26 || task != 18 || unattributed != 0 {
		t.Fatalf("golden admissions lost attribution: planning=%d task=%d unattributed=%d", planning, task, unattributed)
	}
	other, err := s.ForRepository("acme/another").Insights(now, 90)
	must(t, err)
	for _, day := range other.Daily {
		if day.Admissions != 0 || day.PlanningAdmissions != 0 || day.TaskAdmissions != 0 {
			t.Fatal("legacy admissions leaked to a different repository")
		}
	}
}

func TestInsights(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	db := raw(t, path)
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 100 })
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	day := func(back int) string { return now.AddDate(0, 0, -back).Format("2006-01-02") }
	at := func(back int) string { return day(back) + "T10:00:00Z" }

	// Tasks: created inside the window unless noted.
	fortyTwo, seven, ninetyNine := uint64(42), uint64(7), uint64(99)
	tasks := []model.Task{
		insightTask("task-doc-1", "cycle-exec", at(1), model.StatusPublished, "documentation", "M", &fortyTwo),
		insightTask("task-doc-2", "cycle-exec", at(2), model.StatusPublished, "documentation", "M", &fortyTwo),
		insightTask("task-open", "cycle-exec", at(2), model.StatusPublished, "features", "S", &seven),
		insightTask("task-unseen", "cycle-exec", at(3), model.StatusPublished, "documentation", "M", &ninetyNine),
		insightTask("task-fail", "cycle-exec", at(3), model.StatusFailed, "features", "S", nil),
		insightTask("task-run", "cycle-exec", at(3), model.StatusExecuting, "features", "S", nil),
		insightTask("task-outside", "cycle-since", at(29), model.StatusQueued, "outside-category", "M", nil),
		insightTask("task-old", "cycle-old", at(30), model.StatusPublished, "features", "S", nil),
	}
	for _, task := range tasks {
		if task.ID == "task-doc-2" {
			task.Config.GitHubRepo = "FIXTURE/PROJECT"
		}
		must(t, store.Put(s, store.Tasks.Key(task.ID), task))
	}

	// Cycles: an audit twin and an out-of-window control for each grouping.
	must(t, store.Put(s, store.Cycles.Key("cycle-exec"), insightCycle("cycle-exec", model.CycleModeExecution, model.CycleCompleted, 1, at(1),
		insightProposal("p-doc", "documentation", model.DecisionAccepted),
		insightProposal("p-feat-no", "features", model.DecisionRejected),
		insightProposal("p-feat-maybe", "features", model.DecisionDeferred),
		insightProposal("p-request-no", "", model.DecisionRejected),
		insightProposal("p-request-maybe", "", model.DecisionDeferred),
		insightProposal("p-seeded", "features", model.DecisionCandidate))))
	must(t, store.Put(s, store.Cycles.Key("cycle-audit"), insightCycle("cycle-audit", model.CycleModeAudit, model.CycleCompleted, 2, at(1),
		insightProposal("a-doc", "documentation", model.DecisionAccepted),
		insightProposal("a-doc-two", "documentation", model.DecisionAccepted))))
	must(t, store.Put(s, store.Cycles.Key("cycle-failed"), insightCycle("cycle-failed", model.CycleModeExecution, model.CycleFailed, 3, at(3))))
	must(t, store.Put(s, store.Cycles.Key("cycle-since"), insightCycle("cycle-since", model.CycleModeExecution, model.CycleCompleted, 4, at(29),
		insightProposal("p-boundary", "features", model.DecisionAccepted))))
	must(t, store.Put(s, store.Cycles.Key("cycle-old"), insightCycle("cycle-old", model.CycleModeExecution, model.CycleCompleted, 0, at(30),
		insightProposal("p-before", "documentation", model.DecisionAccepted))))

	// PR observations: pr 42 observes open then merged (latest wins); pr 7 open; pr 99 never observed.
	observe := func(id string, number uint64, state, observedAt, repository string) {
		must(t, store.Put(s, store.PRs.Key(id), model.PRObservation{
			Repository: repository, ObservedAt: observedAt,
			PR: model.PullRequest{Number: number, State: state, Title: "PR"},
		}))
	}
	observe("pr-42-open", 42, "open", at(5), "fixture/project")
	observe("pr-42-merged", 42, "merged", at(2), "FIXTURE/PROJECT")
	observe("pr-7-open", 7, "open", at(2), "fixture/project")

	// Admissions: yesterday 4 planning + 2 task; three days ago 3 usage but only 2 rows.
	for range 4 {
		must(t, s.ReserveSession(0, insightAdmission(at(1), "")))
	}
	must(t, s.ReserveSession(0, insightAdmission(at(1), "task-doc-1")))
	must(t, s.ReserveSession(0, insightAdmission(at(1), "task-doc-1")))
	must(t, s.ReserveSession(0, insightAdmission(at(3), "")))
	must(t, s.ReserveSession(0, insightAdmission(at(3), "task-fail")))
	exec(t, db, "INSERT INTO usage(repository,day,sessions) VALUES ('',?1,1) ON CONFLICT(repository,day) DO UPDATE SET sessions=sessions+1", day(3))
	// Before the window: counted nowhere.
	exec(t, db, "INSERT INTO usage(repository,day,sessions) VALUES ('',?1,5)", day(30))
	exec(t, db, "INSERT INTO admissions(id,at,day,data) VALUES ('a-old',?1,?1,'{}')", at(30))

	insights, err := s.Insights(now, 30)
	must(t, err)
	if insights.Days != 30 || insights.Since != day(29) || insights.Until != day(0) {
		t.Fatalf("window: %+v", insights)
	}
	if len(insights.Daily) != 30 {
		t.Fatalf("daily rows: %d", len(insights.Daily))
	}
	yesterday := insights.Daily[29-1]
	if yesterday.Day != day(1) || yesterday.Admissions != 6 || yesterday.PlanningAdmissions != 4 || yesterday.TaskAdmissions != 2 || yesterday.UnattributedAdmissions != 0 || yesterday.CyclesStarted != 2 {
		t.Fatalf("yesterday: %+v", yesterday)
	}
	earlier := insights.Daily[29-3]
	if earlier.Admissions != 3 || earlier.PlanningAdmissions != 1 || earlier.TaskAdmissions != 1 || earlier.UnattributedAdmissions != 1 || earlier.CyclesStarted != 1 || earlier.TasksCreated != 3 {
		t.Fatalf("three days ago: %+v", earlier)
	}
	boundary := insights.Daily[0]
	if boundary.Day != day(29) || boundary.CyclesStarted != 1 || boundary.TasksCreated != 1 {
		t.Fatalf("window boundary: %+v", boundary)
	}
	if insights.Cycles["execution"] != 3 || insights.Cycles["audit"] != 1 || insights.Cycles["completed"] != 3 || insights.Cycles["failed"] != 1 || insights.Cycles["running"] != 0 || insights.Cycles["interrupted"] != 0 || insights.Cycles["idle"] != 0 {
		t.Fatalf("cycles: %+v", insights.Cycles)
	}
	if insights.Tasks["published"] != 4 || insights.Tasks["failed"] != 1 || insights.Tasks["executing"] != 1 || insights.Tasks["queued"] != 1 {
		t.Fatalf("tasks: %+v", insights.Tasks)
	}
	for _, name := range []string{"queued", "executing", "reviewing", "repairing", "verifying", "publishing", "published", "blocked", "failed", "cancelled"} {
		if _, ok := insights.Tasks[name]; !ok {
			t.Fatalf("missing status key %s in %+v", name, insights.Tasks)
		}
	}
	documentation := insights.Categories[8]
	features := insights.Categories[0]
	if documentation.Category != "documentation" || documentation.Accepted != 1 || documentation.Rejected != 0 || documentation.Deferred != 0 || documentation.Tasks != 3 || documentation.Published != 3 || documentation.InProgress != 0 || documentation.Unresolved != 0 || documentation.Cancelled != 0 {
		t.Fatalf("documentation: %+v", documentation)
	}
	if features.Category != "features" || features.Accepted != 1 || features.Rejected != 1 || features.Deferred != 1 || features.Tasks != 3 || features.Published != 1 || features.InProgress != 1 || features.Unresolved != 1 {
		t.Fatalf("features: %+v", features)
	}
	if len(insights.Categories) != len(config.Categories())+1 {
		t.Fatalf("category rows: %d", len(insights.Categories))
	}
	uncategorized := insights.Categories[len(insights.Categories)-1]
	if uncategorized.Category != "uncategorized" || uncategorized.Rejected != 1 || uncategorized.Deferred != 1 || uncategorized.Tasks != 1 || uncategorized.InProgress != 1 {
		t.Fatalf("uncategorized: %+v", uncategorized)
	}
	tierM := insights.Tiers[2]
	tierS := insights.Tiers[1]
	if tierM.Tier != "M" || tierM.Tasks != 4 || tierM.Published != 3 || tierM.Admissions != 2 {
		t.Fatalf("tier M: %+v", tierM)
	}
	if tierS.Tier != "S" || tierS.Tasks != 3 || tierS.Published != 1 || tierS.Admissions != 1 {
		t.Fatalf("tier S: %+v", tierS)
	}
	if insights.PRs["merged"] != 1 || insights.PRs["open"] != 1 || insights.PRs["closed"] != 0 || insights.PRs["unobserved"] != 1 {
		t.Fatalf("prs: %+v", insights.PRs)
	}
	if insights.Measurement == "" {
		t.Fatal("measurement caveat is empty")
	}
	if _, err := s.Insights(now, 14); err == nil || err.Error() != "Insights cover 7, 30 or 90 days" {
		t.Fatalf("invalid days: %v", err)
	}
}

func TestInsightsUTCWindowAcrossDST(t *testing.T) {
	t.Parallel()
	s := open(t, statePath(t))
	location, err := time.LoadLocation("America/New_York")
	must(t, err)
	now := time.Date(2026, 11, 2, 0, 30, 0, 0, time.UTC).In(location)
	must(t, store.Put(s, store.Cycles.Key("before"), insightCycle("before", model.CycleModeExecution, model.CycleCompleted, 1, "2026-10-26T23:59:59Z")))
	must(t, store.Put(s, store.Cycles.Key("inside"), insightCycle("inside", model.CycleModeExecution, model.CycleCompleted, 2, "2026-10-27T00:00:00Z")))
	insights, err := s.Insights(now, 7)
	must(t, err)
	if insights.Since != "2026-10-27" || insights.Until != "2026-11-02" || len(insights.Daily) != 7 || insights.Daily[0].Day != insights.Since {
		t.Fatalf("UTC window: %+v", insights)
	}
	if insights.Cycles["execution"] != 1 || insights.Daily[0].CyclesStarted != 1 {
		t.Fatalf("cycle totals disagree with the UTC window: %+v", insights)
	}
}

func TestInsightsTierAdmissionsUseCreatedTaskCohort(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 100 })
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, task := range []model.Task{
		insightTask("old-S", "cycle", "2026-09-01T10:00:00Z", model.StatusExecuting, "features", "S", nil),
		insightTask("old-M", "cycle", "2026-09-01T10:00:00Z", model.StatusExecuting, "features", "M", nil),
		insightTask("new-S", "cycle", "2026-10-12T10:00:00Z", model.StatusQueued, "features", "S", nil),
	} {
		must(t, store.Put(s, store.Tasks.Key(task.ID), task))
	}
	for _, taskID := range []string{"old-S", "old-S", "old-S", "old-S", "old-S", "old-M", "new-S"} {
		must(t, s.ReserveSession(0, insightAdmission("2026-10-13T10:00:00Z", taskID)))
	}
	must(t, s.ReserveSession(0, insightAdmission("2026-09-01T10:00:00Z", "old-S")))
	for _, days := range []int{7, 30} {
		insights, err := s.Insights(now, days)
		must(t, err)
		small, medium := insights.Tiers[1], insights.Tiers[2]
		if small.Tasks != 1 || small.Admissions != 6 || small.CreatedTaskAdmissions != 1 {
			t.Fatalf("%d-day S cohort: %+v", days, small)
		}
		if medium.Tasks != 0 || medium.Admissions != 1 || medium.CreatedTaskAdmissions != 0 {
			t.Fatalf("%d-day M cohort: %+v", days, medium)
		}
	}
}
