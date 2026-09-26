package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/report"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func admission(at string) store.Admission {
	a := store.NewAdmission("cycle", str("task"), "repair", config.NewRoute("fixture", "medium"))
	a.At = at
	return a
}

func saveConfig(t *testing.T, s *store.Store, edit func(*config.Config)) config.Config {
	t.Helper()
	c := config.Default()
	edit(&c)
	must(t, s.Put("settings", "config", c))
	return c
}

func TestDurableAndBudgetAtomic(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 1 })
	must(t, s.Put("x", "a", []int{1, 2}))
	must(t, s.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", config.NewRoute("fixture", "low"))))
	if err := s.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", config.NewRoute("fixture", "low"))); err == nil {
		t.Fatal("second reservation exceeded the daily budget")
	} else if !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("budget error is not classified: %v", err)
	}
	must(t, s.Close())
	s = open(t, path)
	value, err := store.Get[[]int](s, "x", "a")
	must(t, err)
	if value == nil || len(*value) != 2 || (*value)[0] != 1 || (*value)[1] != 2 {
		t.Fatalf("durable record differs: %v", value)
	}
	today, err := s.SessionsToday()
	must(t, err)
	if today != 1 {
		t.Fatalf("sessions today %d", today)
	}
}

func TestPlanningCapacityReflectsPolicyUsageAndUTCDay(t *testing.T) {
	s := open(t, statePath(t))
	for agents, required := range map[uint64]uint64{8: 12, 9: 13, 10: 14} {
		saveConfig(t, s, func(c *config.Config) { c.DiscoveryAgents = agents })
		capacity, err := s.PlanningCapacity()
		must(t, err)
		if capacity.Required != required || capacity.Status != model.PlanningCapacityStatusReady {
			t.Fatalf("agents %d: %+v", agents, capacity)
		}
		must(t, capacity.EnsureAvailable())
	}
	saveConfig(t, s, func(c *config.Config) { c.DiscoveryAgents = 9; c.MaxSessionsPerDay = 12 })
	capacity, err := s.PlanningCapacity()
	must(t, err)
	if capacity.Status != model.PlanningCapacityStatusLimitTooLow || !strings.Contains(capacity.Message(), "cannot fund") {
		t.Fatalf("%+v %q", capacity, capacity.Message())
	}
	if err := capacity.EnsureAvailable(); err == nil || !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("limit too low is not budget exhaustion: %v", err)
	}
	saveConfig(t, s, func(c *config.Config) { c.DiscoveryAgents = 9; c.MaxSessionsPerDay = 14 })
	for i, expected := range [][2]uint64{{1, 13}, {2, 12}} {
		must(t, s.ReserveSession(0, store.NewAdmission("cycle", nil, "discovery", config.NewRoute("fixture", "low"))))
		capacity, err := s.PlanningCapacity()
		must(t, err)
		if capacity.Used != expected[0] || capacity.Remaining != expected[1] {
			t.Fatalf("after %d reservations: %+v", i+1, capacity)
		}
		if i == 0 {
			if capacity.Status != model.PlanningCapacityStatusReady {
				t.Fatalf("%+v", capacity)
			}
			must(t, capacity.EnsureAvailable())
		} else {
			if capacity.Status != model.PlanningCapacityStatusDailyExhausted || !strings.Contains(capacity.Message(), "Wait until UTC midnight") {
				t.Fatalf("%+v %q", capacity, capacity.Message())
			}
			if capacity.EnsureAvailable() == nil {
				t.Fatal("exhausted capacity was available")
			}
		}
	}
	s2 := open(t, statePath(t))
	saveConfig(t, s2, func(c *config.Config) { c.DiscoveryAgents = 9; c.MaxSessionsPerDay = 14 })
	for range 2 {
		must(t, s2.ReserveSession(0, admission("2026-03-01T23:30:00Z")))
	}
	at := time.Date(2026, 3, 1, 23, 59, 0, 0, time.UTC)
	capacity, err = s2.PlanningCapacityAt(at)
	must(t, err)
	reset := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC).Unix()
	if capacity.Day != "2026-03-01" || capacity.Used != 2 || capacity.Remaining != 12 || capacity.Status != model.PlanningCapacityStatusDailyExhausted || capacity.NextResetAt != reset {
		t.Fatalf("%+v", capacity)
	}
	capacity, err = s2.PlanningCapacityAt(time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	must(t, err)
	if capacity.Day != "2026-03-02" || capacity.Used != 0 || capacity.Remaining != 14 || capacity.Status != model.PlanningCapacityStatusReady {
		t.Fatalf("%+v", capacity)
	}
}

func usageReport(t *testing.T, path string) map[string]any {
	t.Helper()
	value, err := report.UsageReport(path)
	must(t, err)
	return value
}

