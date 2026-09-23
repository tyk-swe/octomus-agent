package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

func TestIdentityAndDestinationRules(t *testing.T) {
	for _, tc := range []struct{ title, key, want string }{
		{" ÉCOLE ", "", "école"}, {"Original", "ΟΣ", "ος"}, {"Original", "İ", "i̇"},
	} {
		if got := ProblemIdentity(tc.title, tc.key); got != tc.want {
			t.Fatalf("ProblemIdentity(%q, %q) = %q", tc.title, tc.key, got)
		}
	}
	normalized, id, err := NotificationDestination("https://example.com/hook")
	if err != nil || normalized == "" || len(id) != 64 {
		t.Fatalf("destination = %q, %q, %v", normalized, id, err)
	}
	again, againID, err := NotificationDestination("https://example.com/hook")
	if err != nil || again != normalized || againID != id {
		t.Fatal("destination identity changed")
	}
	for _, paths := range [][]string{{"../a"}, {"/a"}, {"./a"}, {""}, make([]string, 41)} {
		if _, err := DecisionMemoryFingerprint("revision", paths, ""); err == nil {
			t.Errorf("invalid paths %v", paths)
		}
	}
}
func TestEveryStatusMatchesItsSerializedName(t *testing.T) {
	for _, s := range []Status{StatusQueued, StatusExecuting, StatusReviewing, StatusRepairing, StatusVerifying, StatusPublishing, StatusPublished, StatusBlocked, StatusFailed, StatusCancelled} {
		data, err := json.Marshal(s)
		if err != nil || string(data) != fmt.Sprintf("%q", s.String()) {
			t.Fatal(s, err)
		}
		if s.Active() != (s >= StatusExecuting && s <= StatusPublishing) {
			t.Fatal(s)
		}
	}
}
func TestEmptyOrIncompleteReviewNeverClean(t *testing.T) {
	for _, r := range []Review{{Completed: true}, {Completed: true, Summary: "\u2003"}, {Summary: "interrupted"}, {Completed: true, Summary: "findings", Findings: []Finding{{Title: "issue"}}}} {
		if r.Clean() {
			t.Fatal("unclean review authorized")
		}
	}
	if !(Review{Completed: true, Summary: "Reviewed the full diff"}).Clean() {
		t.Fatal("clean review refused")
	}
}
func TestOperatingModesAndOwnedAttemptSnapshots(t *testing.T) {
	for _, paused := range []bool{true, false} {
		var c Control
		mode := "continuous"
		if paused {
			mode = "paused"
		}
		if err := json.Unmarshal([]byte(fmt.Sprintf(`{"paused":%t,"mode":%q,"cycle_number":4,"next_cycle_at":5,"idle_streak":0,"context_fingerprint":""}`, paused, mode)), &c); err != nil {
			t.Fatal(err)
		}
		if c.Paused != paused || c.Mode == OperatingModeRunOnce {
			t.Fatal(c)
		}
	}
	c := DefaultControl()
	c.Batch = &RunBatch{ID: "batch", Phase: BatchPhasePlanning}
	c.SetMode(OperatingModeRunOnce)
	if c.Paused || c.Batch == nil {
		t.Fatal(c)
	}
	c.SetMode(OperatingModeContinuous)
	if c.Batch != nil || c.Paused {
		t.Fatal(c)
	}
	task := Task{Config: config.Default(), Proposal: Proposal{Evidence: []string{"saved"}}, Sessions: []Session{NewSession("native", "executor", config.NewRoute("saved", "low"))}, Reviews: []ReviewRound{{Result: Review{Findings: []Finding{{Title: "saved"}}}}}, SupersededBy: []string{"next"}}
	original := task.Clone()
	policy := AttemptPolicyFromConfig(task.Config)
	policy.TaskTimeoutSeconds = 123
	task.AttemptPolicy = &policy
	execution := task.ExecutionConfig()
	if execution.TaskTimeoutSeconds != 123 || task.Config.TaskTimeoutSeconds == 123 {
		t.Fatal("attempt policy mutates historical configuration")
	}
	execution.Roles["discovery"] = config.NewRoute("changed", "low")
	snapshot := task.Clone()
	snapshot.Proposal.Evidence[0] = "changed"
	snapshot.Sessions[0].Route.Model = "changed"
	snapshot.Reviews[0].Result.Findings[0].Title = "changed"
	snapshot.SupersededBy[0] = "changed"
	snapshot.AttemptPolicy.MaxRetries = 9
	if !reflect.DeepEqual(task.Config, original.Config) || task.Proposal.Evidence[0] != "saved" || task.Sessions[0].Route.Model != "saved" || task.Reviews[0].Result.Findings[0].Title != "saved" || task.SupersededBy[0] != "next" || task.AttemptPolicy.MaxRetries == 9 {
		t.Fatal("aliased task snapshot")
	}
	cycle := Cycle{Assessments: []any{map[string]any{"evidence": []any{"saved"}}}}
	clone := cycle.Clone()
	clone.Assessments[0].(map[string]any)["evidence"].([]any)[0] = "changed"
	if cycle.Assessments[0].(map[string]any)["evidence"].([]any)[0] != "saved" {
		t.Fatal("aliased opaque evidence")
	}
}
func TestSessionTransitionsPreserveEvidence(t *testing.T) {
	sessions := []Session{NewSession("a", "executor", config.NewRoute("m", "low")), {Status: SessionCompleted, Summary: "done"}, {Status: SessionRunning, Summary: "recorded"}}
	FailRunning(sessions, "terminal failure")
	if sessions[0].Status != SessionFailed || sessions[0].Summary != "terminal failure" || sessions[1].Summary != "done" || sessions[2].Summary != "recorded" {
		t.Fatal(sessions)
	}
	sessions[0].MarkRunning()
	InterruptRunning(sessions)
	if sessions[0].Status != SessionInterrupted || CompletedSessions(sessions) != 1 {
		t.Fatal(sessions)
	}
}
func TestUTCIdentitiesAndTypedErrors(t *testing.T) {
	at := time.Date(2026, 1, 1, 23, 45, 0, 0, time.FixedZone("west", -3600))
	if UTCDay(at) != "2026-01-02" {
		t.Fatal("non-UTC day")
	}
	if a, b := ID(), ID(); a == b || len(a) != 36 || a[14] != '4' {
		t.Fatal("UUID v4 identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, Now()); err != nil {
		t.Fatal(err)
	}
	if BlockedReasonFromError(fmt.Errorf("outer: %w", BlockedReasonTimeout)) != BlockedReasonTimeout {
		t.Fatal("typed cause lost")
	}
	p := PlanningCapacity{Status: PlanningCapacityStatusDailyExhausted, Required: 13}
	if p.EnsureAvailable() == nil || BlockedReasonFromError(p.EnsureAvailable()) != BlockedReasonBudgetExhausted {
		t.Fatal("capacity classification")
	}
}

func TestSameWorkComparisons(t *testing.T) {
	left := Proposal{Title: " Concrete improvement ", Target: "main", ProblemKey: "stable-key"}
	for _, tc := range []struct {
		title, key, target string
		want               bool
	}{
		{"concrete IMPROVEMENT", "different", "main", true},
		{"Reworded", "STABLE-KEY", "main", true},
		{"Reworded", "different", "main", false},
		{"Concrete improvement", "stable-key", "other", false},
	} {
		right := Proposal{Title: tc.title, ProblemKey: tc.key, Target: tc.target}
		if got := left.SameWork(right); got != tc.want {
			t.Fatalf("SameWork(%+v) = %t", right, got)
		}
	}
}

func TestTimestampFractionPrecision(t *testing.T) {
	for _, c := range []struct {
		n        int
		fraction string
	}{{0, ""}, {120000000, ".120"}, {123400000, ".123400"}, {123456700, ".123456700"}} {
		got := timestamp(time.Date(2026, 1, 1, 0, 0, 0, c.n, time.UTC))
		want := "2026-01-01T00:00:00" + c.fraction + "+00:00"
		if got != want {
			t.Errorf("%s != %s", got, want)
		}
	}
}
