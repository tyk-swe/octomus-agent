// Durable record behaviour: record IDs, review cleanliness, enum wire names, task actions and version-7 task compatibility.

package model

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/config"
)

// Saved records and workspace paths carry these IDs, so the lowercase version-4 form must not drift.
func TestIDIsLowercaseUUIDv4(t *testing.T) {
	form := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if first, second := ID(), ID(); !form.MatchString(first) || !form.MatchString(second) || first == second {
		t.Fatalf("ID() = %q, %q; want two distinct lowercase version-4 UUIDs", first, second)
	}
}

func TestReviewClean(t *testing.T) {
	for _, r := range []Review{{Completed: true}, {Completed: true, Summary: "\u2003"}, {Summary: "interrupted"}, {Completed: true, Summary: "findings", Findings: []Finding{{Title: "issue"}}}} {
		if r.Clean() {
			t.Fatal("unclean review authorized")
		}
	}
	if !(Review{Completed: true, Summary: "Reviewed the full diff"}).Clean() {
		t.Fatal("clean review refused")
	}
}

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

func TestEnumWireNames(t *testing.T) {
	enumRoundTrip[Status](t, []string{"queued", "executing", "reviewing", "repairing", "verifying", "publishing", "published", "blocked", "failed", "cancelled"})
	enumRoundTrip[BlockedReason](t, []string{"budget_exhausted", "storage_limit", "stale_base", "remote_conflict", "publication_uncertain", "runner_unavailable", "invalid_review", "verification_failed", "dependency_blocked", "invalid_plan", "workspace_invalid", "retry_limit", "timeout", "unknown"})
	enumRoundTrip[CapacityStatus](t, []string{"ready", "daily_exhausted", "limit_too_low"})
	enumRoundTrip[BaselineStatus](t, []string{"running", "passed", "failed", "cancelled", "timed_out", "interrupted"})
	enumRoundTrip[CycleStatus](t, []string{"running", "completed", "idle", "failed", "interrupted"})
	enumRoundTrip[SessionStatus](t, []string{"running", "completed", "failed", "interrupted"})
	enumRoundTrip[CycleMode](t, []string{"execution", "audit"})
	enumRoundTrip[OperatingMode](t, []string{"paused", "run_once", "continuous"})
	enumRoundTrip[BatchPhase](t, []string{"draining", "planning", "executing", "merging"})
	enumRoundTrip[AutoMergeStatus](t, []string{"waiting", "manual", "merging", "uncertain", "merged", "closed"})
}