func daily(t *testing.T, value map[string]any) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, row := range value["daily"].([]any) {
		rows = append(rows, row.(map[string]any))
	}
	return rows
}

func number(value any) float64 {
	switch v := value.(type) {
	case json.Number:
		f, _ := v.Float64()
		return f
	case float64:
		return v
	}
	return -1
}

func TestAdmissionAndCounterCommitTogetherAcrossDaysAndRestarts(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 2 })
	first := admission("2026-09-09T23:59:59Z")
	must(t, s.ReserveSession(0, first))
	// A duplicate ledger id fails the admission insert after the counter moved,
	// and the whole reservation rolls back.
	if err := s.ReserveSession(0, first); err == nil {
		t.Fatal("duplicate admission id was accepted")
	}
	must(t, s.ReserveSession(0, admission("2026-09-09T23:59:59.500Z")))
	if err := s.ReserveSession(0, admission("2026-09-09T23:59:59.900Z")); err == nil || !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("third same-day admission: %v", err)
	}
	must(t, s.ReserveSession(0, admission("2026-09-10T00:00:00Z")))
	must(t, s.Close())
	s = open(t, path)
	// Offsets convert to the UTC day.
	must(t, s.ReserveSession(0, admission("2026-09-10T09:00:01+09:00")))
	must(t, s.Close())
	value := usageReport(t, path)
	if len(value["admissions"].([]any)) != 4 {
		t.Fatal(canonical(t, value["admissions"]))
	}
	rows := daily(t, value)
	if len(rows) != 2 {
		t.Fatal(canonical(t, rows))
	}
	for i, day := range []string{"2026-09-09", "2026-09-10"} {
		row := rows[i]
		if row["day"] != day || number(row["admissions"]) != 2 || number(row["attributed_admissions"]) != 2 || number(row["unattributed_admissions"]) != 0 {
			t.Fatal(canonical(t, row))
		}
	}
	if value["has_admission_ledger"] != true {
		t.Fatal(canonical(t, value))
	}
}

// reservations measure the live operator config and the durable counter, never a
// task's recorded config snapshot.
func TestLivePolicySurvivesRestartAndNeverUsesTaskSnapshot(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	queued := task()
	queued.Config.MaxSessionsPerDay = 150
	must(t, s.Put("task", queued.ID, queued))
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 2 })
	reserve := func(role string) error {
		return s.ReserveSession(0, store.NewAdmission("cycle", &queued.ID, role, queued.Route))
	}
	must(t, reserve("executor"))
	must(t, reserve("reviewer"))
	if err := reserve("repair"); err == nil || !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("third admission: %v", err)
	}
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 1 })
	must(t, s.Close())
	s = open(t, path)
	if err := reserve("repair"); err == nil {
		t.Fatal("restart forgot the recorded admissions")
	}
	if today, err := s.SessionsToday(); err != nil || today != 2 {
		t.Fatalf("sessions today = %d, %v", today, err)
	}
	saveConfig(t, s, func(c *config.Config) { c.MaxSessionsPerDay = 3 })
	must(t, reserve("repair"))
	saveConfig(t, s, func(c *config.Config) { c.MaxWorkspaceBytes = 42 })
	if err := s.ReserveSession(42, store.NewAdmission("cycle", nil, "reviewer", queued.Route)); err == nil || !errors.Is(err, model.BlockedReasonStorageLimit) {
		t.Fatalf("storage-limited admission: %v", err)
	}
	if today, err := s.SessionsToday(); err != nil || today != 3 {
		t.Fatalf("sessions today = %d, %v", today, err)
	}
	saved, err := store.Get[model.Task](s, "task", queued.ID)
	must(t, err)
	if saved.Config.MaxSessionsPerDay != 150 {
		t.Fatalf("task snapshot mutated: %+v", saved.Config)
	}
}

