package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

func TestFixtureAdmissionTotalsAcrossUTCMidnight(t *testing.T) {
	t.Parallel()
	state := testStore(t)
	cfg := config.Default()
	cfg.MaxSessionsPerDay = 5
	if err := state.Put("settings", "config", cfg); err != nil {
		t.Fatal(err)
	}
	assertAdmissions(t, state, 0, "empty fixture")

	before := time.Date(2026, 3, 1, 23, 59, 59, 0, time.UTC)
	after := before.Add(time.Second)
	reserve := func(at time.Time) error {
		admission := store.NewAdmission("midnight-fixture", nil, "executor", config.NewRoute("fixture", "medium"))
		admission.At = at.Format(time.RFC3339)
		return state.ReserveSession(0, admission)
	}
	if err := reserve(before); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := reserve(after); err != nil {
			t.Fatal(err)
		}
	}
	// A scenario's total spans both days, even though the live budget correctly
	// resets at midnight. Use fixed dates so the regression never changes clocks.
	assertAdmissions(t, state, 6, "one prior-day and five next-day admissions")
	for _, test := range []struct {
		at   time.Time
		used uint64
	}{{before, 1}, {after, 5}} {
		capacity, err := state.PlanningCapacityAt(test.at)
		if err != nil || capacity.Day != model.UTCDay(test.at) || capacity.Used != test.used || capacity.Remaining != cfg.MaxSessionsPerDay-test.used {
			t.Fatalf("daily capacity at %s = %+v, %v; want %d used", test.at, capacity, err, test.used)
		}
	}
	if err := reserve(after); !errors.Is(err, model.BlockedReasonBudgetExhausted) {
		t.Fatalf("sixth admission on one day = %v; want budget exhaustion", err)
	}
	assertAdmissions(t, state, 6, "rejected admission changes neither ledger nor counters")
}
