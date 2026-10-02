package store_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/tyk-swe/octomus-agent/internal/testutil"
)

func TestCleanupCandidatesCircularOrderAcrossEligibilityGaps(t *testing.T) {
	t.Parallel()
	testutil.SkipVolumeUnderRace(t)
	const (
		old    = "2020-01-01T00:00:00Z"
		cutoff = "2021-01-01T00:00:00Z"
		recent = "2022-01-01T00:00:00Z"
	)
	for _, tc := range []struct {
		count  int
		stride int
	}{
		{0, 1}, {1, 1}, {99, 1}, {100, 1}, {101, 1}, {240, 1},
		{240, 0}, {240, 17}, {2400, 17},
	} {
		t.Run(fmt.Sprintf("records=%d/eligible_every=%d", tc.count, tc.stride), func(t *testing.T) {
			path := statePath(t)
			s := open(t, path)
			writer := raw(t, path)
			tx, err := writer.Begin()
			must(t, err)
			defer tx.Rollback()
			stmt, err := tx.Prepare("INSERT INTO records VALUES (?1,?2,?3)")
			must(t, err)
			for i := range tc.count {
				// Interleave kinds with the same IDs, leaving gaps in each kind's
				// sequence. IDs deliberately do not sort in insertion order.
				for _, kind := range []string{"task", "cycle"} {
					terminal, clock := "published", "updated_at"
					if kind == "cycle" {
						terminal, clock = "completed", "completed_at"
					}
					data := map[string]any{"status": terminal, clock: old, "started_at": old}
					lifecycle := map[string]any{}
					data["lifecycle"] = lifecycle
					if tc.stride > 0 && i%tc.stride == 0 {
						switch (i / tc.stride) % 4 {
						case 1:
							data["status"], data[clock], lifecycle["archived_at"] = "failed", recent, old
						case 2:
							if kind == "cycle" {
								data["status"], data[clock] = "idle", nil // Legacy start-time fallback.
							}
						case 3:
							data[clock] = "2020-01-01T02:00:00+02:00"
						}
					} else {
						switch i % 8 {
						case 0:
							data["status"] = "failed"
						case 1:
							data[clock] = recent
						case 2:
							data[clock] = cutoff
						case 3:
							lifecycle["archived_at"] = recent
						case 4:
							lifecycle["discarded_at"] = old
						case 5:
							data[clock] = "invalid-date"
						case 6:
							lifecycle["archived_at"] = "invalid-date"
						case 7:
							lifecycle["archived_at"], lifecycle["discarded_at"] = old, old
						}
					}
					encoded, err := json.Marshal(data)
					must(t, err)
					_, err = stmt.Exec(kind, fmt.Sprintf("row-%d", i), string(encoded))
					must(t, err)
				}
			}
			must(t, stmt.Close())
			must(t, tx.Commit())

			check := func(deleted int) {
				t.Helper()
				for _, kind := range []string{"task", "cycle"} {
					for _, cursor := range []int{-1, 0, 1, 99, 100, 118, tc.count - 2, tc.count - 1, tc.count + 1} {
						after := ""
						if cursor >= 0 {
							after = fmt.Sprintf("row-%d", cursor)
						}
						start := cursor
						if cursor < 0 || cursor >= tc.count || cursor == deleted {
							start = -1
						}
						want := []string{}
						for offset := 1; offset <= tc.count && len(want) < 100; offset++ {
							i := (start + offset) % tc.count
							if i != deleted && tc.stride > 0 && i%tc.stride == 0 {
								want = append(want, fmt.Sprintf("row-%d", i))
							}
						}
						got, err := s.CleanupCandidates(kind, cutoff, after)
						must(t, err)
						if !slices.Equal(got, want) {
							t.Fatalf("%s after %q (deleted %d) = %v; want %v", kind, after, deleted, got, want)
						}
					}
				}
			}
			check(-1)
			if tc.count > 100 {
				// A deleted cursor restarts at the beginning; other cursors still
				// cross the missing sequence without skipping an eligible record.
				exec(t, writer, "DELETE FROM records WHERE id='row-100'")
				exec(t, writer, "DELETE FROM record_meta WHERE id='row-100'")
				check(100)
			}
		})
	}
}