func TestReportNeverCreatesMissingState(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := report.UsageReport(filepath.Join(missing, "state.db")); err == nil || !strings.Contains(err.Error(), "state database") {
		t.Fatalf("missing state reported: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("report created the parent directory")
	}
}

func TestConcurrentPlanningSessionsAppendWithoutLosingEvidence(t *testing.T) {
	s := open(t, statePath(t))
	c := cycleFor(task())
	must(t, s.Put("cycle", c.ID, c))
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := model.NewSession(fmt.Sprintf("session-%d", i), "discovery", config.NewRoute("fixture", "low"))
			errs <- s.AppendCycleSession(c.ID, session)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	saved, err := store.Get[model.Cycle](s, "cycle", c.ID)
	must(t, err)
	if len(saved.Sessions) != 6 {
		t.Fatalf("%d sessions", len(saved.Sessions))
	}
	must(t, s.AppendCycleSession(c.ID, saved.Sessions[0]))
	saved, err = store.Get[model.Cycle](s, "cycle", c.ID)
	must(t, err)
	if len(saved.Sessions) != 6 {
		t.Fatalf("re-appending an existing session changed the count to %d", len(saved.Sessions))
	}
	if err := s.AppendCycleSession("missing", saved.Sessions[0]); err == nil || !strings.Contains(err.Error(), "Missing cycle missing") {
		t.Fatalf("%v", err)
	}
}

// Store half of the cancellation regressions: the SQL guard only cancels tasks
// that have not started publishing and have no output commit, and the cancel
// marker is an explicit record.
func TestCancellationGuardsPublicationCheckpoints(t *testing.T) {
	s := open(t, statePath(t))
	for _, c := range []struct {
		status model.Status
		output *string
		ok     bool
	}{
		{model.StatusQueued, nil, true},
		{model.StatusVerifying, nil, true},
		{model.StatusBlocked, nil, true},
		{model.StatusVerifying, str("out00001"), false},
		{model.StatusBlocked, str("out00001"), false},
		{model.StatusPublishing, nil, false},
		{model.StatusPublished, str("out00001"), false},
	} {
		tk := task()
		tk.Status = c.status
		tk.OutputCommit = c.output
		must(t, s.Put("task", tk.ID, tk))
		changed, err := s.CancelTask(tk.ID)
		must(t, err)
		if changed != c.ok {
			t.Fatalf("%s output=%v: cancelled %v", c.status, c.output, changed)
		}
		saved, err := store.Get[model.Task](s, "task", tk.ID)
		must(t, err)
		if c.ok {
			if saved.Status != model.StatusCancelled || saved.UpdatedAt == tk.UpdatedAt {
				t.Fatalf("%+v", saved)
			}
			if saved.Route != tk.Route || saved.Proposal.ID != tk.Proposal.ID {
				t.Fatal("cancellation rewrote the task beyond its status")
			}
		} else if saved.Status != c.status {
			t.Fatalf("%s was cancelled", c.status)
		}
	}
	changed, err := s.CancelTask("missing")
	must(t, err)
	if changed {
		t.Fatal("cancelled a missing task")
	}
	set, err := s.MarkerSet("cancel", "t")
	must(t, err)
	if set {
		t.Fatal("marker set before marking")
	}
	must(t, s.MarkCancel("t"))
	if set, _ := s.MarkerSet("cancel", "t"); !set {
		t.Fatal("marker missing after marking")
	}
	must(t, s.ClearCancel("t"))
	if set, _ := s.MarkerSet("cancel", "t"); set {
		t.Fatal("marker survived clearing")
	}
}

func observation(number uint64) model.PullRequest {
	return model.PullRequest{
		Number:    number,
		Title:     "Delivered",
		Branch:    "octomus/work",
		Head:      "delivered-head",
		Base:      "main",
		URL:       fmt.Sprintf("https://github.com/Fixture/Project/pull/%d", number),
		Body:      "",
		State:     "open",
		CreatedAt: "2026-01-01T00:00:00Z",
		Owned:     true,
	}
}

func TestRepositoryHistoryAndRediscoveryLineageIgnoreRepositoryCasing(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	published := reviewTask()
	published.Status = model.StatusPublished
	number := uint64(42)
	published.PRNumber = &number
	published.OutputCommit = str("delivered-head")
	must(t, s.Put("task", published.ID, published))
	must(t, s.Put("decision", "decision", map[string]any{"id": "decision", "repository": "Fixture/Project"}))
	must(t, s.Put("pr", "Fixture/Project:42", model.PrObservation{
		Repository:    "Fixture/Project",
		PR:            observation(42),
		ObservedAt:    model.Now(),
		DeliveredHead: str("delivered-head"),
	}))
	must(t, s.Close())
	s = open(t, path)
	for _, repo := range []string{"fixture/project", "Fixture/Project", "FIXTURE/PROJECT"} {
		byTitle := published.Proposal
		byTitle.ProblemKey = "different-key"
		byKey := published.Proposal
		byKey.Title = "Different wording"
		for _, proposed := range []model.Proposal{byTitle, byKey} {
			matches, err := s.DuplicateTasks(repo, []model.Proposal{proposed})
			must(t, err)
			if len(matches) != 1 || matches[0].ID != published.ID {
				t.Fatalf("%s: %d matches", repo, len(matches))
			}
		}
		otherTarget := published.Proposal
		otherTarget.Target = "Main"
		if matches, _ := s.DuplicateTasks(repo, []model.Proposal{otherTarget}); len(matches) != 0 {
			t.Fatalf("%s: target casing matched", repo)
		}
		memory, err := s.DecisionMemory(repo)
		must(t, err)
		if len(memory) != 1 || generic(t, memory[0])["id"] != "decision" {
			t.Fatalf("%s: %v", repo, memory)
		}
		output, err := s.LatestPrOutput(repo, 42)
		must(t, err)
		if output == nil || *output != "delivered-head" {
			t.Fatalf("%s: %v", repo, output)
		}
		id, saved, err := s.PrObservation(repo, 42)
		must(t, err)
		if id != "Fixture/Project:42" || saved == nil || saved.DeliveredHead == nil || *saved.DeliveredHead != "delivered-head" {
			t.Fatalf("%s: %s %+v", repo, id, saved)
		}
	}
	if matches, _ := s.DuplicateTasks("different/project", []model.Proposal{published.Proposal}); len(matches) != 0 {
		t.Fatal("different repository matched")
	}
	if memory, _ := s.DecisionMemory("different/project"); len(memory) != 0 {
		t.Fatal("different repository has memory")
	}
	if output, _ := s.LatestPrOutput("different/project", 42); output != nil {
		t.Fatal("different repository has output")
	}
	if _, saved, _ := s.PrObservation("different/project", 42); saved != nil {
		t.Fatal("different repository has an observation")
	}
	old := reviewTask()
	old.Status = model.StatusCancelled
	old.RediscoveryRequested = true
	must(t, s.Put("task", old.ID, old))
	requests, err := s.RediscoveryRequests("fixture/project")
	must(t, err)
	if len(requests) != 1 || generic(t, requests[0])["id"] != old.ID {
		t.Fatal(canonical(t, requests))
	}
	if requests, _ := s.RediscoveryRequests("different/project"); len(requests) != 0 {
		t.Fatal("different repository has requests")
	}
	replacement := reviewTask()
	replacement.CycleID = "replacement-cycle"
	replacement.Config.GitHubRepo = "fixture/project"
	replacement.Supersedes = []string{old.ID}
	replacement.Proposal.Reconsiders = []string{old.ID}
	plan := cycleFor(replacement)
	plan.ID = "replacement-cycle"
	must(t, s.CommitPlan(plan, []model.Task{replacement}))
	superseded, err := store.Get[model.Task](s, "task", old.ID)
	must(t, err)
	if len(superseded.SupersededBy) != 1 || superseded.SupersededBy[0] != replacement.ID || superseded.RediscoveryRequested {
		t.Fatalf("%+v", superseded)
	}
	if superseded.Config.GitHubRepo != "Fixture/Project" {
		t.Fatal("lineage update rewrote the saved repository casing")
	}
	if superseded.RediscoveryResult == nil || !strings.HasPrefix(*superseded.RediscoveryResult, "accepted: ") {
		t.Fatalf("%v", superseded.RediscoveryResult)
	}
	if requests, _ := s.RediscoveryRequests("FIXTURE/PROJECT"); len(requests) != 0 {
		t.Fatal("superseded request still listed")
	}
}

func TestDuplicateTitlesTrimSavedAndProposedWhitespace(t *testing.T) {
	{
		path := statePath(t)
		s := open(t, path)
		saved := reviewTask()
		saved.Status = model.StatusPublished
		saved.Proposal.Title = "  Concrete improvement  "
		must(t, s.Put("task", saved.ID, saved))
		before, _, err := s.GetRaw("task", saved.ID)
		must(t, err)
		must(t, s.Close())

		s = open(t, path)
		after, _, err := s.GetRaw("task", saved.ID)
		must(t, err)
		if string(before) != string(after) {
			t.Fatal("reopen rewrote the saved task")
		}
		proposed := saved.Proposal
		proposed.Title = "concrete IMPROVEMENT"
		proposed.ProblemKey = "different-key"
		matches, err := s.DuplicateTasks("fixture/project", []model.Proposal{proposed})
		must(t, err)
		if len(matches) != 1 || matches[0].ID != saved.ID {
			t.Fatalf("%d matches", len(matches))
		}
		for _, ws := range []string{"", " ", "\t\r\n", "\u0085   　"} {
			saved.Proposal.Title = ws + "Concrete improvement" + ws
			must(t, s.Put("task", saved.ID, saved))
			for _, title := range []string{"concrete IMPROVEMENT", "\tConcrete improvement "} {
				proposed.Title = title
				matches, err := s.DuplicateTasks("fixture/project", []model.Proposal{proposed})
				must(t, err)
				if len(matches) != 1 || matches[0].ID != saved.ID || !equalJSON(t, matches[0], saved) {
					t.Fatalf("ws=%q title=%q: %d matches", ws, title, len(matches))
				}
			}
		}
		for _, title := range []string{"Concrete  improvement", "​Concrete improvement"} {
			proposed.Title = title
			if matches, _ := s.DuplicateTasks("fixture/project", []model.Proposal{proposed}); len(matches) != 0 {
				t.Fatalf("%q matched", title)
			}
		}
		proposed.Title = "concrete IMPROVEMENT"
		archived := saved
		archived.Lifecycle.ArchivedAt = str(model.Now())
		must(t, s.Put("task", saved.ID, archived))
		if matches, _ := s.DuplicateTasks("fixture/project", []model.Proposal{proposed}); len(matches) != 0 {
			t.Fatal("archived task matched")
		}
		cancelled := saved
		cancelled.Status = model.StatusCancelled
		must(t, s.Put("task", saved.ID, cancelled))
		if matches, _ := s.DuplicateTasks("fixture/project", []model.Proposal{proposed}); len(matches) != 0 {
			t.Fatal("cancelled task matched")
		}
		must(t, s.Close())
	}
}

func TestDuplicateProblemIdentitiesPreserveUnicodeAndLegacyFallbacks(t *testing.T) {
	type identityCase struct {
		title, key, proposedTitle, proposedKey string
		duplicate                              bool
	}
	cases := []identityCase{
		{"Original", "\tÉCOLE ", "Reworded", "école", true},
		{"Original", "ΟΣ", "Reworded", "ος", true},
		{"Original", "İ", "Reworded", "i̇", true},
		{"\tÉCOLE ", "\t\u0085 　", "Reworded", "école", true},
		{" ÉCOLE ", "", "Reworded", "école", true},
		{"Original", "école", " ÉCOLE\t", "\n ", true},
		{" Concrete improvement ", "one", "concrete IMPROVEMENT", "two", true},
		{"ÉCOLE", "one", "école", "two", false},
		{"Original", "​key", "Reworded", "key", false},
		{"Original", "key  words", "Reworded", "key words", false},
	}
	{
		path := statePath(t)
		s := open(t, path)
		type expectation struct {
			proposed  model.Proposal
			duplicate bool
			id        string
		}
		var proposals []expectation
		for i, c := range cases {
			saved := reviewTask()
			saved.ID = fmt.Sprintf("saved-%d", i)
			saved.Proposal.Target = fmt.Sprintf("topic/%d", i)
			saved.Proposal.Title = c.title
			saved.Proposal.ProblemKey = c.key
			proposed := saved.Proposal
			proposed.Title = c.proposedTitle
			proposed.ProblemKey = c.proposedKey
			if saved.Proposal.SameWork(proposed) != c.duplicate {
				t.Fatalf("case %d: same_work", i)
			}
			must(t, s.Put("task", saved.ID, saved))
			proposals = append(proposals, expectation{proposed, c.duplicate, saved.ID})
		}
		evidence, err := s.ListRaw("task")
		must(t, err)
		must(t, s.Close())

		s = open(t, path)
		again, err := s.ListRaw("task")
		must(t, err)
		if fmt.Sprint(evidence) != fmt.Sprint(again) {
			t.Fatal("reopen changed the saved tasks")
		}
		for _, e := range proposals {
			matches, err := s.DuplicateTasks("fixture/project", []model.Proposal{e.proposed})
			must(t, err)
			expected := 0
			if e.duplicate {
				expected = 1
			}
			if len(matches) != expected {
				t.Fatalf("%s: %d matches", e.id, len(matches))
			}
			if e.duplicate && matches[0].ID != e.id {
				t.Fatalf("%s matched %s", e.id, matches[0].ID)
			}
			for _, decision := range []string{model.DecisionRejected, model.DecisionDeferred} {
				p := e.proposed
				p.Decision = decision
				if matches, _ := s.DuplicateTasks("fixture/project", []model.Proposal{p}); len(matches) != 0 {
					t.Fatalf("%s decision matched", decision)
				}
			}
		}
		must(t, s.Close())
	}
}

func TestDuplicateLookupLoadsOnlyMatchesWithoutTruncatingOrRepeatingThem(t *testing.T) {
	path := statePath(t)
	s := open(t, path)
	saved := reviewTask()
	db := raw(t, path)
	tx, err := db.Begin()
	must(t, err)
	for i := range 601 {
		row := saved
		row.ID = fmt.Sprintf("match-%d", i)
		row.Proposal.Title = fmt.Sprintf("Historical title %d", i)
		if _, err := tx.Exec("INSERT INTO records VALUES ('task',?1,?2)", row.ID, canonical(t, row)); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := generic(t, saved)
	unrelated["id"] = "unrelated"
	unrelated["proposal"].(map[string]any)["title"] = "Unrelated historical task"
	unrelated["proposal"].(map[string]any)["problem_key"] = "unrelated"
	// Identity filtering must not deserialize evidence for unrelated tasks.
	unrelated["verification"] = "unreadable historical evidence"
	if _, err := tx.Exec("INSERT INTO records VALUES ('task','unrelated',?1)", canonical(t, unrelated)); err != nil {
		t.Fatal(err)
	}
	must(t, tx.Commit())
	titleMatch := saved.Proposal
	titleMatch.Title = "Historical title 600"
	titleMatch.ProblemKey = "another-key"
	matches, err := s.DuplicateTasks("fixture/project", []model.Proposal{saved.Proposal, titleMatch, saved.Proposal})
	must(t, err)
	ids := map[string]bool{}
	for _, m := range matches {
		ids[m.ID] = true
	}
	if len(matches) != 601 || len(ids) != 601 || !ids["match-0"] || !ids["match-600"] {
		t.Fatalf("%d matches, %d distinct", len(matches), len(ids))
	}
}

func firstItem(t *testing.T, page store.Page) map[string]any {
	t.Helper()
	if len(page.Items) == 0 {
		t.Fatal("empty page")
	}
	return decodeMap(t, page.Items[0])
}

func TestProposalContentRevisionsCoverOmittedAndTruncatedEvidence(t *testing.T) {
	{
		path := statePath(t)
		s := open(t, path)
		c := cycleFor(reviewTask())
		c.Proposals[0].Reason = strings.Repeat("r", 2100)
		must(t, s.Put("cycle", c.ID, c))
		must(t, s.Close())

		s = open(t, path)
		original, err := store.Get[model.Cycle](s, "cycle", c.ID)
		must(t, err)
		if !equalJSON(t, original, c) {
			t.Fatal("saved cycle differs")
		}
		page, err := s.ProposalPage(store.HistoryQuery{})
		must(t, err)
		summary := firstItem(t, page)
		if number(summary["content_revision"]) != 1 || summary["prompt"] != "" || canonical(t, summary["evidence"]) != "[]" {
			t.Fatal(canonical(t, summary))
		}
		// Changes outside the proposal must not invalidate cached detail.
		c.Status = model.CycleCompleted
		must(t, s.Put("cycle", c.ID, c))
		page, err = s.ProposalPage(store.HistoryQuery{})
		must(t, err)
		if !equalJSON(t, firstItem(t, page), summary) {
			t.Fatal("cycle status change revised the proposal summary")
		}
		for revision := 2; revision <= 4; revision++ {
			switch revision {
			case 2:
				c.Proposals[0].Prompt = "Consolidated execution instructions"
			case 3:
				c.Proposals[0].Evidence = append(c.Proposals[0].Evidence, "Fresh file evidence")
			default:
				c.Proposals[0].Reason += "Updated rationale after the truncated prefix"
			}
			must(t, s.Put("cycle", c.ID, c))
			page, err = s.ProposalPage(store.HistoryQuery{})
			must(t, err)
			refreshed := firstItem(t, page)
			if number(refreshed["content_revision"]) != float64(revision) {
				t.Fatalf("revision %d: %v", revision, refreshed["content_revision"])
			}
			refreshed["content_revision"] = json.Number("1")
			if !equalJSON(t, refreshed, summary) {
				t.Fatalf("revision %d: summary changed beyond the revision", revision)
			}
			raw, err := s.ProposalDetail(c.ID, c.Proposals[0].ID)
			must(t, err)
			detail := decodeMap(t, raw)
			if number(detail["content_revision"]) != float64(revision) {
				t.Fatal(canonical(t, detail))
			}
			delete(detail, "content_revision")
			if !equalJSON(t, detail, c.Proposals[0]) {
				t.Fatalf("revision %d: detail differs", revision)
			}
		}
		must(t, s.Close())
		s = open(t, path)
		must(t, s.Put("cycle", c.ID, c))
		page, err = s.ProposalPage(store.HistoryQuery{})
		must(t, err)
		if number(firstItem(t, page)["content_revision"]) != 4 {
			t.Fatal("an unchanged rewrite bumped the revision")
		}
		saved, err := store.Get[model.Cycle](s, "cycle", c.ID)
		must(t, err)
		if !equalJSON(t, saved, c) {
			t.Fatal("saved cycle differs after reopen")
		}
		must(t, s.Close())
	}
}

func TestCycleSummariesCountCandidateDecisions(t *testing.T) {
	{
		path := statePath(t)
		s := open(t, path)
		c := cycleFor(reviewTask())
		c.Proposals[0].Decision = model.DecisionAccepted
		candidate := c.Proposals[0]
		candidate.ID = "rediscover-1"
		candidate.Decision = model.DecisionCandidate
		candidate.Reconsiders = []string{c.Proposals[0].ID}
		c.Proposals = append(c.Proposals, candidate)
		must(t, s.Put("cycle", c.ID, c))
		revisions := func(s *store.Store) []string {
			var out []string
			for _, p := range c.Proposals {
				raw, err := s.ProposalDetail(c.ID, p.ID)
				must(t, err)
				out = append(out, fmt.Sprint(decodeMap(t, raw)["content_revision"]))
			}
			return out
		}
		before := revisions(s)
		must(t, s.Close())
		db := raw(t, path)
		savedText := queryString(t, db, "SELECT data FROM records WHERE kind='cycle' AND id=?1", c.ID)

		s = open(t, path)
		if queryString(t, db, "SELECT data FROM records WHERE kind='cycle' AND id=?1", c.ID) != savedText {
			t.Fatal("reopen rewrote saved evidence")
		}
		if fmt.Sprint(revisions(s)) != fmt.Sprint(before) {
			t.Fatal("reopen changed cached proposal revisions")
		}
		page, err := s.HistoryPage("cycle", store.HistoryQuery{})
		must(t, err)
		summary := firstItem(t, page)
		if canonical(t, summary["decisions"]) != `{"accepted":1,"candidate":1,"deferred":0,"rejected":0}` {
			t.Fatalf("%s", canonical(t, summary["decisions"]))
		}
		// Later writes keep counting candidates.
		c.Status = model.CycleCompleted
		must(t, s.Put("cycle", c.ID, c))
		page, err = s.HistoryPage("cycle", store.HistoryQuery{})
		must(t, err)
		if number(firstItem(t, page)["decisions"].(map[string]any)["candidate"]) != 1 {
			t.Fatal("candidates not counted after write")
		}
		must(t, s.Close())
	}
}

func TestCommitPlanIsAtomicOnLineageFailure(t *testing.T) {
	s := open(t, statePath(t))
	// A control batch in the planning phase makes commit_plan write settings
	// mid-transaction, so a surviving "executing" phase would prove a partial commit.
	control := map[string]any{
		"paused": false, "mode": "run_once", "cycle_number": 1, "next_cycle_at": 0,
		"error": nil, "idle_streak": 0, "context_fingerprint": "",
		"batch": map[string]any{"id": "run-1", "phase": "planning", "cycle_id": "cycle-1"},
	}
	must(t, s.Put("settings", "control", control))
	// A reconsiders entry naming a task that was never saved fails the lineage
	// lookup after the cycle, task, control and decision writes already ran.
	queued := reviewTask()
	queued.CycleID = "cycle-1"
	plan := cycleFor(queued)
	plan.ID = "cycle-1"
	plan.RunID = str("run-1")
	plan.Proposals[0].Reconsiders = []string{"missing-task-id"}
	plan.DecisionMemory = []any{map[string]any{"id": "decision-1", "repository": "Fixture/Project"}}
	if err := s.CommitPlan(plan, []model.Task{queued}); err == nil {
		t.Fatal("plan with a missing rediscovery target committed")
	}
	assertEmpty := func(taskID string) {
		t.Helper()
		if c, _ := store.Get[model.Cycle](s, "cycle", "cycle-1"); c != nil {
			t.Fatal("cycle survived the failed commit")
		}
		if tk, _ := store.Get[model.Task](s, "task", taskID); tk != nil {
			t.Fatal("task survived the failed commit")
		}
		if d, _, _ := s.GetValue("decision", "decision-1"); d != nil {
			t.Fatal("decision survived the failed commit")
		}
		saved, _, err := s.GetValue("settings", "control")
		must(t, err)
		if !equalJSON(t, saved, control) {
			t.Fatal(canonical(t, saved))
		}
	}
	assertEmpty(queued.ID)
	// The supersedes lineage lookup fails the same way and must leave the same
	// empty store behind.
	superseding := reviewTask()
	superseding.CycleID = "cycle-1"
	superseding.Supersedes = []string{"missing"}
	plan = cycleFor(superseding)
	plan.ID = "cycle-1"
	plan.RunID = str("run-1")
	if err := s.CommitPlan(plan, []model.Task{superseding}); err == nil {
		t.Fatal("plan with a missing superseded task committed")
	}
	assertEmpty(superseding.ID)
	// A valid plan against the same control moves the batch to executing and
	// records the decision memory in the same transaction.
	valid := reviewTask()
	valid.CycleID = "cycle-1"
	plan = cycleFor(valid)
	plan.ID = "cycle-1"
	plan.RunID = str("run-1")
	plan.DecisionMemory = []any{map[string]any{"id": "decision-1", "repository": "Fixture/Project"}}
	must(t, s.CommitPlan(plan, []model.Task{valid}))
	saved, err := store.Get[model.Control](s, "settings", "control")
	must(t, err)
	if saved.Batch == nil || saved.Batch.Phase != model.BatchPhaseExecuting || saved.IdleStreak != 0 {
		t.Fatalf("%+v", saved)
	}
	if d, _, _ := s.GetValue("decision", "decision-1"); d == nil {
		t.Fatal("decision memory missing")
	}
	// An empty plan grows the idle streak.
	empty := cycleFor(valid)
	empty.ID = "cycle-2"
	must(t, s.CommitPlan(empty, nil))
	saved, err = store.Get[model.Control](s, "settings", "control")
	must(t, err)
	if saved.IdleStreak != 1 {
		t.Fatalf("%+v", saved)
	}
}

func TestOldAttentionSurvivesBoundedDashboardAndPages(t *testing.T) {
	s := open(t, statePath(t))
	old := task()
	old.Status = model.StatusBlocked
	must(t, s.Put("task", old.ID, old))
	published := task()
	published.Status = model.StatusPublished
	published.Proposal.Prompt = strings.Repeat("x", 64000)
	for i := range 1000 {
		published.ID = fmt.Sprintf("task-%d", i)
		must(t, s.Put("task", published.ID, published))
	}
	snapshot, err := s.Dashboard()
	must(t, err)
	if len(snapshot.Tasks) != 300 {
		t.Fatalf("%d tasks", len(snapshot.Tasks))
	}
	if len(canonical(t, snapshot)) >= 1024*1024 {
		t.Fatal("dashboard snapshot exceeds 1 MiB")
	}
	if snapshot.Counts["blocked"] != 1 || len(snapshot.AttentionTasks) != 1 || decodeMap(t, snapshot.AttentionTasks[0])["id"] != old.ID {
		t.Fatalf("%v", snapshot.Counts)
	}
	page, err := s.HistoryPage("task", store.HistoryQuery{Status: str("attention")})
	must(t, err)
	if len(page.Items) != 1 || decodeMap(t, page.Items[0])["id"] != old.ID {
		t.Fatalf("%d attention items", len(page.Items))
	}
	old.Status = model.StatusExecuting
	must(t, s.Put("task", old.ID, old))
	active, err := s.Dashboard()
	must(t, err)
	if decodeMap(t, active.Tasks[0])["id"] != old.ID || len(active.Tasks) != 300 {
		t.Fatal("active task is not first")
	}
	first, err := s.HistoryPage("task", store.HistoryQuery{})
	must(t, err)
	second, err := s.HistoryPage("task", store.HistoryQuery{Before: first.NextCursor})
	must(t, err)
	if len(first.Items) != 50 || first.NextCursor == nil || len(second.Items) != 50 {
		t.Fatalf("%d/%d items", len(first.Items), len(second.Items))
	}
	seen := map[string]bool{}
	for _, item := range first.Items {
		seen[decodeMap(t, item)["id"].(string)] = true
	}
	for _, item := range second.Items {
		if seen[decodeMap(t, item)["id"].(string)] {
			t.Fatal("pages overlap")
		}
	}
}

// planning validation consumes these duplicates.
func TestUnresolvedProblemIdentitySurvivesRewording(t *testing.T) {
	s := open(t, statePath(t))
	old := task()
	old.Status = model.StatusBlocked
	old.Proposal.ProblemKey = "stable-problem"
	must(t, s.Put("task", old.ID, old))
	proposed := old.Proposal
	proposed.ID = "fresh"
	proposed.Title = "Different wording for the same work"
	duplicates, err := s.DuplicateTasks(old.Config.GitHubRepo, []model.Proposal{proposed})
	must(t, err)
	if len(duplicates) != 1 || duplicates[0].ID != old.ID {
		t.Fatalf("%d duplicates", len(duplicates))
	}
}

func TestPublishedWorkRemainsInDuplicateLookups(t *testing.T) {
	s := open(t, statePath(t))
	delivered := task()
	delivered.Status = model.StatusPublished
	number := uint64(42)
	delivered.PRNumber = &number
	delivered.Proposal.ProblemKey = "stable-problem"
	must(t, s.Put("task", delivered.ID, delivered))
	for _, matchTitle := range []bool{true, false} {
		proposed := delivered.Proposal
		if matchTitle {
			proposed.ProblemKey = "different-key"
		} else {
			proposed.Title = "Different wording for delivered work"
		}
		duplicates, err := s.DuplicateTasks(delivered.Config.GitHubRepo, []model.Proposal{proposed})
		must(t, err)
		if len(duplicates) != 1 || duplicates[0].ID != delivered.ID {
			t.Fatalf("title lookup %v: %d", matchTitle, len(duplicates))
		}
		if d, _ := s.DuplicateTasks("another/project", []model.Proposal{proposed}); len(d) != 0 {
			t.Fatal("another repository matched")
		}
		proposed.Target = "another-branch"
		if d, _ := s.DuplicateTasks(delivered.Config.GitHubRepo, []model.Proposal{proposed}); len(d) != 0 {
			t.Fatal("another target matched")
		}
	}
}
