package store_test

// OCTOMUS_SCALE_TEST=1 enables the explicit scale checks:
//   OCTOMUS_SCALE_TEST=1 go test ./internal/store -run 'Scale|Bounded|Duplicate' -v -count=1
//
// TotalAlloc measures Go heap allocation around each query. The SQLite page
// cache lives outside the Go heap. Bounds include slack for GC work while still
// catching full-evidence materialization.

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
)

// heapAllocated returns the cumulative Go-heap bytes allocated so far.
func heapAllocated() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

func TestBoundedHistoryScale(t *testing.T) {
	if os.Getenv("OCTOMUS_SCALE_TEST") == "" {
		t.Skip("OCTOMUS_SCALE_TEST is not set: explicit 100,000-record allocation and latency measurement")
	}
	path := statePath(t)
	s := open(t, path)
	writer := raw(t, path)
	dataBytes, err := json.Marshal(map[string]any{
		"id": "fixture", "status": "published",
		"proposal":   map[string]any{"title": "Historical task", "target": "main", "prompt": strings.Repeat("x", 4096)},
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
	})
	must(t, err)
	data := string(dataBytes)
	inserted := 0
	var baseline uint64
	for _, count := range []int64{1000, 10000, 100000} {
		tx, err := writer.Begin()
		must(t, err)
		stmt, err := tx.Prepare("INSERT INTO records VALUES ('task',?1,?2)")
		must(t, err)
		for i := inserted; i < int(count); i++ {
			if _, err := stmt.Exec(fmt.Sprintf("historical-%d", i), data); err != nil {
				t.Fatal(err)
			}
		}
		must(t, stmt.Close())
		must(t, tx.Commit())
		inserted = int(count)
		elapsed := make([]time.Duration, 0, 20)
		var peak uint64
		var stateBytes uint64
		for range 20 {
			startAlloc := heapAllocated()
			start := time.Now()
			snapshot, err := s.Dashboard()
			must(t, err)
			elapsed = append(elapsed, time.Since(start))
			if len(snapshot.Tasks) != 300 {
				t.Fatalf("dashboard tasks = %d, want 300", len(snapshot.Tasks))
			}
			if snapshot.Counts["published"] != count {
				t.Fatalf("published count = %d, want %d", snapshot.Counts["published"], count)
			}
			serialized, err := json.Marshal(snapshot)
			must(t, err)
			stateBytes = uint64(len(serialized))
			if allocated := heapAllocated() - startAlloc; allocated > peak {
				peak = allocated
			}
		}
		slices.Sort(elapsed)
		if baseline == 0 {
			baseline = peak
		}
		// Allow 2x the 1,000-task baseline plus GC slack. A full-history
		// materialization would exceed this bound by orders of magnitude.
		if peak > baseline*2+1024*1024 {
			t.Fatalf("Go allocations grew with full history: baseline %d peak %d", baseline, peak)
		}
		if stateBytes >= 1024*1024 {
			t.Fatalf("serialized snapshot = %d bytes", stateBytes)
		}
		t.Logf("history=%d state_bytes=%d p50_us=%d p95_us=%d peak_bytes=%d",
			count, stateBytes, elapsed[10].Microseconds(), elapsed[19].Microseconds(), peak)
	}
}

func TestDuplicateHistoryScale(t *testing.T) {
	if os.Getenv("OCTOMUS_SCALE_TEST") == "" {
		t.Skip("OCTOMUS_SCALE_TEST is not set: explicit 2,000-task duplicate lookup with 64 KiB evidence per task")
	}
	path := statePath(t)
	s := open(t, path)
	// Build the fixture from the typed task so it follows the strict record
	// format as fields are added; a hand-written map fell behind and stopped
	// decoding, which disabled this gate.
	historical := task()
	historical.Status = model.StatusPublished
	historical.Branch = "tyk/history"
	historical.CreatedAt = "2026-01-01T00:00:00Z"
	historical.UpdatedAt = "2026-01-01T00:00:00Z"
	historical.Verification = []model.Verification{{
		Command: "fixture", Success: true, Output: strings.Repeat("x", 64*1024),
		Revision: "source", CreatedAt: "2026-01-01T00:00:00Z",
	}}
	proposals := make([]model.Proposal, 0, 20)
	for i := range 20 {
		p := historical.Proposal
		p.Title = fmt.Sprintf("New task %d", i)
		p.ProblemKey = fmt.Sprintf("new-key-%d", i)
		proposals = append(proposals, p)
	}
	writer := raw(t, path)
	tx, err := writer.Begin()
	must(t, err)
	stmt, err := tx.Prepare("INSERT INTO records VALUES ('task',?1,?2)")
	must(t, err)
	for i := range 2000 {
		historical.ID = fmt.Sprintf("historical-%d", i)
		historical.Proposal.Title = fmt.Sprintf("Historical task %d", i)
		historical.Proposal.ProblemKey = fmt.Sprintf("historical-key-%d", i)
		row, err := json.Marshal(historical)
		must(t, err)
		if i == 0 {
			// Keep the rows decodable so an implementation that accidentally
			// loads every task fails the allocation gate, not a decode.
			var probe model.Task
			must(t, json.Unmarshal(row, &probe))
		}
		if _, err := stmt.Exec(historical.ID, string(row)); err != nil {
			t.Fatal(err)
		}
	}
	must(t, stmt.Close())
	must(t, tx.Commit())
	startAlloc := heapAllocated()
	start := time.Now()
	matches, err := s.DuplicateTasks("FIXTURE/PROJECT", proposals)
	must(t, err)
	elapsed := time.Since(start)
	if len(matches) != 0 {
		t.Fatalf("duplicate lookup returned %d tasks", len(matches))
	}
	peak := heapAllocated() - startAlloc
	// Loading all 2,000 tasks' 64 KiB verification outputs would allocate at
	// least ~128 MiB; the covering-index lookup allocates under 1 MiB here
	// (one small string triple per scanned index row). 4 MiB keeps the gate
	// over 30x below the regression it exists to catch while absorbing driver
	// and GC noise.
	if peak >= 4*1024*1024 {
		t.Fatalf("Duplicate lookup allocated historical evidence: %d bytes", peak)
	}
	t.Logf("history=2000 evidence_bytes=65536 proposals=20 elapsed_us=%d peak_bytes=%d",
		elapsed.Microseconds(), peak)
}
