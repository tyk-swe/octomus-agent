package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
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

// enumRoundTrip checks that every wire name encodes, decodes and prints as the
// value at its index, and that a value past the names cannot be saved.
func enumRoundTrip[T interface {
	~uint8
	String() string
}](t *testing.T, names []string) {
	t.Helper()
	for i, name := range names {
		value := T(i)
		data, err := json.Marshal(value)
		if err != nil || string(data) != strconv.Quote(name) {
			t.Fatalf("json.Marshal(%T(%d)) = %s, %v; want %q", value, i, data, err, name)
		}
		var decoded T
		if err := json.Unmarshal(data, &decoded); err != nil || decoded != value {
			t.Fatalf("json.Unmarshal(%s) = %d, %v; want %d", data, decoded, err, i)
		}
		if value.String() != name {
			t.Fatalf("%T(%d).String() = %q; want %q", value, i, value.String(), name)
		}
	}
	outside := T(len(names))
	if data, err := json.Marshal(outside); err == nil {
		t.Fatalf("json.Marshal(%T(%d)) = %s; want an error", outside, len(names), data)
	}
	if outside.String() != "" {
		t.Fatalf("%T(%d).String() = %q; want empty", outside, len(names), outside.String())
	}
	decoded := T(1)
	if err := json.Unmarshal([]byte(`"not-a-value"`), &decoded); err == nil || decoded != 1 {
		t.Fatalf("unknown name = %d, %v; want an error and no change", decoded, err)
	}
}

func TestEveryEnumRoundTripsItsWireNames(t *testing.T) {
	enumRoundTrip[Status](t, []string{"queued", "executing", "reviewing", "repairing", "verifying", "publishing", "published", "blocked", "failed", "cancelled"})
	enumRoundTrip[BlockedReason](t, []string{"budget_exhausted", "storage_limit", "stale_base", "remote_conflict", "publication_uncertain", "runner_unavailable", "invalid_review", "verification_failed", "dependency_blocked", "invalid_plan", "workspace_invalid", "retry_limit", "timeout", "unknown"})
	enumRoundTrip[PlanningCapacityStatus](t, []string{"ready", "daily_exhausted", "limit_too_low"})
	enumRoundTrip[BaselineStatus](t, []string{"running", "passed", "failed", "cancelled", "timed_out", "interrupted"})
	enumRoundTrip[CycleMode](t, []string{"execution", "audit"})
	enumRoundTrip[OperatingMode](t, []string{"paused", "run_once", "continuous"})
	enumRoundTrip[BatchPhase](t, []string{"draining", "planning", "executing"})
}

// A reason added without guidance would otherwise read as unclassified.
func TestEveryBlockedReasonHasItsOwnGuidance(t *testing.T) {
	if len(blockedReasonMessages) != len(blockedReasonNames) {
		t.Fatalf("%d blocked reason messages for %d names", len(blockedReasonMessages), len(blockedReasonNames))
	}
	seen := map[string]BlockedReason{}
	for i := range blockedReasonNames {
		reason := BlockedReason(i)
		message := reason.Error()
		if message == "" || message != blockedReasonMessages[i] {
			t.Fatalf("%s.Error() = %q", reason, message)
		}
		if other, ok := seen[message]; ok {
			t.Fatalf("%s and %s share guidance %q", other, reason, message)
		}
		seen[message] = reason
	}
	if got := BlockedReason(200).Error(); got != "Unclassified task failure; inspect the recorded diagnostics" {
		t.Fatalf("out-of-range reason = %q", got)
	}
}

// A planning pass runs grounding, every discovery agent, one proposal review per
// reviewer slot and consolidation; admission must fund all of them up front.
func TestPlanningAdmissionsMatchPlanningRoles(t *testing.T) {
	c := config.Default()
	for _, agents := range []uint64{8, 9, 10} {
		c.DiscoveryAgents = agents
		want := 1 + agents + uint64(len(ReviewerSlots())) + 1
		if got := c.PlanningAdmissionsRequired(); got != want {
			t.Fatalf("PlanningAdmissionsRequired() with %d agents = %d; want %d", agents, got, want)
		}
	}
}
