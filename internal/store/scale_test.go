package store_test

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

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
