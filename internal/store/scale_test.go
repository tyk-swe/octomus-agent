package store_test

// Port of tests/history_scale.rs: explicit scale checks gated behind
// OCTOMUS_SCALE_TEST=1, matching the reference's #[ignore] opt-in.
//   OCTOMUS_SCALE_TEST=1 go test ./internal/store -run 'Scale|Bounded|Duplicate' -v -count=1
//
// The reference measures peak live bytes with a counting global allocator.
// Go cannot intercept allocations, so these tests bound the runtime.MemStats
// TotalAlloc delta around each measured call instead. Every Go-heap byte a
// query materializes is counted, and a bound on allocated bytes is strictly
// stronger than the reference's bound on live bytes: live growth inside the
// window can never exceed what was allocated. modernc.org/sqlite keeps its
// page cache in off-heap arena memory, so — exactly like the Rust allocator
// ignoring SQLite's C heap — these numbers cover the port's own allocations,
// which is where a full-evidence deserialization regression would show.
// Background GC work inside a measured window can add noise beyond the
// operation's own allocations, so the bounds carry the documented slack below.

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

// Port of tests/history_scale.rs bounded_history_scale.
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
		// Same bound shape as the reference: within 2x the 1,000-task baseline
		// plus slack. The slack is wider than the reference's 64 KiB because it
		// covers Go runtime allocations the counting Rust allocator never saw:
		// GC workbufs and mark state allocated when a collection lands inside a
		// measured call. The bound still fails by orders of magnitude if a query
		// scales with history — materializing 100,000 full records is ~500 MB.
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

// Port of tests/history_scale.rs duplicate_history_scale.
func TestDuplicateHistoryScale(t *testing.T) {
	if os.Getenv("OCTOMUS_SCALE_TEST") == "" {
		t.Skip("OCTOMUS_SCALE_TEST is not set: explicit 2,000-task duplicate lookup with 64 KiB evidence per task")
	}
	path := statePath(t)
	s := open(t, path)
	proposal := map[string]any{
		"id": "proposal", "title": "Historical task", "problem_key": "historical-key",
		"target": "main", "problem": "Missing behavior", "benefit": "Useful behavior",
		"scope": "one file", "evidence": []string{"README.md"}, "category": "features",
		"tier": "M", "dependencies": []string{}, "prompt": "Implement behavior",
		"decision": "accepted", "reason": "Grounded",
	}
	data := map[string]any{
		"id": "historical", "cycle_id": "cycle", "status": "published",
		"proposal":        proposal,
		"route":           map[string]any{"backend": "codex", "model": "fixture", "effort": "low"},
		"config":          map[string]any{"github_repo": "fixture/project"},
		"source_revision": "source", "comparison_base": "source", "default_revision": "source",
		"branch": "tyk/history", "workspace": "",
		"execution_session": nil, "repair_session": nil, "sessions": []any{}, "reviews": []any{},
		"verification": []map[string]any{{
			"command": "fixture", "success": true, "output": strings.Repeat("x", 64*1024),
			"revision": "source", "created_at": "2026-01-01T00:00:00Z",
		}},
		"output_commit": nil, "pr_number": nil, "pr_url": nil,
		"attempts": 0, "error": nil,
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z",
	}
	// Keep the probe valid for implementations that accidentally load all tasks.
	rawData, err := json.Marshal(data)
	must(t, err)
	var probe model.Task
	must(t, json.Unmarshal(rawData, &probe))
	proposalBytes, err := json.Marshal(proposal)
	must(t, err)
	proposals := make([]model.Proposal, 0, 20)
	for i := range 20 {
		var p model.Proposal
		must(t, json.Unmarshal(proposalBytes, &p))
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
		data["id"] = fmt.Sprintf("historical-%d", i)
		proposal["title"] = fmt.Sprintf("Historical task %d", i)
		proposal["problem_key"] = fmt.Sprintf("historical-key-%d", i)
		row, err := json.Marshal(data)
		must(t, err)
		if _, err := stmt.Exec(data["id"], string(row)); err != nil {
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
	// and GC noise the Rust counting allocator never saw.
	if peak >= 4*1024*1024 {
		t.Fatalf("Duplicate lookup allocated historical evidence: %d bytes", peak)
	}
	t.Logf("history=2000 evidence_bytes=65536 proposals=20 elapsed_us=%d peak_bytes=%d",
		elapsed.Microseconds(), peak)
}