func TestTaskAllowedActions(t *testing.T) {
	at, commit := "2026-01-01T00:00:00+00:00", "abc123"
	reason := func(r BlockedReason) *BlockedReason { return &r }
	for _, tc := range []struct {
		name string
		task Task
		want []string
	}{
		{"archived and discarded", Task{Status: StatusFailed, Lifecycle: WorkspaceLifecycle{ArchivedAt: &at, DiscardedAt: &at}}, []string{}},
		{"archived", Task{Status: StatusPublished, Lifecycle: WorkspaceLifecycle{ArchivedAt: &at}}, []string{"discard"}},
		{"archived failed with output", Task{Status: StatusFailed, OutputCommit: &commit, Lifecycle: WorkspaceLifecycle{ArchivedAt: &at}}, []string{"discard"}},
		{"published", Task{Status: StatusPublished, OutputCommit: &commit}, []string{"archive"}},
		{"cancelled", Task{Status: StatusCancelled}, []string{"archive", "supersede"}},
		{"cancelled with output", Task{Status: StatusCancelled, OutputCommit: &commit}, []string{"archive"}},
		{"cancelled rediscovery requested", Task{Status: StatusCancelled, RediscoveryRequested: true}, []string{"archive"}},
		{"cancelled superseded", Task{Status: StatusCancelled, SupersededBy: []string{"next"}}, []string{"archive"}},
		{"cancelled discarded", Task{Status: StatusCancelled, Lifecycle: WorkspaceLifecycle{DiscardedAt: &at}}, []string{"archive"}},
		{"discarded failed", Task{Status: StatusFailed, Lifecycle: WorkspaceLifecycle{DiscardedAt: &at}}, []string{}},
		{"publishing", Task{Status: StatusPublishing}, []string{}},
		{"publishing with output", Task{Status: StatusPublishing, OutputCommit: &commit}, []string{}},
		{"blocked with output", Task{Status: StatusBlocked, OutputCommit: &commit, BlockedReason: reason(BlockedStaleBase)}, []string{"archive", "supersede"}},
		{"failed with output", Task{Status: StatusFailed, OutputCommit: &commit, BlockedReason: reason(BlockedRemoteConflict)}, []string{"archive", "reconcile"}},
		{"failed without a reason", Task{Status: StatusFailed}, []string{"cancel", "archive", "retry"}},
		{"blocked without a reason", Task{Status: StatusBlocked}, []string{"cancel", "archive", "retry"}},
	} {
		if got := tc.task.AllowedActions(); got == nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: AllowedActions() = %#v; want %#v", tc.name, got, tc.want)
		}
	}

	for _, status := range []Status{StatusQueued, StatusExecuting, StatusReviewing, StatusRepairing, StatusVerifying} {
		if got := (Task{Status: status}).AllowedActions(); !reflect.DeepEqual(got, []string{"cancel"}) {
			t.Errorf("%s: AllowedActions() = %#v; want [cancel]", status, got)
		}
		if got := (Task{Status: status, OutputCommit: &commit}).AllowedActions(); got == nil || len(got) != 0 {
			t.Errorf("%s with output: AllowedActions() = %#v; want []", status, got)
		}
	}

	recovery := map[BlockedReason][]string{
		BlockedStaleBase:            {"supersede"},
		BlockedInvalidPlan:          {"supersede"},
		BlockedWorkspaceInvalid:     {"supersede"},
		BlockedRemoteConflict:       {"reconcile"},
		BlockedPublicationUncertain: {"reconcile"},
		BlockedDependencyBlocked:    {"retry", "supersede"},
		BlockedRunnerUnavailable:    {"retry", "supersede"},
	}
	for _, status := range []Status{StatusFailed, StatusBlocked} {
		for i := range blockedReasonNames {
			r := BlockedReason(i)
			tail, ok := recovery[r]
			if !ok {
				tail = []string{"retry"}
			}
			want := append([]string{"cancel", "archive"}, tail...)
			if got := (Task{Status: status, BlockedReason: &r}).AllowedActions(); !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s: AllowedActions() = %#v; want %#v", status, r.String(), got, want)
			}
		}
	}
}

func TestRepairProgressCompat(t *testing.T) {
	rounds := uint64(1)
	task := Task{Config: config.Default(), Status: StatusBlocked, RepairRounds: &rounds, RepairProgress: &RepairProgress{Revision: "reviewed", NoProgressRounds: 1, AwaitingReview: true}}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "repair_progress")
	delete(fields, "repair_rounds")
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Task
	if err := json.Unmarshal(legacy, &loaded); err != nil || loaded.RepairProgress != nil || loaded.RepairRounds != nil {
		t.Fatalf("pre-checkpoint version-7 task failed to load: progress=%+v error=%v", loaded.RepairProgress, err)
	}
	clone := task.Clone()
	if clone.RepairRounds == nil || *clone.RepairRounds != rounds {
		t.Fatalf("completed repair count did not survive serialization: %v", clone.RepairRounds)
	}
	*clone.RepairRounds++
	if *task.RepairRounds != 1 {
		t.Fatal("task clones share mutable repair counts")
	}
	if clone.RepairProgress == nil || *clone.RepairProgress != *task.RepairProgress {
		t.Fatalf("progress did not survive task serialization: %+v", clone.RepairProgress)
	}
	clone.RepairProgress.NoProgressRounds++
	if task.RepairProgress.NoProgressRounds != 1 {
		t.Fatal("task clones share mutable repair progress")
	}
}
